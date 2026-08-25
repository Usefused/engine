import { describe, expect, it } from "vitest";
import { Fixture, FixtureOperation, FixtureUnifiedOperation } from "./fixture.js";
import { searchDocs } from "./searchDocs.js";

const listRepos: FixtureOperation = {
  operation_id: "github.listRepos",
  service_id: "svc-1",
  name: "List user repositories",
  description: "List public repositories for the specified GitHub user.",
  method: "GET",
  path: "/users/{username}/repos",
  parameters: [
    {
      name: "username", in: "path", required: true, type: "string", description: "",
      serialization: { style: "simple", explode: false, allow_reserved: false, allow_empty_value: false },
    },
    {
      name: "per_page", in: "query", required: false, type: "integer", description: "",
      serialization: { style: "form", explode: true, allow_reserved: false, allow_empty_value: false },
    },
  ],
  responses: { "200": { description: "Repositories", representations: [] } },
};

const getRepo: FixtureOperation = {
  operation_id: "github.getRepo",
  service_id: "svc-1",
  name: "Get a repository",
  description: "Get a single repository by owner and repository name.",
  method: "GET",
  path: "/repos/{owner}/{repo}",
  parameters: [
    {
      name: "owner", in: "path", required: true, type: "string", description: "",
      serialization: { style: "simple", explode: false, allow_reserved: false, allow_empty_value: false },
    },
    {
      name: "repo", in: "path", required: true, type: "string", description: "",
      serialization: { style: "simple", explode: false, allow_reserved: false, allow_empty_value: false },
    },
  ],
  request_content: {
    required: true,
    payload_parameter: "body",
    representations: [{
      media_type: "application/octet-stream",
      serialization: "raw",
      schema: {
        dialect: "https://json-schema.org/draft/2020-12/schema",
        raw: { type: "string" },
        content_hash: "sha256:test",
        projection: { type: "string" },
      },
      item_encoding: { binary_encoding: "base64" },
    }],
  },
  responses: {
    "200": {
      description: "Repository",
      representations: [{ media_type: "application/json" }],
    },
  },
};

function testFixture(): Fixture {
  return new Fixture([listRepos, getRepo]);
}

const syncRepos = {
  name: "repos.sync",
  description: "Synchronize reviewed repositories.",
  input_schema: { type: "object", properties: { owner: { type: "string" } } },
  output_schema: { type: "object" },
  targets: [{
    public_target: "source", service_target: "github", operation_id: "github.listRepos",
    depends_on: [], output_schema: { type: "array" },
    service_id: "internal-service", service_version_id: "internal-version", endpoint_id: "internal-endpoint",
    rollback: { operation_id: "github.restoreRepos", service_id: "rollback-service", endpoint_id: "rollback-endpoint" },
  }],
  private_mapping: { forbidden: true },
} as unknown as FixtureUnifiedOperation;

describe("searchDocs", () => {
  it("list mode (no args) returns every operation as bare identifiers only", () => {
    const result = searchDocs(testFixture(), {});
    expect(result.mode).toBe("list");
    if (result.mode !== "list") throw new Error("expected list mode");
    expect(result.operations).toHaveLength(2);
    // Deliberately schema-free -- no parameters/request_content/responses keys
    // should appear on a summary (design doc, Discovery section).
    for (const op of result.operations) {
      expect(op).not.toHaveProperty("parameters");
      expect(op).not.toHaveProperty("request_content");
      expect(op).not.toHaveProperty("responses");
    }
  });

  it("query mode returns a fuzzy-narrowed, still schema-free list", () => {
    const result = searchDocs(testFixture(), { query: "list repos" });
    expect(result.mode).toBe("query");
    if (result.mode !== "query") throw new Error("expected query mode");
    expect(result.operations.map((o) => o.operation_id)).toContain("github.listRepos");
  });

  it("query mode respects the limit", () => {
    const result = searchDocs(testFixture(), { query: "repo", limit: 1 });
    if (result.mode !== "query") throw new Error("expected query mode");
    expect(result.operations).toHaveLength(1);
  });

  it("operationId mode returns full schema for exactly that operation", () => {
    const result = searchDocs(testFixture(), { operationId: "github.getRepo" });
    expect(result.mode).toBe("operationId");
    if (result.mode !== "operationId" || "error" in result) {
      throw new Error("expected a successful operationId result");
    }
    if (!("parameters" in result.operation)) throw new Error("expected physical operation detail");
    expect(result.operation.operation_id).toBe("github.getRepo");
    expect(result.operation.parameters).toHaveLength(2);
    expect(result.operation.parameters[0].in).toBe("path");
    expect(result.operation.request_content).toEqual(getRepo.request_content);
    expect(result.operation.responses["200"]).toBeDefined();
  });

  it("operationId mode returns an error, not a partial match, for an unregistered ID", () => {
    const result = searchDocs(testFixture(), { operationId: "not.registered" });
    expect(result.mode).toBe("operationId");
    if (!("error" in result)) throw new Error("expected an error result");
    expect(result.error).toMatch(/no such operationId/);
  });

  it("operationId takes precedence over query when both are present", () => {
    const result = searchDocs(testFixture(), { operationId: "github.getRepo", query: "list" });
    expect(result.mode).toBe("operationId");
  });

  it("lists and queries Unified operations under exact authored names", () => {
    const fixture = new Fixture([listRepos], [syncRepos]);
    const listed = searchDocs(fixture, {});
    if (listed.mode !== "list") throw new Error("expected list mode");
    expect(listed.operations).toContainEqual({
      kind: "unified", operation_id: "repos.sync", description: "Synchronize reviewed repositories.",
    });
    const queried = searchDocs(fixture, { query: "synchronize repositories" });
    if (queried.mode !== "query") throw new Error("expected query mode");
    expect(queried.operations.map((operation) => operation.operation_id)).toContain("repos.sync");
  });

  it("projects Unified detail without internal identities or private fields", () => {
    const result = searchDocs(new Fixture([], [syncRepos]), { operationId: "repos.sync" });
    if (result.mode !== "operationId" || "error" in result) throw new Error("expected Unified detail");
    if (!("kind" in result.operation)) throw new Error("expected Unified operation detail");
    expect(result.operation.kind).toBe("unified");
    expect(result.operation.operation_id).toBe("repos.sync");
    const encoded = JSON.stringify(result.operation);
    for (const forbidden of ["service_id", "service_version_id", "endpoint_id", "private_mapping", "selectors"]) {
      expect(encoded).not.toContain(forbidden);
    }
    expect(encoded).toContain("github.restoreRepos");
  });
});
