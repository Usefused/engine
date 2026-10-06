import { test } from 'node:test';
import assert from 'node:assert/strict';
import { harnest } from './fused-agent-transport.ts';

const approval = { type: 'human_approval', id: 'approval-1', callId: 'call-1', action: 'update_form_field', message: 'Change Name to Checkout?', expiresAt: '2099-01-01T00:00:00Z' };
const edit = { type: 'client_tool', id: 'edit-1', callId: 'call-1', name: 'update_form_field', arguments: { value: 'Checkout' } };

/** Models the real SSE suspension and subsequent JSON continuations without live model calls. */
function runtime(t, decision) {
  const paths = [];
  t.mock.method(globalThis, 'fetch', async (path, init) => {
    paths.push(path);
    // Initial streaming completion must suspend before the edit is available.
    if (path === '/agent/responses') return new Response(`data: ${JSON.stringify({ type: 'response.completed', status: 'requires_action', requiredAction: approval })}\n\n`);
    // Denial completes without ever issuing an executable client request.
    if (path === '/agent/approvals/approval-1') {
      assert.equal(JSON.parse(init.body).decision, decision);
      return Response.json(decision === 'approve' ? { status: 'requires_action', requiredAction: edit } : { status: 'completed', outputText: 'No change made.' });
    }
    assert.equal(path, '/agent/client-tools/edit-1');
    return Response.json({ status: 'completed', outputText: 'Updated.' });
  });
  return paths;
}

// An explicit decision must precede both frontend execution and continuation submission.
test('approval pauses edits until the user approves the exact call', async t => {
  const paths = runtime(t, 'approve');
  let edits = 0;
  let decide;
  let opened;
  const shown = new Promise(resolve => { opened = resolve; });
  const run = harnest.streamResponse('Change name', 'session', () => {}, undefined, async () => { edits++; return { updated: true }; }, {}, async value => {
    assert.equal(value.id, approval.id);
    opened();
    return new Promise(resolve => { decide = resolve; });
  });
  await shown;
  assert.equal(edits, 0);
  assert.deepEqual(paths, ['/agent/responses']);
  decide('approve');
  await run;
  assert.equal(edits, 1);
});

// Denial reaches the server, but no rejected change reaches the page executor.
test('denied changes never execute', async t => {
  const paths = runtime(t, 'deny');
  let edits = 0;
  await harnest.streamResponse('Change name', 'session', () => {}, undefined, async () => { edits++; }, {}, async () => 'deny');
  assert.equal(edits, 0);
  assert.deepEqual(paths, ['/agent/responses', '/agent/approvals/approval-1']);
});

// Cancellation invalidates even a late approve click before any network decision or edit.
test('stopping while awaiting approval cannot apply the pending change', async t => {
  const paths = runtime(t, 'approve');
  const controller = new AbortController();
  await assert.rejects(harnest.streamResponse('Change name', 'session', () => {}, controller.signal, async () => assert.fail('edit ran'), {}, async () => { controller.abort(); return 'approve'; }), { name: 'AbortError' });
  assert.deepEqual(paths, ['/agent/responses']);
});

// Older clients must fail closed when they cannot render the required approval controls.
test('missing approval UI cannot silently approve a change', async t => {
  const paths = runtime(t, 'approve');
  await assert.rejects(harnest.streamResponse('Change name', 'session', () => {}, undefined, async () => assert.fail('edit ran')), /Approval is unavailable/);
  assert.deepEqual(paths, ['/agent/responses']);
});
