import assert from "node:assert/strict";
import test from "node:test";
import { webhookSchemaPending } from "./webhook-schema.ts";

// Missing and generic object contracts must retain the inference explanation.
test("webhook bodies remain pending until a shape is declared", () => {
  for (const schema of [undefined, null, {}, { type: "object" }, { type: "object", properties: {} }, { type: "unknown" }]) {
    assert.equal(webhookSchemaPending(schema), true);
  }
});

// Valid non-object and referenced shapes must not receive a false pending notice.
test("declared webhook shapes do not require object properties", () => {
  for (const schema of [
    { type: "object", properties: { id: { type: "string" } } },
    { $ref: "#/components/schemas/Event" },
    { type: "array", items: { type: "string" } },
    { type: "string" },
    { type: "number" },
    { oneOf: [{ type: "string" }] },
    { anyOf: [{ type: "object" }] },
    { allOf: [{ $ref: "#/components/schemas/Event" }] },
  ]) {
    assert.equal(webhookSchemaPending(schema), false);
  }
});
