import assert from "node:assert/strict";
import test from "node:test";
import { planSchemaReference, resolvePlannedSchemaReference, resolveSchemaPointer, schemaPointerTokens, schemaReferenceLabel } from "./schema-reference.ts";

// Saved component names use RFC 6901 escaping rather than the final pointer segment.
test("decodes complete shared and OpenAPI definition names", () => {
  for (const reference of ["#/$defs/a~1b~0c/properties/id", "#/components/schemas/a~1b~0c/properties/id"]) {
    assert.equal(schemaReferenceLabel(reference), "a/b~c");
    assert.deepEqual(planSchemaReference({}, reference), { kind: "component", name: "a/b~c", suffix: ["properties", "id"] });
  }
  assert.deepEqual(schemaPointerTokens("#/$defs/space%20name"), ["$defs", "space name"]);
});

// Local references must work without network access, including false and root cycles.
test("local definitions take precedence and preserve restrictive boolean schemas", async () => {
  const root = { $defs: { "a/b": false }, properties: { child: { $ref: "#" } } };
  const noFetch = async () => { throw new Error("unexpected component request"); };
  const resolved = await resolvePlannedSchemaReference(planSchemaReference(root, "#/$defs/a~1b"), noFetch);
  assert.equal(resolved.schema, false);
  assert.equal(resolved.document, root);
  assert.equal((await resolvePlannedSchemaReference(planSchemaReference(root, "#"), noFetch)).schema, root);
});

// A missing local child must not fall through to a same-named remote definition.
test("missing local subschemas fail closed without remote fallback", async () => {
  const root = { $defs: { Node: { type: "object" } } };
  const plan = planSchemaReference(root, "#/$defs/Node/properties/missing");
  assert.equal(plan.kind, "unavailable");
  let requests = 0;
  await assert.rejects(resolvePlannedSchemaReference(plan, async () => { requests++; return {}; }));
  assert.equal(requests, 0);
});

// A fetched subschema retains its original raw document for all nested references.
test("fetched definitions preserve local scope and exact subschema selection", async () => {
  const component = { $defs: { Inner: { type: "integer" } }, properties: { selected: { $ref: "#/$defs/Inner" } } };
  const names = [];
  const result = await resolvePlannedSchemaReference(planSchemaReference({}, "#/components/schemas/outer~1name/properties/selected"), async (name) => {
    names.push(name);
    return component;
  });
  assert.deepEqual(names, ["outer/name"]);
  assert.equal(result.document, component);
  assert.equal(result.schema, component.properties.selected);
  const nested = planSchemaReference(result.document, result.schema.$ref);
  assert.equal(nested.kind, "local");
  assert.equal(nested.schema.type, "integer");
});

// External URLs and malformed escaping never become arbitrary Registry lookups.
test("unsupported references cannot initiate network access", async () => {
  for (const reference of ["https://evil.example/schema#/Node", "../schema.json#/Node", "#anchor", "#/$defs/a~2b", "#/$defs/bad%", "#/components/schemas/"]) {
    let requests = 0;
    const plan = planSchemaReference({}, reference);
    assert.equal(plan.kind, "unavailable");
    await assert.rejects(resolvePlannedSchemaReference(plan, async () => { requests++; return {}; }));
    assert.equal(requests, 0);
  }
});

// JSON Pointer traversal never exposes JavaScript prototype members or array aliases.
test("pointer lookup uses own members and canonical array indexes", () => {
  const root = { allOf: [false, { type: "string" }] };
  assert.deepEqual(resolveSchemaPointer(root, ["allOf", "0"]), { found: true, value: false });
  for (const tokens of [["__proto__"], ["constructor"], ["allOf", "length"], ["allOf", "01"], ["allOf", "-1"]]) {
    assert.deepEqual(resolveSchemaPointer(root, tokens), { found: false });
  }
});

// A saved false schema is returned as data while a missing nested target remains an error.
test("remote boolean schemas and missing suffixes remain distinguishable", async () => {
  const result = await resolvePlannedSchemaReference(planSchemaReference({}, "#/$defs/Closed"), async () => false);
  assert.equal(result.schema, false);
  await assert.rejects(resolvePlannedSchemaReference(planSchemaReference({}, "#/$defs/Closed/properties/id"), async () => false), /not found/);
});
