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
