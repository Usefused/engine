import test from 'node:test';
import assert from 'node:assert/strict';
import { indentCode } from './code-indentation.ts';

// Soft tabs insert at the caret and replace single-line selections without moving to another field.
test('inserts a soft tab at a caret or in place of a selection', () => {
  assert.deepEqual(indentCode('abc', 1, 1, false), { value: 'a  bc', start: 3, end: 3 });
  assert.deepEqual(indentCode('abc', 0, 2, false), { value: '  c', start: 2, end: 2 });
});

// A selection ending at a new line must leave that next line untouched.
test('indents selected lines and preserves the endpoint boundary', () => {
  assert.deepEqual(indentCode('a\nb\nc', 0, 4, false), { value: '  a\n  b\nc', start: 2, end: 8 });
  assert.deepEqual(indentCode('\na', 0, 2, false), { value: '  \n  a', start: 2, end: 6 });
});

// Mixed pasted tabs and spaces should each lose only one indentation level.
test('outdents mixed indentation while retaining the selected text', () => {
  assert.deepEqual(indentCode('  a\n\tb\n c', 2, 9, true), { value: 'a\nb\nc', start: 0, end: 5 });
  assert.deepEqual(indentCode('  a', 1, 1, true), { value: 'a', start: 0, end: 0 });
  assert.deepEqual(indentCode('abc', 2, 2, true), { value: 'abc', start: 2, end: 2 });
});
