import assert from 'node:assert/strict';
import test from 'node:test';
import { compileInputContract, validateInputText } from './schedule-input-validation.ts';

// Nested JSON types and required members must fail without coercing or stripping user data.
test('JSON validation identifies nested paths and preserves false, zero, null and unknown keys', () => {
  const contract = compileInputContract({type:'object',required:['message'],additionalProperties:false,properties:{message:{type:'string'},count:{type:'integer',minimum:0},enabled:{type:'boolean'},rows:{type:'array',items:{type:'object',required:['value'],properties:{value:{type:['number','null']}}}}}});
  const valid = validateInputText('{"message":"hello","count":0,"enabled":false,"rows":[{"value":null}]}',contract);
  assert.deepEqual(valid.issues,[]);
  assert.equal(valid.value.enabled,false);
  assert.equal(valid.value.count,0);
  const invalid = validateInputText('{"count":"0","enabled":"false","rows":[{"value":"wrong"}],"extra":true}',contract);
  assert.deepEqual(new Set(invalid.issues.map(issue=>issue.path)),new Set(['/message','/extra','/count','/enabled','/rows/0/value']));
  assert.equal(invalid.value.count,'0');
  assert.equal(invalid.value.extra,true);
  // Overflowing JSON numbers must not silently serialize as null after validation.
  assert.ok(validateInputText('{"message":"hello","count":1e400}',contract).issues.length);
});

// Full schema validation covers choices and references instead of approximating primitive field types.
test('JSON validation resolves local definitions, unions, enums and constraints', () => {
  const contract = compileInputContract({$defs:{entry:{type:'string',enum:['daily','weekly']}},type:'object',properties:{mode:{$ref:'#/$defs/entry'},choice:{oneOf:[{type:'boolean'},{type:'integer',minimum:1}]}}});
  assert.deepEqual(validateInputText('{"mode":"daily","choice":false}',contract).issues,[]);
  assert.equal(validateInputText('{"mode":"yearly","choice":0}',contract).issues.some(issue=>issue.path==='/mode'),true);
  assert.equal(validateInputText('{"choice":0}',contract).issues.some(issue=>issue.path==='/choice'),true);
  assert.ok(compileInputContract({$schema:'http://json-schema.org/draft-07/schema#',type:'array',items:[{type:'number'}]}).validate);
});

// Broken JSON and unresolved contracts never produce a misleading green success state.
test('syntax and contract failures are explicit without leaking input values', () => {
  const malformed=validateInputText('{"secret":"private-value",}',compileInputContract(true));
  assert.equal(malformed.parsed,false);
  assert.doesNotMatch(malformed.issues[0].message,/private-value/);
  const unavailable=validateInputText('{}',compileInputContract({$ref:'https://example.com/schema'}));
  assert.ok(unavailable.warning);
  assert.equal(validateInputText('{}',compileInputContract(false)).issues.length,1);
});
