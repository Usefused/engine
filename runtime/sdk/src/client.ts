import * as grpc from '@grpc/grpc-js';
import * as loader from '@grpc/proto-loader';
import { fileURLToPath } from 'node:url';
import { createHash, randomUUID } from 'node:crypto';
import { isIP } from 'node:net';
import type { ExecutionRequest, Transport } from './types.js';

const definition = loader.loadSync(fileURLToPath(new URL('./engine.proto', import.meta.url)), { keepCase: true, longs: String, enums: String, defaults: false, oneofs: true });
export const engineDefinition = definition;
const EngineService = ((grpc.loadPackageDefinition(definition).engine as any).v1.EngineService) as grpc.ServiceClientConstructor;
export interface EngineOptions { grpcUrl: string; appId: string; token: string; timeoutMs?: number; maxResponseBytes?: number }
export class EngineError extends Error {
  /** Never include raw gRPC/provider messages, which can contain connection details. */
  constructor(public readonly code: string, public readonly status?: number) { super(code); this.name = 'EngineError'; }
}

/** Match Bridge's TLS policy while refusing credentials embedded in endpoint URLs. */
export function engineAddress(value: string): { target: string; secure: boolean } {
  // Reject ambiguous origins before URL normalization can alter them.
  if (value.trim() !== value || /[\x00-\x20\\?#]/.test(value)) throw new Error('Invalid Engine gRPC origin');
  const url = new URL(value);
  // Only a credential-free origin can identify an Engine transport endpoint.
  if (url.username || url.password || url.search || url.hash || !['', '/'].includes(url.pathname)) throw new Error('Invalid Engine gRPC origin');
  const host = url.hostname.replace(/^\[|\]$/g, '');
  const loopback = isIP(host) !== 0 && (host === '::1' || host.startsWith('127.'));
  const secure = url.protocol === 'https:' || url.protocol === 'grpcs:';
  // Plaintext is confined to explicit loopback IPs for local testing.
  if (!secure && !(['http:', 'grpc:'].includes(url.protocol) && loopback)) throw new Error('Engine gRPC requires TLS; plaintext is restricted to loopback');
  // Respect explicit ports before applying protocol defaults.
  return { target: url.host + (url.port ? '' : secure ? ':443' : ':50051'), secure };
}

/** One runtime owns one channel and handshake; every call remains scoped to its immutable Engine app. */
export class EngineTransport implements Transport {
  private readonly client: grpc.Client;
  private readonly options: EngineOptions;
  private connected?: Promise<void>;
  private closing?: Promise<void>;
  private closed = false;
  private handshakeComplete = false;
  // Validate scope and resource bounds before opening a channel.
  constructor(options: EngineOptions) {
    // Execution requires an immutable version UUID, never a mutable app name.
    if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(options.appId)) throw new Error('An immutable Engine app UUID is required');
    // Keep authentication metadata bounded and free of header control characters.
    if (!options.token || options.token.length > 8192 || /[^\x21-\x7e]/.test(options.token)) throw new Error('An Engine execution token is required');
    // Prevent unbounded or immediately expired RPC deadlines.
    if (options.timeoutMs !== undefined && (!Number.isSafeInteger(options.timeoutMs) || options.timeoutMs < 1 || options.timeoutMs > 600000)) throw new Error('Invalid Engine timeout');
    // Reject unsafe response allocation limits before configuring gRPC.
    if (options.maxResponseBytes !== undefined && (!Number.isSafeInteger(options.maxResponseBytes) || options.maxResponseBytes < 1 || options.maxResponseBytes > 64 * 1024 * 1024)) throw new Error('Invalid response limit');
    const address = engineAddress(options.grpcUrl);
    this.options = { ...options };
    // Use the validated TLS policy and disable retries to avoid execution replay.
    this.client = new EngineService(address.target, address.secure ? grpc.credentials.createSsl() : grpc.credentials.createInsecure(), { 'grpc.enable_retries': 0, 'grpc.max_receive_message_length': options.maxResponseBytes ?? 4 * 1024 * 1024 });
    this.watchChannel();
  }

  /** Re-establish Engine cache admission after transport loss, without replaying an Execute call. */
  private watchChannel(): void {
    // Shutdown must prevent new admission, RPCs and connectivity watches.
    if (this.closed) return;
    const channel = this.client.getChannel();
    const previous = channel.getConnectivityState(false);
    // Observe transport loss without resending any provider operation.
    channel.watchConnectivityState(previous, Date.now() + 30000, () => {
      // Shutdown must prevent new admission, RPCs and connectivity watches.
      if (this.closed) return;
      const next = channel.getConnectivityState(false);
      // Losing readiness invalidates prior admission into Engine cache.
      if (this.handshakeComplete && previous === grpc.connectivityState.READY && next !== grpc.connectivityState.READY) {
        this.connected = undefined;
        this.handshakeComplete = false;
      }
      this.watchChannel();
    });
  }

  /** Authenticate each RPC independently so a warm connection never replaces token checks. */
  private metadata(): grpc.Metadata {
    const metadata = new grpc.Metadata();
    metadata.set('x-app-id', this.options.appId);
    metadata.set('x-api-key', this.options.token);
    return metadata;
  }

  /** Deduplicate simultaneous startup handshakes and allow a failed handshake to be attempted again. */
  connect(): Promise<void> {
    // Shutdown must prevent new admission, RPCs and connectivity watches.
    if (this.closed) return Promise.reject(new EngineError('ENGINE_CLOSED'));
    // Share concurrent admission and permit a later retry only when admission fails.
    this.connected ??= this.unary('Connect', {}).then(() => { this.handshakeComplete = true; }).catch(error => { this.connected = undefined; throw error; });
    return this.connected;
  }

  /** Invoke a bounded unary RPC without exposing wire-level error text. */
  private unary(method: string, request: object): Promise<any> {
    // Translate asynchronous RPC completion into one bounded result.
    return new Promise((resolve, reject) => {
      // Reduce wire failures to status codes instead of exposing provider diagnostics.
      (this.client as any)[method](request, this.metadata(), { deadline: Date.now() + (this.options.timeoutMs ?? 30000) }, (error: grpc.ServiceError | null, response: unknown) => {
        // Only successful transport completion may expose the response.
        if (error) reject(new EngineError('ENGINE_RPC_FAILED', error.code));
        else resolve(response);
      });
    });
  }

  /** Execute once: transport reconnection never authorizes replay of provider side effects. */
  async execute(request: ExecutionRequest): Promise<unknown> {
    request.signal?.throwIfAborted();
    await this.connect();
    request.signal?.throwIfAborted();
    // Shutdown must prevent new admission, RPCs and connectivity watches.
    if (this.closed) throw new EngineError('ENGINE_CLOSED');
    // Bound operation identity before constructing the wire request.
    if (!request.endpoint || request.endpoint.length > 512) throw new Error('Invalid operation');
    const encoded = JSON.stringify(request.input);
    // Reject missing JSON and oversized inputs before buffering them.
    if (encoded === undefined || Buffer.byteLength(encoded) > 1024 * 1024) throw new Error('Invalid or oversized operation input');
    const params = Buffer.from(encoded);
    const credentials: Record<string, string> = {};
    for (const [key, wire] of Object.entries({ endUserRef: 'fused_end_user_ref', authType: 'fused_auth_type', authName: 'fused_auth_name', resourceId: 'fused_resource_id' })) {
      const value = request.selector?.[key as keyof NonNullable<ExecutionRequest['selector']>];
      // Absent selectors leave Engine routing defaults intact.
      if (value !== undefined) {
        // Selectors are bounded non-secret identifiers without control characters.
        if (!value || value.length > 512 || /[\x00-\x1f]/.test(value)) throw new Error('Invalid Fused selector');
        credentials[wire] = value;
      }
    }
    const pages = request.pagination?.maxPages;
    // Explicit selection must never degrade into the default bucket on malformed input.
    if (request.bucketId !== undefined && !/^[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}$/i.test(request.bucketId)) throw new Error('Invalid Fused bucket identity');
    // Pagination must fit a positive protobuf uint32.
    if (pages !== undefined && (!Number.isSafeInteger(pages) || pages < 1 || pages > 0xffffffff)) throw new Error('Invalid pagination bound');
    // An omitted key identifies this single execution with a fresh UUID.
    const idempotency = request.idempotencyKey ?? randomUUID();
    // Bound the idempotency identity and reject ambiguous whitespace.
    if (!idempotency || idempotency.length > 256 || /[\x00-\x20]/.test(idempotency)) throw new Error('Invalid idempotency key');
    // Translate asynchronous RPC completion into one bounded result.
    return new Promise((resolve, reject) => {
      // Omit unspecified bucket and pagination fields to preserve Engine defaults.
      const call = (this.client as any).Execute({ endpoint_name: request.endpoint, ...(request.bucketId === undefined ? {} : { bucket_id: request.bucketId }), params, credentials, idempotency_key: idempotency, request_body_hash: createHash('sha256').update(params).digest('hex'), ...(pages === undefined ? {} : { pagination: { max_pages: pages } }) }, this.metadata(), { deadline: Date.now() + (this.options.timeoutMs ?? 30000) }) as grpc.ClientReadableStream<any>;
      let settled = false, size = 0, status = 0;
      const chunks: Buffer[] = [];
      // Cleanup once even when cancellation races terminal transport events.
      const finish = (error?: Error, value?: unknown) => {
        // Cancellation and terminal events may race; only the first result wins.
        if (settled) return;
        settled = true;
        request.signal?.removeEventListener('abort', abort);
        // Stop data on failure while preserving single-settlement semantics.
        if (error) { call.cancel(); reject(error); } else resolve(value);
      };
      // Use a stable local error for caller-triggered cancellation.
      const abort = () => finish(new EngineError('ENGINE_CANCELLED'));
      request.signal?.addEventListener('abort', abort, { once: true });
      // Cover cancellation between the earlier check and listener registration.
      if (request.signal?.aborted) abort();
      // Inspect media before interpreting any buffered response as JSON.
      call.on('metadata', metadata => {
        const family = String(metadata.get('fused-response-media-family')[0] ?? 'json');
        // Unknown media is parsed later; declared non-JSON responses fail immediately.
        if (!['json', 'unknown'].includes(family)) finish(new EngineError('ENGINE_RESPONSE_NOT_JSON'));
        status = Number(metadata.get('fused-response-status')[0] ?? 0);
      });
      // Bound the aggregate body across all frames, not just individual messages.
      call.on('data', frame => {
        // Cancellation and terminal events may race; only the first result wins.
        if (settled) return;
        // Provider error text remains private even inside an otherwise successful RPC.
        if (frame.error) return finish(new EngineError('ENGINE_EXECUTION_FAILED'));
        // Frames may refine the status first supplied in metadata.
        if (frame.status_code) status = frame.status_code;
        // Status-only frames should not allocate response buffers.
        if (frame.result?.length) {
          size += frame.result.length;
          // Cancel before an oversized aggregate response consumes further memory.
          if (size > (this.options.maxResponseBytes ?? 4 * 1024 * 1024)) return finish(new EngineError('ENGINE_RESPONSE_TOO_LARGE'));
          chunks.push(Buffer.from(frame.result));
        }
      });
      // Expose a stable error code without leaking remote diagnostics.
      call.on('error', (error: grpc.ServiceError) => finish(new EngineError('ENGINE_RPC_FAILED', error.code)));
      // Return only a complete successful JSON response.
      call.on('end', () => {
        // Missing or unsuccessful statuses cannot become successful empty results.
        if (status < 200 || status >= 300) return finish(new EngineError('ENGINE_EXECUTION_FAILED', status));
        // Successful empty bodies map to null; malformed JSON fails with a stable error.
        try { finish(undefined, chunks.length ? JSON.parse(Buffer.concat(chunks).toString('utf8')) : null); }
        catch { finish(new EngineError('ENGINE_RESPONSE_INVALID')); }
      });
    });
  }

  /** Start provider OAuth with Engine-owned token storage using the same execution identity. */
  async startConnectSession(options: { bucketId: string; serviceId: string; endUserRef: string; returnUrl?: string; scopes?: string[]; resourceInput?: Record<string, string> }) {
    await this.connect();
    // Shutdown must prevent new admission, RPCs and connectivity watches.
    if (this.closed) throw new EngineError('ENGINE_CLOSED');
    const result = await this.unary('StartConnectSession', { bucket_id: options.bucketId, service_id: options.serviceId, end_user_ref: options.endUserRef, return_url: options.returnUrl ?? '', created_by_app_id: this.options.appId, scopes: options.scopes ?? [], resource_input: options.resourceInput ?? {} });
    return { authorizeUrl: result.authorize_url as string, expiresAt: result.expires_at as string, connectSessionId: result.connect_session_id as string };
  }

  /** Read credential-free connection metadata from Engine under this app's bucket scope. */
  async getConnection(connectionId: string): Promise<Record<string, unknown> | null> {
    await this.connect();
    // Shutdown must prevent new admission, RPCs and connectivity watches.
    if (this.closed) throw new EngineError('ENGINE_CLOSED');
    const result = await this.unary('GetConnection', { connection_id: connectionId });
    // Keep absent connections distinct from empty metadata objects.
    return result.found ? result.connection : null;
  }

  /** List opaque provider resource choices without returning tenant URLs or tokens. */
  async listConnectionResources(connectionId: string): Promise<ReadonlyArray<Record<string, unknown>>> {
    await this.connect();
    // Shutdown must prevent new admission, RPCs and connectivity watches.
    if (this.closed) throw new EngineError('ENGINE_CLOSED');
    const result = await this.unary('ListConnectionResources', { connection_id: connectionId });
    // Protobuf may omit empty repeated fields; callers always get a collection.
    return result.resources ?? [];
  }

  /** Release Engine cache references before closing the channel; repeated shutdown is harmless. */
  close(): Promise<void> {
    // Share one shutdown promise so duplicate closes cannot release admission twice.
    this.closing ??= (async () => {
      // Block new calls before waiting for prior admission to settle.
      this.closed = true;
      // Release attempted admission, then close the channel even if remote release fails.
      try { if (this.connected) { await this.connected; await this.unary('Disconnect', { app_id: this.options.appId }); } }
      finally { this.client.close(); }
    })();
    return this.closing;
  }
}
