import assert from 'node:assert/strict';
import test from 'node:test';
import { appTryOperations, appTryRequest } from './app-try-contract.ts';
import { compileInputContract, validateInputText } from './schedule-input-validation.ts';

/** Models immutable exported operation branches with a shared component reference. */
function document() {
  return { 'x-fused-app-id': 'version', paths: { '/v1/apps/{app_id}/executions': { post: { requestBody: { content: { 'application/json': { schema: { oneOf: [{ $ref: '#/components/schemas/Lookup' }, { $ref: '#/components/schemas/Execute' }] } } } } } } }, components: { schemas: {
    Lookup: { properties: { operation: { const: 'lookup' }, selector: {}, bucket_id: { enum: ['bucket'], 'x-fused-bucket-names': { bucket: 'Sandbox' } }, input: { type: 'object', required: ['count'], properties: { count: { $ref: '#/components/schemas/Count' }, enabled: { type: 'boolean' } }, additionalProperties: false } } },
    Count: { type: 'integer', minimum: 1 }, Execute: { properties: { operation: { const: 'execute' }, input: { type: 'object', properties: { person: { $ref: '#/$defs/Person' } }, $defs: { Person: { type: 'string' } } } } },
  } } };
}

// Shared component references and authored local definitions must validate in their own input roots.
test('exact schemas drive typed forms without losing references or constraints', () => {
  const operations = appTryOperations(document(), 'version', false);
  assert.deepEqual(operations.map(item => item.name), ['lookup', 'execute']);
  assert.deepEqual(operations[0].buckets, [{ id: 'bucket', name: 'Sandbox' }]);
  const validator = compileInputContract(operations[0].input);
  assert.equal(validator.warning, undefined);
  assert.equal(validateInputText('{"count":0}', validator).issues.length, 1);
  assert.equal(validateInputText('{"count":1,"enabled":false}', validator).issues.length, 0);
  const unified = appTryOperations(document(), 'version', true);
  assert.equal(unified.length, 1);
  assert.equal(validateInputText('{"person":2}', compileInputContract(unified[0].input)).issues.length, 1);
});

// Invalid contracts cannot become permissive empty forms or external schema fetches.
test('rejects foreign, duplicate, broken and remote operation contracts', () => {
  assert.throws(() => appTryOperations(document(), 'foreign', false), /version changed/);
  for (const ref of ['https://example.com/schema', '#/missing', '#/components/schemas/Lookup']) {
    const invalid = document(); invalid.components.schemas.Lookup = { $ref: ref };
    assert.throws(() => appTryOperations(invalid, 'version', false), /unavailable|resolved|identity/);
  }
  const duplicate = document(); duplicate.components.schemas.Execute.properties.operation.const = 'lookup';
  assert.throws(() => appTryOperations(duplicate, 'version', false), /ambiguous/);
});

// Routing controls never pollute input, broaden bucket scope, or follow a new Unified App version silently.
test('builds exact REST and Unified App requests with isolated routing', () => {
  const [physical, unified] = appTryOperations(document(), 'version', false);
  assert.deepEqual(appTryRequest(physical, { count: 1, enabled: false }, 'bucket', 'version', false, { end_user_ref: 'alice', resource_id: '' }), { operation: 'lookup', input: { count: 1, enabled: false }, bucket_id: 'bucket', selector: { end_user_ref: 'alice' } });
  assert.deepEqual(appTryRequest(unified, { person: 'Alice' }, '', 'version', true, {}), { operation: 'execute', input: { person: 'Alice' }, expected_app_id: 'version' });
  assert.throws(() => appTryRequest(physical, {}, 'unapproved', 'version', false, {}), /permitted/);
});
