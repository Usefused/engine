import { describe, expect, it } from "vitest";
import { Fixture, type FixtureOperation, type FixtureSchemaContract } from "./fixture.js";
import { searchDocs } from "./searchDocs.js";

/** Keeps the shared reference independent from the one fixture-level definition dictionary. */
function sharedFixture(): Fixture {
  const definition: FixtureSchemaContract = {
    dialect: "https://json-schema.org/draft/2020-12/schema", content_hash: "fixture",
    raw: { type: "object", properties: { label: { type: "string" } } }, projection: {},
  };
  const operation: FixtureOperation = {
    operation_id: "createItem", service_id: "service", service_version_id: "version-a", name: "createItem",
    description: "Create item", method: "POST", path: "/items", parameters: [], responses: {},
    pagination: { supported: false, caller_bound_supported: false },
    request_content: { representations: [{media_type: "application/json", serialization: "json", schema: {
      dialect: definition.dialect, content_hash: "root", shared_definitions: true,
      raw: { $ref: "#/$defs/Payload" }, projection: {},
    }}]},
  };
  return new Fixture([operation], [], { "version-a": { Payload: definition } });
}

describe("shared schema documentation", () => {
  // Shared dependencies remain visible without expanding the graph into every operation response.
  it("advertises a lazy dictionary section while keeping the compact root", () => {
    const result = searchDocs(sharedFixture(), { operationId: "createItem" });
    expect(result).toMatchObject({ mode: "operationId", operation: {
      schema_status: { complete: false, available_sections: ["parameters", "request", "definitions"] },
      request_content: { representations: [{ schema: { raw: { $ref: "#/$defs/Payload" } } }] },
    }});
    expect(JSON.stringify(result)).not.toContain('"label"');
  });

  // Existing exact section/JSON Pointer retrieval is enough; no extra tool or schema copy is needed.
  it("retrieves one saved definition with the existing bounded section API", () => {
    const result = searchDocs(sharedFixture(), { operationId: "createItem", section: "definitions", schemaPath: "/Payload/raw/properties/label" });
    expect(result).toMatchObject({ mode: "section", value: { type: "string" }, schema_status: { complete: true } });
  });
});
