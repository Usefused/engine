import assert from 'node:assert/strict';
import test from 'node:test';
import { diffCode, codeDiffContext } from './code-diff.ts';

// Reconstructing both sides proves no removed line leaks into the current editor value.
test('diff preserves both files through insertions, replacements and deletions', () => {
  for (const [before, after] of [['a\nb\nc', 'a\nnew\nc'], ['', 'new'], ['old', ''], ['a\na\nb', 'a\nb\na'], ['a\n', 'a'], ['same', 'same']]) {
    const rows = diffCode(before, after);
    assert.equal(rows.filter(row => row.kind !== 'added').map(row => row.text).join('\n'), before);
    assert.equal(rows.filter(row => row.kind !== 'removed').map(row => row.text).join('\n'), after);
    assert.deepEqual(rows.filter(row => row.oldLine).map(row => row.oldLine), before === '' ? [] : before.split('\n').map((_, i) => i + 1));
    assert.deepEqual(rows.filter(row => row.newLine).map(row => row.newLine), after === '' ? [] : after.split('\n').map((_, i) => i + 1));
  }
});

// The common lines stay neutral while replacement lines remain reviewable in old/new order.
test('replacement is one removed line followed by one added line', () => {
  assert.deepEqual(diffCode('start\nold\nend', 'start\nnew\nend').map(row => row.kind), ['unchanged', 'removed', 'added', 'unchanged']);
});

// Large full-file changes must remain bounded yet reconstructable rather than exhausting editor memory.
test('large replacements preserve exact content with bounded comparison', () => {
  const before = Array.from({ length: 1000 }, (_, i) => `old ${i}`).join('\n');
  const after = Array.from({ length: 1000 }, (_, i) => `new ${i}`).join('\n');
  const rows = diffCode(before, after);
  assert.equal(rows.length, 2000);
  assert.equal(rows.filter(row => row.kind === 'added').map(row => row.text).join('\n'), after);
});

// Separate hunks retain nearby context without filling the review with unchanged files.
test('review collapses unchanged runs and keeps every change', () => {
  const before = Array.from({ length: 30 }, (_, i) => `line ${i}`).join('\n');
  const rows = diffCode(before, before.replace('line 5\n', 'changed 5\n').replace('line 25\n', 'changed 25\n'));
  const review = codeDiffContext(rows);
  assert.deepEqual(review.filter(row => ['added', 'removed'].includes(row.kind)), rows.filter(row => row.kind !== 'unchanged'));
  assert.equal(review.filter(row => row.kind === 'gap').length, 3);
});
