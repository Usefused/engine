import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

// Wire changes in Engine must reach the distributed client without a manual schema copy.
test('runtime packages the authoritative Engine protocol', async () => {
  const authoritative = await readFile(new URL('../../../../proto/engine/v1/engine.proto', import.meta.url));
  const packaged = await readFile(new URL('../src/engine.proto', import.meta.url));
  assert.deepEqual(packaged, authoritative);
});
