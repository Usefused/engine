/**
 * callClient is trusted host code -- it runs in the outer Node process, not
 * inside the vm sandbox, and is the *only* place in this package allowed to
 * use fetch/network primitives directly. sandbox.ts binds a wrapped version
 * of `call` into the sandbox's global scope; the raw fetch here is never
 * itself exposed to sandboxed script -- see sandbox.ts for why that
 * distinction is the whole point (design doc, "No I/O outside call()").
 *
 * This makes an HTTP request to the Engine's own /mcp/call endpoint
 * (internal/engine/sandbox/mcp_shared_runtime.go) rather than the vendor
 * directly. Credentials are never read or held here: the request carries
 * only (operationId, params) plus the session bearer used as a
 * lookup key with session authority, and the Go side resolves provider credentials server-side from the
 * session's validated token -- see sprint/lighter_mcp_runtime_design.md,
 * "Credentials never enter the process running the script."
 */

import { ModelRecovery, modelRecoveryFromUnknown } from "./sessionContract.js";

interface CallResponse {
  result?: unknown;
  error?: string;
  code?: string;
  recovery_action?: unknown;
  execute_request?: unknown;
  provider_execution?: unknown;
  automatic_replay?: unknown;
  auth_action?: unknown;
}

const bridgeRecoveries = new WeakMap<object, ModelRecovery>();
const bridgeAuthActions = new WeakMap<object, BridgeAuthAction>();

/** BridgeAuthAction is the validated browser handoff created by Engine for one failed provider call. */
export interface BridgeAuthAction {
  action: "connect" | "reconnect";
  url: string;
  elicitationId: string;
  expiresAt: string;
}

/** Returns only recovery metadata validated from an Engine bridge response for this exact host-owned error. */
export function bridgeRecoveryForError(error: unknown): ModelRecovery | undefined {
  // WeakMap identity prevents a script-thrown lookalike from claiming trusted bridge recovery.
  if (typeof error !== "object" || error === null) return undefined;
  return bridgeRecoveries.get(error);
}

/** Returns a browser handoff only when it was attached to this exact host-owned bridge error. */
export function bridgeAuthActionForError(error: unknown): BridgeAuthAction | undefined {
  // Object identity prevents sandbox-authored lookalikes from opening arbitrary URLs in the MCP client.
  if (typeof error !== "object" || error === null) return undefined;
  return bridgeAuthActions.get(error);
}

/** PhysicalCallOptions carries caller-owned execution controls separately from provider parameters. */
export interface PhysicalCallOptions {
  pagination?: {
    maxPages: number;
  };
}

export interface CallClientOptions {
  sessionId: string;
  enginePort: string;
}

/** Reads the non-secret identifiers this runtime needs from its own
 * environment. Called once at startup, not per call() invocation. */
export function callClientOptionsFromEnv(): CallClientOptions {
  const sessionId = process.env.FUSED_SESSION_ID;
  const enginePort = process.env.FUSED_ENGINE_PORT;
  if (!sessionId) {
    throw new Error("FUSED_SESSION_ID is required but not set");
  }
  if (!enginePort) {
    throw new Error("FUSED_ENGINE_PORT is required but not set");
  }
  return { sessionId, enginePort };
}

/**
 * remoteCall performs the single HTTP round trip a call() invocation needs.
 * Kept as a thin, single-purpose function (separation of concerns) so the
 * things that make call() safe -- the vm allowlist, the call-count cap, the
 * per-invocation timeout -- all live in sandbox.ts instead of being tangled
 * into the transport code.
 * The invocation signal aborts both fetch and body consumption; Engine observes
 * the disconnected request through its existing execution context.
 */
export async function remoteCall(
  options: CallClientOptions,
  operationId: string,
  params: Record<string, unknown>,
  signal?: AbortSignal,
  callOptions?: PhysicalCallOptions,
): Promise<unknown> {
  signal?.throwIfAborted();
  // Omitting pagination preserves the established envelope and automatic Engine pagination behavior.
  const pagination = callOptions?.pagination;
  let response: Response;
  try {
    response = await fetch(`http://localhost:${options.enginePort}/mcp/call`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${options.sessionId}`,
      },
      body: JSON.stringify({ operation_id: operationId, params, ...(pagination ? { pagination } : {}) }),
      signal,
    });
  } catch (error) {
    // Caller cancellation already has precise invocation semantics and must not be relabeled as bridge uncertainty.
    if (signal?.aborted) throw error;
    throw new Error("MCP_BRIDGE_UNAVAILABLE: Engine bridge did not respond; request outcome is unknown. Do not automatically retry provider mutations.");
  }

  let body: CallResponse;
  try {
    body = (await response.json()) as CallResponse;
  } catch (error) {
    // Abort during body consumption belongs to the invocation owner just like abort during fetch.
    if (signal?.aborted) throw error;
    // A proxy or truncated bridge response cannot prove whether provider dispatch occurred.
    throw new Error("MCP_BRIDGE_RESPONSE_INVALID: Engine bridge returned invalid JSON; request outcome is unknown. Do not automatically retry provider mutations.");
  }
  // JSON primitives and arrays cannot satisfy the bridge's reviewed result-or-error envelope.
  if (typeof body !== "object" || body === null || Array.isArray(body)) {
    throw new Error("MCP_BRIDGE_RESPONSE_INVALID: Engine bridge returned an invalid envelope; request outcome is unknown. Do not automatically retry provider mutations.");
  }
  // Provider and bridge errors preserve the existing reviewed response contract.
  if (!response.ok || body.error) {
    throw callResponseError(body, response.status);
  }
  return body.result;
}

/** Creates one host-owned error and attaches only closed bridge recovery fields by object identity. */
function callResponseError(body: CallResponse, status: number): Error {
  const failure = new Error(callErrorMessage(body, status));
  const recovery = modelRecoveryFromUnknown(body);
  // Invalid or absent metadata leaves outer execute recovery conservative instead of re-deriving Engine policy.
  if (recovery) bridgeRecoveries.set(failure, recovery);
  const authAction = bridgeAuthActionFromResponse(body, recovery);
  // Authentication URLs become actionable only when the complete Engine-owned recovery envelope agrees.
  if (authAction) bridgeAuthActions.set(failure, authAction);
  return failure;
}

/** Validates the complete Engine browser-auth contract before trusting its URL. */
function bridgeAuthActionFromResponse(body: CallResponse, recovery: ModelRecovery | undefined): BridgeAuthAction | undefined {
  // Ordinary provider failures and partial recovery envelopes cannot request browser navigation.
  if (!recovery || !validAuthRecovery(recovery)) return undefined;
  // Only a named object can carry the closed authentication action fields.
  if (typeof body.auth_action !== "object" || body.auth_action === null || Array.isArray(body.auth_action)) return undefined;
  const action = body.auth_action as Record<string, unknown>;
  const expectedAction = expectedAuthAction(body.code);
  // Stable error and action names must agree so one unreviewed code cannot inherit navigation authority.
  if (expectedAction === undefined || action.action !== expectedAction) return undefined;
  // Navigation values remain bounded even though the Engine bridge owns their source.
  if (typeof action.url !== "string" || action.url.length === 0 || action.url.length > 16_384 || !isAbsoluteWebURL(action.url)) return undefined;
  // Opaque IDs need only be non-empty and bounded; the client must not infer their structure.
  if (typeof action.elicitation_id !== "string" || action.elicitation_id.length === 0 || action.elicitation_id.length > 128) return undefined;
  // Absolute expiry is required so hosts can retire stale browser actions without inventing a lifetime.
  if (typeof action.expires_at !== "string" || action.expires_at.length > 64 || !isRFC3339Timestamp(action.expires_at)) return undefined;
  return { action: expectedAction, url: action.url, elicitationId: action.elicitation_id, expiresAt: action.expires_at };
}

/** Admits only the two reviewed browser-auth recovery pairs. */
function validAuthRecovery(recovery: ModelRecovery): boolean {
  // A proven pre-provider block allows the host to retry the tool after consent.
  if (recovery.provider_execution === "not_started") return recovery.recovery_action === "complete_authentication" && recovery.execute_request === "retry_after_auth";
  // An unknown provider outcome keeps the link usable but forbids automatic or complete-script replay.
  return recovery.provider_execution === "unknown" && recovery.recovery_action === "complete_authentication" && recovery.execute_request === "do_not_replay";
}

/** Maps only the two stable Engine connection codes to browser action names. */
function expectedAuthAction(code: unknown): BridgeAuthAction["action"] | undefined {
  // A missing connection begins first-time consent.
  if (code === "connection_required") return "connect";
  // An unusable persisted grant begins replacement consent.
  if (code === "reconnect_required") return "reconnect";
  return undefined;
}

/** Restricts client-opened handoffs to explicit HTTP(S) destinations. */
function isAbsoluteWebURL(value: string): boolean {
  try {
    const parsed = new URL(value);
    return parsed.protocol === "http:" || parsed.protocol === "https:";
  } catch {
    // Malformed values remain ordinary bridge errors without navigation authority.
    return false;
  }
}

/** Accepts the RFC 3339 shape emitted by Engine while rejecting locale-dependent date strings. */
function isRFC3339Timestamp(value: string): boolean {
  // A timezone is mandatory because browser-local interpretation would change expiry semantics.
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(value)) return false;
  return Number.isFinite(Date.parse(value));
}

/** Preserves an Engine-owned bridge code once without inferring codes from provider error text. */
function callErrorMessage(body: CallResponse, status: number): string {
  const message = body.error ?? `call() failed with HTTP ${status}`;
  // Uncoded errors retain their established text and cannot manufacture machine-readable recovery.
  if (typeof body.code !== "string" || !body.code) {
    return message;
  }
  const prefix = `${body.code}:`;
  // Physical errors already carry their stable prefix in prose, so adding it again would obscure correction guidance.
  return message.startsWith(prefix) ? message : `${prefix} ${message}`;
}
