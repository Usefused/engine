import test from 'node:test';
import assert from 'node:assert/strict';
import { composeUnifiedAppConsumer, unifiedAppAlias } from './unified-app-consumer.ts';
const app = { app_id: 'target', kind: 'unified_app', name: 'Customer lookup', version: '1.0.0' };
// Attaching a capability must preserve private routing and all pre-existing consumer scope.
test('existing consumer retains configuration and gets a new version', () => {
 const source = { name: 'Portal', version: '2.0.0', kind: 'sdk', bucket: 'production', services: { stripe: { version: 'v1', operations: ['getCustomer'], bucket: 'private' } }, language: 'python' };
 const next = composeUnifiedAppConsumer({ version: '2.0.1', services: {}, bucket: 'wrong' }, source, app, 'lookup');
 assert.equal(next.version, '2.0.1'); assert.equal(next.bucket, 'production'); assert.deepEqual(next.services, source.services);
 assert.deepEqual(next.unified_apps.lookup, { name: app.name, version: app.version }); assert.equal(source.version, '2.0.0');
});
// Alias collisions are explicit errors instead of implicit replacement of an existing tool.
test('rejects unsafe aliases and conflicting targets', () => {
 assert.throws(() => composeUnifiedAppConsumer({}, undefined, app, '../x'));
 assert.throws(() => composeUnifiedAppConsumer({}, { unified_apps: { lookup: { name: 'Other', version: '1.0.0' } } }, app, 'lookup'));
 assert.equal(unifiedAppAlias('Customer lookup'), 'customer_lookup');
 assert.equal(unifiedAppAlias('Class'), 'app_class');
 assert.throws(() => composeUnifiedAppConsumer({}, undefined, app, 'execute'));
 assert.throws(() => composeUnifiedAppConsumer({}, { unified_apps: { lookup_sync: { name: 'Other', version: '1.0.0' } } }, app, 'lookup'));
});
