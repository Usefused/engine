import assert from 'node:assert/strict';
import test from 'node:test';
import {schemaExample} from './schema-example.ts';

// Editable app payloads must not contain the schema envelope or descriptors.
test('generates ready-to-edit nested JSON including Stripe-style line item arrays', () => {
 const schema = {type:'object',required:['line_items'],properties:{customer:{type:'string'},line_items:{type:'array',items:{type:'object',properties:{price:{type:'string'},quantity:{type:'integer',minimum:1}}}},mode:{enum:['subscription','payment']},success_url:{type:'string',format:'uri'}}};
 assert.deepEqual(JSON.parse(schemaExample(schema).json), {customer:'string',line_items:[{price:'string',quantity:1}],mode:'subscription',success_url:'https://example.com'});
});

// Explicit literals must survive generation, including falsy examples, rather than being replaced by type placeholders.
test('preserves authored values and selects a useful union branch', () => {
 const schema = {type:'object',properties:{count:{const:0},enabled:{default:false},nothing:{example:null},name:{examples:['Taylor']},choice:{anyOf:[{type:'null'},{type:'string',enum:['active']}]}}};
 assert.deepEqual(JSON.parse(schemaExample(schema).json),{count:0,enabled:false,nothing:null,name:'Taylor',choice:'active'});
});

// Reusable definitions may repeat on siblings, but cyclic or external references must remain unavailable.
test('resolves local definitions and refuses cyclic or remote schemas', () => {
 const schema = {type:'object',properties:{a:{$ref:'#/$defs/Name'},b:{$ref:'#/$defs/Name'}},$defs:{Name:{type:'string'}}};
 assert.deepEqual(JSON.parse(schemaExample(schema).json),{a:'string',b:'string'});
 assert.ok(schemaExample({$ref:'#'}).error);
 assert.ok(schemaExample({$ref:'https://example.com/schema'}).error);
 assert.ok(schemaExample(false).error);
 assert.ok(schemaExample({type:'array',minItems:100000,items:{type:'string'}}).error);
});

// Common collection and scalar limits keep scaffolds useful without pretending to be a complete JSON Schema validator.
test('handles empty arrays, negative ranges, nullable types, and bounded strings', () => {
 assert.equal(schemaExample({type:'array',maxItems:0,items:{type:'string'}}).json,'[]');
 assert.equal(schemaExample({type:'integer',maximum:-2}).json,'-2');
 assert.equal(schemaExample({type:['string','null'],minLength:8}).json,'"stringxx"');
 assert.equal(schemaExample({type:'string',maxLength:3}).json,'"str"');
});
