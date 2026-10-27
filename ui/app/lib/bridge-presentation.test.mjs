import assert from 'node:assert/strict';
import test from 'node:test';
import { bridgeHasNoInput, bridgeSchemaSummary, bridgeUsage, groupBridgeOperations } from './bridge-presentation.ts';

// Declared empty input differs from an open response whose fields are not documented.
test('Bridge schema explanations preserve contract meaning', () => {
  const empty = { type: 'object', properties: {}, additionalProperties: false };
  assert.equal(bridgeHasNoInput(empty), true);
  assert.match(bridgeSchemaSummary(empty, 'Input'), /No input required/);
  assert.match(bridgeSchemaSummary({ type: 'object' }, 'Output'), /does not mean the data is empty/);
  assert.equal(bridgeSchemaSummary(false, 'Output'), null);
  assert.equal(bridgeSchemaSummary({ type: 'object', properties: { accepted: { type: 'boolean' } } }, 'Output'), null);
});

// References and composed validation can impose arguments even with no named root properties.
test('unknown or composed input never gets a misleading empty argument example', () => {
  // Exercise unsupported shapes without assuming that every object accepts empty input.
  for (const schema of [null, true, false, { type: 'object' }, { type: 'object', additionalProperties: false, allOf: [{ required: ['id'] }] }]) {
    assert.equal(bridgeHasNoInput(schema), false);
    assert.match(bridgeUsage('pair', 'profile', 'hash', 'connection', 'op', { input_schema: schema }).code, /  input,/);
  }
});

// Identical operation names on separate profiles retain distinct immutable routing identities.
test('code and binding stay aligned without mixing profiles or unsafe identity text', () => {
  const contract = { input_schema: { type: 'object', additionalProperties: false } };
  const a = bridgeUsage('pair', 'upstairs', 'hash-a', 'speaker', 'volume.get', contract);
  const b = bridgeUsage('pair', 'downstairs"\n', 'hash-b', 'speaker', 'volume.get', contract);
  assert.match(a.code, /fused.callBridge/);
  assert.match(a.code, /  {},/);
  assert.equal(JSON.parse(a.binding).bridges.local_operation.profile_id, 'upstairs');
  assert.equal(JSON.parse(b.binding).bridges.local_operation.profile_id, 'downstairs"\n');
  assert.equal(JSON.parse(b.binding).bridges.local_operation.manifest_hash, 'hash-b');
  assert.equal(a.code, b.code);
});

// A profile can expose the same operation on several connections, including interleaved catalogue entries.
test('connection grouping and search retain exact operation identities', () => {
  const operations = [
    { connection_id: 'Kitchen', operation_id: 'volume.get' },
    { connection_id: '__proto__', operation_id: 'device.info' },
    { connection_id: 'Bedroom', operation_id: 'volume.get' },
    { connection_id: 'Kitchen', operation_id: 'volume.set' },
  ];
  assert.deepEqual(groupBridgeOperations(operations, '').map(/** Compare both identity and operation count for every group. */ group => [group.connection, group.operations.length]), [['Kitchen', 2], ['__proto__', 1], ['Bedroom', 1]]);
  assert.deepEqual(groupBridgeOperations(operations, ' KITCHEN ')[0].operations, [operations[0], operations[3]]);
  assert.equal(groupBridgeOperations(operations, 'volume.get').length, 2);
  assert.equal(groupBridgeOperations(operations, 'missing').length, 0);
  assert.equal(groupBridgeOperations([], '').length, 0);
});
