import { describe, expect, it } from "vitest";
import { Fixture, FixtureOperation } from "./fixture.js";
import { searchDocs } from "./searchDocs.js";

const listRepos: FixtureOperation = {
  operation_id: "github.listRepos",
  service_id: "svc-1",
  name: "List user repositories",
  description: "List public repositories for the specified GitHub user.",
  method: "GET",
  path: "/users/{username}/repos",
  parameters: [
    { name: "username", in: "path", required: true, type: "string" },
    { name: "per_page", in: "query", required: false, type: "integer" },
  ],
  responses: { "200": { type: "array" } },
};

const getRepo: FixtureOperation = {
  operation_id: "github.getRepo",
  service_id: "svc-1",
  name: "Get a repository",
  description: "Get a single repository by owner and repository name.",
  method: "GET",
  path: "/repos/{owner}/{repo}",
  parameters: [
    { name: "owner", in: "path", required: true, type: "string" },
    { name: "repo", in: "path", required: true, type: "string" },
  ],
  request_content: {
    media_type: "application/octet-stream",
    serialization: "raw",
    required: true,
    schema: { type: "string" },
    payload_parameter: "body",
    binary_encoding: "base64",
  },
  responses: { "200": { type: "object" } },
};

function testFixture(): Fixture {
  return new Fixture([listRepos, getRepo]);
}

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
});
