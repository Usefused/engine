import assert from 'node:assert/strict';
import test from 'node:test';
import { updateToolActivity } from '../components/agent/tool-activity.ts';

// A frontend suspension uses a different ID but must not duplicate its streamed tool card.
test('client continuation adopts its pending streamed call once', () => {
  const pending = [{ id: 'stream-1', name: 'get_page_context', status: 'running' }];
  const adopted = updateToolActivity(pending, { id: 'client-1', name: 'get_page_context', status: 'running' }, true);
  assert.deepEqual(adopted, [{ id: 'client-1', name: 'get_page_context', status: 'running' }]);
  assert.equal(updateToolActivity(adopted, { id: 'client-1', name: 'get_page_context', status: 'completed' }).length, 1);
});
// Repeated reads after a completion are separate actions and retain their own outcomes.
test('a later call does not replace completed tool history', () => {
  const completed = [{ id: 'one', name: 'get_page_context', status: 'completed' }];
  assert.equal(updateToolActivity(completed, { id: 'two', name: 'get_page_context', status: 'running' }, true).length, 2);
});
// Failed edits must remain failed rather than render the success affordance used for completed reads.
test('failed client tools retain the correct action and status', () => {
  const calls = [{ id: 'one', name: 'update_form_field', status: 'running' }, { id: 'two', name: 'get_page_context', status: 'completed' }];
  const result = updateToolActivity(calls, { id: 'one', name: 'update_form_field', status: 'failed' });
  assert.equal(result[0].status, 'failed');
  assert.equal(result[1].status, 'completed');
});

import { agentInput, visibleUserRequest } from '../components/agent/agent-input.ts';
// Each turn carries fresh navigation context while history keeps the user's original words.
test('route context round trips without exposing transport copy in history', () => {
  const request = 'Review this form\n\nUser request:\nkeep this literal text';
  const encoded = agentInput(request, '/integrations/buckets');
  assert.match(encoded, /"path":"\/integrations\/buckets"/);
  assert.equal(visibleUserRequest(encoded), request);
  assert.equal(agentInput(request), request);
});
