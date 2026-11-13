import assert from 'node:assert/strict';
import test from 'node:test';
import { executeAppTry, TryTokenError } from './app-try-execution.ts';

function fixture() {
  const controller = new AbortController();
  const issued = [], revoked = [], sent = [];
  return {
    controller, issued, revoked, sent,
    options: { familyID: 'family', operation: 'execute', url: 'http://localhost/v1/apps/version/executions', body: '{}', signal: controller.signal },
    tokens: { generate: async (...args) => { issued.push(args); return { token: 'ephemeral' }; }, revoke: async (...args) => { revoked.push(args); } },
    send: async (...args) => { sent.push(args); return new Response('{}', { status: 200 }); },
  };
}

test('automatic Try issues only selected operation with finite expiry and revokes on completion', async () => {
  const f = fixture();
  await executeAppTry(f.options, f.tokens, f.send);
  assert.equal(f.issued[0][0], 'family');
  assert.deepEqual(f.issued[0][1].allow, ['execute']);
  assert.equal(f.issued[0][1].expires_in, 300);
  assert.match(f.issued[0][1].name, /^try-/);
  assert.equal(f.sent[0][1].headers.Authorization, 'Bearer ephemeral');
  assert.equal(f.sent[0][1].credentials, 'omit');
  assert.deepEqual(f.revoked, [['family', f.issued[0][1].name]]);
});

test('supplied execution tokens bypass issuance and are never revoked', async () => {
  const f = fixture();
  await executeAppTry({ ...f.options, token: 'provided' }, f.tokens, f.send);
  assert.equal(f.issued.length, 0);
  assert.equal(f.revoked.length, 0);
  assert.equal(f.sent[0][1].headers.Authorization, 'Bearer provided');
});

test('authoritative issuance denial prevents all runtime dispatch', async () => {
  const f = fixture();
  f.tokens.generate = async () => { throw new Error('forbidden'); };
  await assert.rejects(executeAppTry(f.options, f.tokens, f.send), TryTokenError);
  assert.equal(f.sent.length, 0);
});

test('closing during issuance cleans the late token without executing', async () => {
  const f = fixture();
  let issue;
  f.tokens.generate = () => new Promise(resolve => { issue = resolve; });
  const pending = executeAppTry(f.options, f.tokens, f.send);
  f.controller.abort();
  issue({ token: 'late' });
  await assert.rejects(pending, { name: 'AbortError' });
  assert.equal(f.sent.length, 0);
  assert.equal(f.revoked.length, 1);
});

test('accepted asynchronous and uncertain runs retain finite lifetime rather than interrupting work', async () => {
  const accepted = fixture();
  await executeAppTry(accepted.options, accepted.tokens, async () => new Response('{}', { status: 202 }));
  assert.equal(accepted.revoked.length, 0);
  const uncertain = fixture();
  await assert.rejects(executeAppTry(uncertain.options, uncertain.tokens, async () => { throw new Error('network'); }));
  assert.equal(uncertain.revoked.length, 0);
  assert.equal(uncertain.issued[0][1].expires_in, 300);
});

test('cleanup failure cannot hide a completed response or cause execution retry', async () => {
  const f = fixture();
  f.tokens.revoke = async () => { throw new Error('offline'); };
  const response = await executeAppTry(f.options, f.tokens, f.send);
  assert.equal(response.status, 200);
  assert.equal(f.sent.length, 1);
});

test('closing revokes immediately even if an in-flight transport has not settled', async () => {
  const f = fixture();
  const session = new AbortController();
  let finish;
  const pending = executeAppTry({ ...f.options, sessionSignal: session.signal }, f.tokens, () => new Promise(resolve => { finish = resolve; }));
  await Promise.resolve();
  session.abort();
  assert.equal(f.revoked.length, 1);
  finish(new Response('{}', { status: 200 }));
  await pending;
  assert.equal(f.revoked.length, 1, 'completion must not revoke twice');
});

test('closing revokes tokens retained after accepted or uncertain responses', async () => {
  for (const send of [async () => new Response('{}', { status: 202 }), async () => { throw new Error('network'); }]) {
    const f = fixture();
    const session = new AbortController();
    await executeAppTry({ ...f.options, sessionSignal: session.signal }, f.tokens, send).catch(() => undefined);
    assert.equal(f.revoked.length, 0);
    session.abort();
    assert.equal(f.revoked.length, 1);
  }
});

test('closing the session during issuance never dispatches the late credential', async () => {
  const f = fixture();
  const session = new AbortController();
  let issue;
  f.tokens.generate = () => new Promise(resolve => { issue = resolve; });
  const pending = executeAppTry({ ...f.options, sessionSignal: session.signal }, f.tokens, f.send);
  session.abort();
  issue({ token: 'late' });
  await assert.rejects(pending, { name: 'AbortError' });
  assert.equal(f.sent.length, 0);
  assert.equal(f.revoked.length, 1);
});

test('sidebar closure never revokes a supplied execution token', async () => {
  const f = fixture();
  await executeAppTry({ ...f.options, token: 'provided' }, f.tokens, async () => new Response('{}', { status: 202 }));
  f.controller.abort();
  assert.equal(f.revoked.length, 0);
});
