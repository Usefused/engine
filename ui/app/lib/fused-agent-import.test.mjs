import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import ts from 'typescript';

const service = { id: 'service-1', name: 'Demo CRM', slug: 'demo-crm' };
const baseline = { service_id: service.id, service_version_id: 'version-1', revision: 7 };

/** Runs the production import adapter and target assertions with only API transport substituted. */
function fixture(overrides = {}, discoveryAPI = {}) {
  const calls = [];
  const api = { graphql: async (_query, variables) => { calls.push({ baseline: variables }); return { serviceWebhookEditor: baseline }; }, integrations: {
    planImport: async input => { calls.push(input); return { plan_id: 'plan-1', review_hash: 'review', service_id: service.id, target_version: 'v1', action: 'update_version', is_new_service: false, target_type: input.target_type, expected_target: input.expected_target, diff: { added: 1, changed: 0, removed: 2 }, ...overrides }; },
    applyImport: async () => { throw new Error('The agent must not apply an import'); },
    ...discoveryAPI,
  } };
  /** Loads real validation rather than duplicating its target-matching rules in the fixture. */
  function load(name) {
    // API and error modules are the external boundaries; domain assertions remain production code.
    if (name === './api') return { api };
    if (name.includes('authorization-error')) return { APIRequestError: class extends Error {} };
    if (name.includes('request-errors')) return { isInternalRequestError: () => false };
    const exports = {};
    const code = ts.transpileModule(readFileSync(new URL(`${name.replace(/\.ts$/, '')}.ts`, import.meta.url), 'utf8'), { compilerOptions: { module: ts.ModuleKind.CommonJS } }).outputText;
    new Function('require', 'exports', code)(load, exports);
    return exports;
  }
  return { prepare: load('./fused-agent-import').prepareAgentServiceImport, calls };
}

// Both source modes bind to the open service; a plan never implies persistence.
test('endpoint planning uses the shared API and exposes removals', async () => {
  const { prepare, calls } = fixture();
  const result = await prepare(service, 'v1', { target_type: 'endpoints', source_url: 'https://example.test/openapi.json' });
  assert.deepEqual(calls, [{ name: service.name, slug: service.slug, version: 'v1', target_type: 'endpoints', source_url: 'https://example.test/openapi.json' }]);
  assert.equal(result.plan.diff.removed, 2);
});

// A webhook source version is separate from the explicitly chosen existing destination.
test('webhook planning preserves exact revision and destination', async () => {
  const { prepare, calls } = fixture();
  await prepare(service, 'v1', { target_type: 'webhooks', source_content: '{"openapi":"3.1.0"}' });
  assert.deepEqual(calls[0], { baseline: { service_id: service.id, version: 'v1' } });
  assert.equal(calls[1].destination_version, 'v1');
  assert.equal(calls[1].version, undefined);
  assert.deepEqual(calls[1].expected_target, baseline);
});

// Older or mismatched receipts must never become an applicable review for another service.
test('rejects creation, wrong versions, surfaces and missing concurrency acknowledgement', async () => {
  for (const mismatch of [{ is_new_service: true }, { service_id: 'other' }, { target_version: 'v2' }, { action: 'create_version' }, { target_type: 'endpoints' }, { expected_target: undefined }]) {
    const { prepare } = fixture(mismatch);
    await assert.rejects(prepare(service, 'v1', { target_type: 'webhooks', source_content: '{}' }), /selected service|baseline revision/);
  }
});

// Invalid requests must be rejected before any remote work or owner-source read occurs.
test('rejects empty, ambiguous, oversized, credential-bearing or unsupported sources', async () => {
  const { prepare, calls } = fixture();
  for (const input of [{ target_type: 'endpoints' }, { target_type: 'webhooks', source_url: 'https://example.test/spec', source_content: '{}' }, { target_type: 'both', source_content: '{}' }, { target_type: 'endpoints', source_url: 'https://user:password@example.test/spec' }, { target_type: 'endpoints', source_url: 'file:///tmp/spec' }, { target_type: 'endpoints', source_content: 'x'.repeat(4 * 1024 * 1024 + 1) }]) {
    await assert.rejects(prepare(service, 'v1', input));
  }
  assert.equal(calls.length, 0);
});

// Website sources must traverse the shared discovery/review/plan protocol and retain the exact destination baseline.
test('webhook website discovery returns a reviewed plan without applying it', async () => {
  const calls = [];
  const receipt = { draft_id: 'draft', draft_revision: 1, review_hash: 'draft-hash' };
  const summary = { session_id: 'session', ...receipt, webhooks: [{ path: 'item.created' }] };
  const plan = { plan_id: 'plan', review_hash: 'plan-hash', target_type: 'webhooks', service_id: service.id, target_version: 'v1', action: 'update_version', is_new_service: false, expected_target: baseline };
  const { prepare } = fixture({}, {
    // The backend returns the stable review snapshot after its asynchronous extraction settles.
    startDiscovery: async input => { calls.push(input); return { session_id: 'session', revision: 6, state: 'awaiting_review', payload: { contract: receipt } }; },
    // Review details are bound to the same immutable receipt the plan request uses.
    getDiscoveryReviewSummary: async (_id, supplied) => { assert.deepEqual(supplied, receipt); return summary; },
    // Preparing a receipt is the only action this adapter is allowed to request.
    actOnDiscovery: async (_id, action) => { calls.push(action); return { state: 'plan_ready', payload: { plan, import_plan: plan } }; },
  });
  const result = await prepare(service, 'v1', { target_type: 'webhooks', source_mode: 'docs', source_url: 'https://example.test/events' });
  assert.equal(calls[0].source_mode, 'docs');
  assert.equal(calls[0].target_type, 'webhooks');
  assert.deepEqual(calls[0].expected_target, baseline);
  assert.equal(calls[1].action, 'request_plan');
  assert.equal(calls[1].expected_revision, 6);
  assert.equal(result.discovery.webhooks[0].path, 'item.created');
});

// A website failure cannot be silently retried as a specification or an empty replacement.
test('website extraction failure and invalid source modes fail closed', async () => {
  const { prepare } = fixture({}, {
    // This authoritative failure must remain visible to the caller.
    startDiscovery: async () => ({ session_id: 'session', revision: 5, state: 'error', payload: { diagnostics: [{ message: 'No supported webhook schemas found.' }] } }),
  });
  await assert.rejects(prepare(service, 'v1', { target_type: 'webhooks', source_mode: 'docs', source_url: 'https://example.test/events' }), /No supported/);
  await assert.rejects(prepare(service, 'v1', { target_type: 'endpoints', source_mode: 'docs', source_url: 'https://example.test/docs' }), /Webhook documentation/);
});

// The agent must receive the concrete crawl cause alongside the terminal summary, without requesting a plan.
test('website crawl diagnostics reach the agent unchanged', async () => {
  const messages = [
    'Starting page: The documentation server returned HTTP 403.',
    'No documentation pages could be fetched and admitted. Extraction did not run.',
  ];
  const { prepare } = fixture({}, {
    // Simulate the public Registry snapshot; no website or model is contacted by this regression.
    startDiscovery: async () => ({ session_id: 'session', revision: 3, state: 'error', payload: {
      diagnostics: messages.map(message => ({ message })),
    } }),
  });
  await assert.rejects(prepare(service, 'v1', { target_type: 'webhooks', source_mode: 'docs', source_url: 'https://example.test/docs' }), { message: messages.join(' ') });
});
