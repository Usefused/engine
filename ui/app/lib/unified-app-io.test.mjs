import assert from 'node:assert/strict';
import test from 'node:test';
import { unifiedAppContract } from './unified-app-io.ts';

// Mirrors Engine's filtered OpenAPI envelope while keeping authored schemas independently rooted.
function document(input, output) {
  return { paths: { '/v1/apps/{app_id}/executions': { post: {
    requestBody: { content: { 'application/json': { schema: { oneOf: [{ $ref: '#/components/schemas/Request' }] } } } },
    responses: { '200': { content: { 'application/json': { schema: { oneOf: [{ $ref: '#/components/schemas/Response' }] } } } } },
  } } }, components: { schemas: {
    Request: { properties: { operation: { const: 'execute' }, input } },
    Response: { properties: { executionId: { type: 'string' }, output } },
  } } };
}

// Authored constraints and references must survive extraction without receipt fields leaking into the output contract.
test('extracts authored schemas with nested definitions and required fields intact', () => {
  const input = { type: 'object', required: ['email'], properties: { email: { type: 'string', format: 'email' }, customer: { $ref: '#/$defs/Customer' } }, $defs: { Customer: { type: 'object' } } };
  const output = { type: 'object', properties: { checkoutUrl: { type: 'string', format: 'uri' } } };
  assert.deepEqual(unifiedAppContract(document(input, output)), { input, output });
  assert.deepEqual(unifiedAppContract(document({}, false)), { input: {}, output: false });
});

// Missing, ambiguous, or physical-operation contracts must produce an error rather than fabricated empty fields.
test('rejects unavailable and mismatched contracts', () => {
  assert.throws(() => unifiedAppContract({}), /unavailable/);
  assert.throws(() => unifiedAppContract(document({}, undefined)), /unavailable/);
  const physical = document({}, {});
  physical.components.schemas.Request.properties.operation.const = 'createCustomer';
  assert.throws(() => unifiedAppContract(physical), /unavailable/);
  const ambiguous = document({}, {});
  ambiguous.paths['/v1/apps/{app_id}/executions'].post.requestBody.content['application/json'].schema.oneOf.push({});
  assert.throws(() => unifiedAppContract(ambiguous), /unavailable/);
});

// Invalid reference envelopes never trigger remote requests or unbounded traversal.
test('rejects broken, external, and cyclic references', () => {
  for (const reference of ['#/missing', 'https://example.com/schema', '#/components/schemas/Request']) {
    const invalid = document({}, {});
    invalid.components.schemas.Request = { $ref: reference };
    assert.throws(() => unifiedAppContract(invalid), /unavailable|could not be resolved/);
  }
});
