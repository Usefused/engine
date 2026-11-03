import test from 'node:test';
import assert from 'node:assert/strict';
import * as grpc from '@grpc/grpc-js';
import { createHash } from 'node:crypto';
import { EngineTransport, EngineError, engineAddress, engineDefinition } from '../src/index.js';
const appId = '11111111-2222-3333-4444-555555555555';

/** Exercise actual protobuf messages against an isolated loopback Engine fixture. */
async function fixture(t: test.TestContext, execute: grpc.handleServerStreamingCall<any, any>) {
  let connects = 0, disconnects = 0;
  const service = (grpc.loadPackageDefinition(engineDefinition).engine as any).v1.EngineService.service;
  const server = new grpc.Server();
  server.addService(service, {
    // Count cache admission separately from operation dispatch.
    Connect(call: grpc.ServerUnaryCall<any, any>, done: grpc.sendUnaryData<any>) { connects++; assert.equal(call.metadata.get('x-app-id')[0], appId); done(null, {}); },
    // Track cache release so repeated closes can be checked.
    Disconnect(_call: grpc.ServerUnaryCall<any, any>, done: grpc.sendUnaryData<any>) { disconnects++; done(null, {}); }, Execute: execute,
    // Return only credential-free connection metadata.
    GetConnection(_call: grpc.ServerUnaryCall<any, any>, done: grpc.sendUnaryData<any>) { done(null, { found: true, connection: { id: 'connection-a', end_user_ref: 'user-a' } }); },
    // Exercise protobuf repeated resource metadata without provider secrets.
    ListConnectionResources(_call: grpc.ServerUnaryCall<any, any>, done: grpc.sendUnaryData<any>) { done(null, { resources: [{ id: 'resource-a', connection_id: 'connection-a' }] }); },
    // Verify connection sessions inherit the same immutable app identity.
    StartConnectSession(call: grpc.ServerUnaryCall<any, any>, done: grpc.sendUnaryData<any>) { assert.equal(call.request.created_by_app_id, appId); done(null, { authorize_url: 'https://provider.example/consent', connect_session_id: 'session', expires_at: 'soon' }); },
  });
  // An ephemeral loopback port keeps fixtures independent and surfaces bind failures.
  const port = await new Promise<number>((resolve, reject) => server.bindAsync('127.0.0.1:0', grpc.ServerCredentials.createInsecure(), (error, port) => error ? reject(error) : resolve(port)));
  const client = new EngineTransport({ grpcUrl: `http://127.0.0.1:${port}`, appId, token: 'test-execution-token', timeoutMs: 2000, maxResponseBytes: 512 });
  // Ensure fixture resources are released even when assertions fail.
  t.after(async () => { await client.close().catch(() => {}); server.forceShutdown(); });
  // Expose counters without granting tests access to private transport state.
  return { client, counts: () => ({ connects, disconnects }) };
}

// Concurrent calls must share admission while retaining their own request metadata.
test('gRPC preserves selectors and hashes, shares handshake, and disconnects once', async t => {
  const requests: any[] = [];
  // Drive the public client through real wire frames rather than mocking transport internals.
  const f = await fixture(t, call => {
    requests.push(call.request);
    assert.equal(call.metadata.get('x-app-id')[0], appId);
    assert.equal(call.metadata.get('x-api-key')[0], 'test-execution-token');
    call.write({ result: Buffer.from('{"ok":') }); call.write({ result: Buffer.from('true}'), status_code: 200 }); call.end();
  });
  const request = { endpoint: 'createCustomer', input: { name: 'Ada' }, selector: { endUserRef: 'user-a', authType: 'oauth', authName: 'main', resourceId: 'resource-a' }, pagination: { maxPages: 2 }, idempotencyKey: 'stable-call' };
  assert.deepEqual(await Promise.all([f.client.execute(request), f.client.execute(request)]), [{ ok: true }, { ok: true }]);
  assert.equal(f.counts().connects, 1);
  assert.equal(requests[0].endpoint_name, 'createCustomer');
  assert.equal(requests[0].request_body_hash, createHash('sha256').update(JSON.stringify(request.input)).digest('hex'));
  assert.equal(requests[0].idempotency_key, 'stable-call');
  assert.deepEqual(requests[0].credentials, { fused_end_user_ref: 'user-a', fused_auth_type: 'oauth', fused_auth_name: 'main', fused_resource_id: 'resource-a' });
  assert.equal(requests[0].pagination.max_pages, 2);
  assert.equal((await f.client.startConnectSession({ bucketId: 'bucket', serviceId: 'service', endUserRef: 'user-a' })).connectSessionId, 'session');
  assert.equal((await f.client.getConnection('connection-a'))?.end_user_ref, 'user-a');
  assert.equal((await f.client.listConnectionResources('connection-a'))[0]?.id, 'resource-a');
  await Promise.all([f.client.close(), f.client.close()]);
  assert.equal(f.counts().disconnects, 1);
  await assert.rejects(f.client.execute(request), /ENGINE_CLOSED/);
});

// An uncertain transport outcome must never repeat a provider side effect.
test('ambiguous transport failure is not replayed and diagnostics are private', async t => {
  let calls = 0;
  // Drive the public client through real wire frames rather than mocking transport internals.
  const f = await fixture(t, call => { calls++; call.destroy({ code: grpc.status.UNAVAILABLE, details: 'private provider credential' } as grpc.ServiceError); });
  // Error predicates must preserve status categories without exposing provider text.
  await assert.rejects(f.client.execute({ endpoint: 'charge', input: {} }), error => error instanceof EngineError && error.code === 'ENGINE_RPC_FAILED' && !error.message.includes('private'));
  assert.equal(calls, 1);
});

// Different malformed response classes must all fail at the client boundary.
test('response bounds and media/status contracts fail closed', async t => {
  for (const mode of ['oversize', 'invalid', 'missing-status', 'provider-error', 'sse']) await t.test(mode, async t => {
    // Drive the public client through real wire frames rather than mocking transport internals.
    const f = await fixture(t, call => {
      // Declared streaming media must be rejected before JSON assembly.
      if (mode === 'sse') { const metadata = new grpc.Metadata(); metadata.set('fused-response-media-family', 'sse'); call.sendMetadata(metadata); }
      // Each mode isolates one invalid media, size, status or error contract.
      call.write({ result: Buffer.from(mode === 'oversize' ? JSON.stringify('x'.repeat(700)) : mode === 'invalid' ? 'broken-json' : '{}'), ...(mode === 'missing-status' ? {} : { status_code: 200 }), ...(mode === 'provider-error' ? { error: 'private error' } : {}) }); call.end();
    });
    await assert.rejects(f.client.execute({ endpoint: 'read', input: {} }), EngineError);
  });
});

// Cancellation should terminate a live request rather than waiting for its deadline.
test('abort cancels an in-flight gRPC stream', async t => {
  const controller = new AbortController();
  // Drive the public client through real wire frames rather than mocking transport internals.
  const f = await fixture(t, _call => { setTimeout(() => controller.abort(), 10); });
  await assert.rejects(f.client.execute({ endpoint: 'slow', input: {}, signal: controller.signal }), /ENGINE_CANCELLED/);
});

// Explicit bucket routing must survive serialization and never fall back on invalid input.
test('optional bucket routing is explicit and rejects malformed selections before execution', async t => {
  const bucketId = '33333333-2222-4333-8444-555555555555';
  let calls = 0;
  // Drive the public client through real wire frames rather than mocking transport internals.
  const f = await fixture(t, call => {
    calls++;
    assert.equal(call.request.bucket_id, bucketId);
    call.write({ result: Buffer.from('{}'), status_code: 200 });
    call.end();
  });
  await f.client.execute({ endpoint: 'read', input: {}, bucketId });
  await assert.rejects(f.client.execute({ endpoint: 'read', input: {}, bucketId: 'support' }), /bucket identity/);
  assert.equal(calls, 1);
});

// Origin validation prevents tokens from travelling over insecure remote channels.
test('remote plaintext and credential-bearing gRPC origins are rejected', () => {
  // Every malformed or insecure origin must fail before opening a channel.
  for (const url of ['http://engine.example:50051', 'grpc://localhost:50051', 'https://token@engine.example', 'https://engine.example/path', 'https://engine.example?token=secret']) assert.throws(() => engineAddress(url));
  assert.deepEqual(engineAddress('https://engine.example'), { target: 'engine.example:443', secure: true });
  assert.deepEqual(engineAddress('grpc://127.0.0.1:1234'), { target: '127.0.0.1:1234', secure: false });
});


// A replacement Engine has independent cache state and requires fresh admission.
test('transport loss requires fresh cache admission but does not replay a prior operation', async t => {
  const service = (grpc.loadPackageDefinition(engineDefinition).engine as any).v1.EngineService.service;
  let connects = 0, calls = 0;
  /** Start one Engine lifetime; replacement uses the same endpoint and independent cache. */
  async function start(port: number) {
    const server = new grpc.Server();
    server.addService(service, {
      // Count cache admission separately from operation dispatch.
      Connect(_call: any, done: any) { connects++; done(null, {}); },
      // Track cache release so repeated closes can be checked.
      Disconnect(_call: any, done: any) { done(null, {}); },
      // Count side effects independently of admission after server replacement.
      Execute(call: any) { calls++; call.write({ result: Buffer.from('{}'), status_code: 200 }); call.end(); },
    });
    // Bind the replacement to the same endpoint or fail the fixture immediately.
    const bound = await new Promise<number>((resolve, reject) => server.bindAsync(`127.0.0.1:${port}`, grpc.ServerCredentials.createInsecure(), (error, port) => error ? reject(error) : resolve(port)));
    // Ensure fixture resources are released even when assertions fail.
    t.after(() => server.forceShutdown());
    return { server, port: bound };
  }
  const first = await start(0);
  const client = new EngineTransport({ grpcUrl: `http://127.0.0.1:${first.port}`, appId, token: 'test-token', timeoutMs: 3000 });
  // Ensure fixture resources are released even when assertions fail.
  t.after(() => client.close().catch(() => {}));
  await client.execute({ endpoint: 'read', input: {} });
  first.server.forceShutdown();
  // Allow the channel watcher to observe readiness loss before restarting Engine.
  await new Promise(resolve => setTimeout(resolve, 50));
  await start(first.port);
  await client.execute({ endpoint: 'read', input: {} });
  assert.equal(connects, 2);
  assert.equal(calls, 2);
});
