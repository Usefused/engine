import assert from 'node:assert/strict';
import test from 'node:test';
import {appExecutionURL} from './app-execution-url.ts';

// Public routing is explicit and preserves deployment base paths without copying the HTTP method.
test('execution endpoint uses configured Engine URL and exact app identity',()=>{
 assert.equal(appExecutionURL('app-id','https://engine.example/base/'),'https://engine.example/base/v1/apps/app-id/executions');
 assert.equal(appExecutionURL('app-id'),'/v1/apps/app-id/executions');
 for(const origin of ['javascript:alert(1)','https://user:secret@engine.example','https://engine.example?token=private','https://engine.example#fragment']){
  assert.equal(appExecutionURL('app-id',origin),'/v1/apps/app-id/executions');
 }
});

// Name-based defaults preserve punctuation as one segment and retain explicit UUID fallback URLs.
test('Unified App endpoints prefer escaped names while UUID-shaped names fall back', () => {
 const family = '40e5eff1-3b32-4d4d-854d-f2621a43ad6e';
 assert.equal(appExecutionURL(family, 'https://engine.example/', ' Café: Billing/100% '), 'https://engine.example/v1/apps/Caf%C3%A9%3A%20Billing%2F100%25/executions');
 assert.equal(appExecutionURL(family, '', 'billing'), '/v1/apps/billing/executions');
 for (const name of ['', '  ', family, family.replaceAll('-', ''), `{${family}}`, `urn:uuid:${family}`]) {
  assert.equal(appExecutionURL(family, '', name), `/v1/apps/${family}/executions`);
 }
});
