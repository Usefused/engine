import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import ts from 'typescript';
import { createBucketContentCache } from './bucket-content-cache.ts';

const pages = {
  secrets: { limit: 10, offset: 0 }, values: { limit: 10, offset: 0 },
  connections: { limit: 10, offset: 0, serviceId: '' }, sdks: { limit: 5, offset: 0 },
  services: { limit: 5, offset: 0, search: '' }, connectionServices: { limit: 20, offset: 0, search: '' },
};
const access = { values: true, secrets: true, connections: true, apps: true, services: true };

/** Executes the real GraphQL selection code with synthetic transport responses only. */
function reader() {
  const calls = [];
  const emptyPage = { items: [], total: 0 };
  const api = { mcpGraphql: async (query, variables) => {
    calls.push({ query, variables });
    return { secretMetaPage: emptyPage, bucketValuePage: emptyPage, authConnectionPage: emptyPage,
      bucketSDKPage: emptyPage, bucketServicePage: emptyPage, connectionServicePage: emptyPage, connectSummary: null };
  } };
  const source = readFileSync(new URL('./buckets.ts', import.meta.url), 'utf8');
  const js = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText;
  const exports = {};
  new Function('require', 'exports', js)(/** Only the API import survives TypeScript's type erasure. */ () => ({ api }), exports);
  return { read: exports.readBucketContents, calls };
}

/** Unvisited views must not fetch values, user connections, usage, or service metadata. */
test('opening secrets fetches one metadata page; each other view reads only its own fields', async () => {
  const { read, calls } = reader();
  await read('bucket-a', pages, access, 'secrets');
  assert.equal(calls.length, 1);
  assert.match(calls[0].query, /secretMetaPage/);
  assert.doesNotMatch(calls[0].query, /encrypted_value|bucketValuePage|authConnectionPage|bucketSDKPage|bucketServicePage/);
  calls.length = 0;
  await read('bucket-a', pages, access, 'apps');
  assert.equal(calls.length, 1);
  assert.match(calls[0].query, /bucketSDKPage/);
  calls.length = 0;
  await read('bucket-a', pages, access, 'services');
  assert.equal(calls.length, 1);
  assert.match(calls[0].query, /bucketServicePage/);
  calls.length = 0;
  await read('bucket-a', pages, access, 'env');
  assert.equal(calls.length, 1);
  assert.match(calls[0].query, /bucketValuePage/);
  calls.length = 0;
  await read('bucket-a', pages, access, 'connected-users');
  assert.equal(calls.length, 3);
  assert.match(calls[0].query, /authConnectionPage/);
  assert.match(calls[1].query, /bucketConnectSummary/);
  assert.match(calls[2].query, /connectionServicePage/);
});

/** Tab selection cannot expand data access beyond the existing field-level grants. */
test('denied views issue no requests and connected-user service filters require service access', async () => {
  const { read, calls } = reader();
  const denied = { values: false, secrets: false, connections: false, apps: false, services: false };
  for (const section of ['secrets', 'env', 'apps', 'services', 'connected-users']) {
    await read('bucket-a', pages, denied, section);
  }
  assert.equal(calls.length, 0);
  await read('bucket-a', pages, { ...access, services: false }, 'connected-users');
  assert.equal(calls.length, 2);
});

/** Completed and in-flight pages remain reusable across navigation, filters, and other buckets. */
test('revisiting tabs or pages reuses cached reads until the bucket is invalidated', async () => {
  const { read, calls } = reader();
  const cache = createBucketContentCache(read);
  const first = cache.read('bucket-a', pages, access, 'secrets');
  assert.equal(cache.read('bucket-a', pages, access, 'secrets'), first);
  await first;
  await cache.read('bucket-a', pages, access, 'apps');
  await cache.read('bucket-b', pages, access, 'secrets');
  await cache.read('bucket-a', { ...pages, services: { ...pages.services, search: 'mail' } }, access, 'secrets');
  assert.equal(calls.length, 3);
  await cache.read('bucket-a', { ...pages, secrets: { limit: 10, offset: 10 } }, access, 'secrets');
  await cache.read('bucket-a', pages, access, 'secrets');
  assert.equal(calls.length, 4);
  cache.invalidate('bucket-a');
  await cache.read('bucket-a', pages, access, 'secrets');
  await cache.read('bucket-a', pages, access, 'apps');
  await cache.read('bucket-b', pages, access, 'secrets');
  assert.equal(calls.length, 6);
  cache.clear();
  await cache.read('bucket-b', pages, access, 'secrets');
  assert.equal(calls.length, 7);
});

/** Responses that started before a write must never repopulate the invalidated cache. */
test('a write during an in-flight read leaves the fresh request authoritative', async () => {
  const pending = [];
  const cache = createBucketContentCache(/** Hold transport completion to reproduce the write/read race. */ () => new Promise((resolve) => pending.push(resolve)));
  const old = cache.read('bucket-a', pages, access, 'secrets');
  cache.invalidate('bucket-a');
  const fresh = cache.read('bucket-a', pages, access, 'secrets');
  pending[1]({ secretMetaTotal: 2 });
  await fresh;
  pending[0]({ secretMetaTotal: 1 });
  await old;
  assert.equal(await cache.read('bucket-a', pages, access, 'secrets'), await fresh);
  assert.equal(pending.length, 2);
});

/** Transient failures remain retryable, and permission changes never reuse an earlier response. */
test('failed reads retry and differing access snapshots have distinct cache entries', async () => {
  let attempts = 0;
  const cache = createBucketContentCache(/** Simulate a one-off transport failure without a live backend. */ async () => {
    attempts++;
    // The first failure must not poison later visits to the same view.
    if (attempts === 1) throw new Error('offline');
    return { secretMetas: [] };
  });
  await assert.rejects(cache.read('bucket-a', pages, access, 'secrets'), /offline/);
  await cache.read('bucket-a', pages, access, 'secrets');
  await cache.read('bucket-a', pages, { ...access, secrets: false }, 'secrets');
  assert.equal(attempts, 3);
});
