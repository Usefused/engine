import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import ts from 'typescript';
const id = '10000000-0000-4000-8000-000000000001';

/** Loads the production discovery boundaries with a recorded, authorized catalogue transport. */
function fixture() {
  const calls = [];
  const api = { async graphql(query, variables) {
    calls.push({ query, variables });
    // Separate responses expose accidental cross-version or unbounded search requests.
    if (query.includes('FusedAgentSearchServices')) return { searchServices: Array.from({ length: 25 }, (_, i) => ({ id, name: `Service ${i}` })) };
    if (query.includes('FusedAgentServiceVersions')) return { serviceVersions: [{ id: 'version-id', name: 'v1', status: 'active' }] };
    return { searchEndpoints: [{ name: 'GetCustomers', method: 'GET', path: '/customers' }] };
  } };
  /** Substitutes transport only; validation and query documents are production code. */
  const require = (name) => {
    // Contract reads use a captured delegate so tests can inspect exact pins.
    if (name === './fused-agent-contracts') return { readAgentContracts: (...args) => { calls.push({ contracts: args }); return args; } };
    return { api };
  };
  const exports = {};
  const code = ts.transpileModule(readFileSync(new URL('./fused-agent-discovery.ts', import.meta.url), 'utf8'), { compilerOptions: { module: ts.ModuleKind.CommonJS } }).outputText;
  new Function('require', 'exports', code)(require, exports);
  return { client: exports, calls };
}

// Only bounded metadata pages reach the model; searches never request auth configuration.
test('service discovery pages names and canonical identities', async () => {
  const { client, calls } = fixture();
  assert.equal((await client.searchAgentServices('stripe')).items.length, 20);
  assert.equal((await client.searchAgentServices('stripe', 20)).next_offset, null);
  assert.doesNotMatch(calls[0].query, /auth|secret|mutation/i);
  await assert.rejects(client.searchAgentServices('x'.repeat(513)), /512/);
  await assert.rejects(client.searchAgentServices('', -1), /offset/);
  assert.equal(calls.length, 2);
});

// Explicit version discovery prevents invalid pins from falling back to all versions.
test('endpoint search browses versions and refuses missing pins', async () => {
  const { client, calls } = fixture();
  assert.equal((await client.searchAgentOperations(id)).versions[0].name, 'v1');
  await assert.rejects(client.searchAgentOperations(id, 'missing', 'customer'), /not found/);
  assert.ok(calls.every(call => !call.query.includes('searchEndpoints')));
  const result = await client.searchAgentOperations(id, 'version-id', 'customer');
  assert.equal(result.items[0].name, 'GetCustomers');
  assert.deepEqual(calls.at(-1).variables, { id, version: 'v1', q: 'customer', offset: 0 });
  assert.match(calls.at(-1).query, /limit: 20/);
});

// Contract details remain tied to an observed service/version/operation without selecting it in an app.
test('discovered contract reads use the exact version', async () => {
  const { client, calls } = fixture();
  await client.readAgentServiceContract(id, 'v1', 'GetCustomers', '/request', 2);
  assert.deepEqual(calls.at(-1).contracts, [{ service: { service_id: id, service_version_id: 'version-id', version: 'v1', operations: ['GetCustomers'] } }, '/request', 2]);
  await assert.rejects(client.readAgentServiceContract(id, '', 'GetCustomers'), /exact/);
  await assert.rejects(client.searchAgentOperations('invented'), /service ID/);
});
