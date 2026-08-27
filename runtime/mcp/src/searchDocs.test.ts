import { describe, expect, it } from "vitest";
import { Fixture, FixtureOperation, FixtureUnifiedOperation } from "./fixture.js";
import { SEARCH_DOCS_MAX_BYTES, SEARCH_DOCS_MAX_LIMIT, searchDocs } from "./searchDocs.js";

const listRepos: FixtureOperation = {
  operation_id: "github.listRepos",
  service_id: "svc-1",
  name: "List user repositories",
  description: "List public repositories for a GitHub user.",
  method: "GET",
  path: "/users/{username}/repos",
  pagination: { supported: true, caller_bound_supported: true, engine_max_pages: 100 },
  parameters: [{
    name: "username", in: "path", required: true, type: "string", description: "",
    serialization: { style: "simple", explode: false, allow_reserved: false, allow_empty_value: false },
  }],
  responses: { "200": { description: "Repositories", representations: [] } },
};

const getRepo: FixtureOperation = {
  operation_id: "github.getRepo",
  service_id: "svc-1",
  name: "Get a repository",
  description: "Get one repository by owner and name.",
  method: "GET",
  path: "/repos/{owner}/{repo}",
  pagination: { supported: false, caller_bound_supported: false },
  parameters: ["owner", "repo"].map((name) => ({
    name, in: "path" as const, required: true, type: "string", description: "",
    serialization: { style: "simple", explode: false, allow_reserved: false, allow_empty_value: false },
  })),
  request_content: {
    required: true,
    payload_parameter: "body",
    representations: [{ media_type: "application/json", serialization: "json" }],
  },
  responses: {
    "200": {
      description: "Repository",
      representations: [{
        media_type: "application/json",
        schema: {
          dialect: "https://json-schema.org/draft/2020-12/schema",
          raw: { type: "object", properties: { id: { type: "string" } } },
          content_hash: "sha256:test",
          projection: { type: "object", properties: { id: { type: "string" } } },
        },
      }],
    },
  },
};

const syncRepos = {
  name: "repos.sync",
  description: "Synchronize reviewed repositories.",
  input_schema: { type: "object", properties: { owner: { type: "string" } } },
  output_schema: { type: "object" },
  targets: [{
    public_target: "source", service_target: "github", operation_id: "github.listRepos",
    depends_on: [], output_schema: { type: "array" },
    service_id: "private-service", service_version_id: "private-version", endpoint_id: "private-endpoint",
    rollback: { operation_id: "github.restoreRepos", service_id: "private-rollback", endpoint_id: "private-endpoint" },
  }],
  private_mapping: { forbidden: true },
} as unknown as FixtureUnifiedOperation;

/** testFixture returns one deterministic mixed physical catalogue. */
function testFixture(): Fixture {
  return new Fixture([listRepos, getRepo]);
}

/** encodedBytes measures the exact UTF-8 tool-result cost asserted by the runtime. */
function encodedBytes(value: unknown): number {
  return Buffer.byteLength(JSON.stringify(value), "utf8");
}

/** oversizedResponseOperation creates a valid section that cannot fit the semantic output budget. */
function oversizedResponseOperation(): FixtureOperation {
  return {
    ...getRepo,
    operation_id: "github.largeRepo",
    name: "Get a large repository contract",
    responses: {
      "200": {
        description: "Large schema",
        representations: [{
          media_type: "application/json",
          schema: {
            dialect: "https://json-schema.org/draft/2020-12/schema",
            raw: { type: "object", description: "x".repeat(SEARCH_DOCS_MAX_BYTES * 2) },
            content_hash: "sha256:large",
            projection: { type: "object" },
          },
        }],
      },
    },
  };
}

/** largeProperties creates navigable schema data that exceeds the semantic budget as a whole. */
function largeProperties(count: number): Record<string, { description: string }> {
  const properties: Record<string, { description: string }> = {};
  for (let index = 0; index < count; index++) {
    properties[`field_${String(index).padStart(4, "0")}`] = { description: "é".repeat(48) };
  }
  return properties;
}

describe("searchDocs", () => {
  // Orientation must not spend its budget on unusable partial schemas.
  it("returns a bounded schema-free list window", () => {
    const result = searchDocs(testFixture(), {});
    // Narrowing fails explicitly when routing regresses so later schema-free assertions remain meaningful.
    if (result.mode !== "list") throw new Error("expected list mode");

    expect(result.operations).toHaveLength(2);
    expect(result.total).toBe(2);
    expect(result.truncated).toBe(false);
    for (const operation of result.operations) {
      // List mode never spends its semantic budget on partial schema fragments.
      expect(operation).not.toHaveProperty("parameters");
      expect(operation).not.toHaveProperty("responses");
      expect(operation).not.toHaveProperty("pagination");
    }
    expect(encodedBytes(result)).toBeLessThanOrEqual(SEARCH_DOCS_MAX_BYTES);
  });

  // One bounded ranking replaces a catalogue scan followed by a mandatory exact lookup.
  it("ranks intent and clamps the result window", () => {
    const result = searchDocs(testFixture(), { query: "list repositories", limit: SEARCH_DOCS_MAX_LIMIT + 20 });
    // Query assertions require the ranked branch rather than a silently changed mode.
    if (result.mode !== "query") throw new Error("expected query mode");

    expect(result.operations[0]?.operation_id).toBe("github.listRepos");
    expect(result.operations.length).toBeLessThanOrEqual(SEARCH_DOCS_MAX_LIMIT);
    expect(encodedBytes(result)).toBeLessThanOrEqual(SEARCH_DOCS_MAX_BYTES);
  });

  // Small reviewed contracts should remain directly usable without lazy follow-up calls.
  it("returns every complete small section for one exact physical operation", () => {
    const result = searchDocs(testFixture(), { operationId: "github.getRepo" });
    // Physical detail is required before inspecting its parameter and response contract.
    if (result.mode !== "operationId" || "error" in result || !("parameters" in result.operation)) {
      throw new Error("expected physical operation detail");
    }

    expect(result.operation.parameters).toHaveLength(2);
    expect(result.operation.pagination).toEqual({
      supported: false,
      caller_bound_supported: false,
      usage: "This GET operation has no Engine pagination contract, so each call() makes one provider request and does not traverse provider pages. Omit the third call() argument's pagination option. Only when this operation's documentation explicitly identifies a page input, continuation output, and stop condition, pass the page input in params, await the two-argument call(), read the continuation from its result, and repeat until the documented stop condition. Never infer page, cursor, or offset semantics from names. Each page counts toward execute's call and time limits.",
    });
    expect(result.operation.responses?.["200"]).toBeDefined();
    expect(result.operation.schema_status).toEqual({
      complete: true,
      included_sections: ["parameters", "request", "response:200"],
      available_sections: ["parameters", "request", "response:200"],
    });
  });

  // Pagination-looking provider parameters cannot manufacture an absent Engine contract.
  it("does not infer pagination from GET parameter names", () => {
    const operation: FixtureOperation = {
      ...getRepo,
      operation_id: "github.getRepoPage",
      parameters: [...(getRepo.parameters ?? []), {
        name: "cursor", in: "query", required: false, type: "string", description: "",
        serialization: { style: "form", explode: true, allow_reserved: false, allow_empty_value: false },
      }],
    };
    const result = searchDocs(new Fixture([operation]), { operationId: operation.operation_id });
    // Exact detail proves the documented provider input cannot change Engine-owned policy metadata.
    if (result.mode !== "operationId" || "error" in result || !("pagination" in result.operation)) {
      throw new Error("expected physical operation detail");
    }

    expect(result.operation.pagination).toMatchObject({ supported: false, caller_bound_supported: false });
    expect(result.operation.pagination.usage).toContain("Never infer page, cursor, or offset semantics from names");
  });

  // Non-GET operations need the omission rule without irrelevant manual GET instructions.
  it("keeps unsupported POST guidance method-appropriate", () => {
    const operation: FixtureOperation = { ...getRepo, operation_id: "github.createRepo", method: "POST" };
    const result = searchDocs(new Fixture([operation]), { operationId: operation.operation_id });
    // Exact detail is required before comparing the method-specific instruction.
    if (result.mode !== "operationId" || "error" in result || !("pagination" in result.operation)) {
      throw new Error("expected physical operation detail");
    }

    expect(result.operation.pagination.usage).toBe("This operation has no Engine pagination contract. Omit the third call() argument's pagination option.");
  });

  // Paginated list guidance must show the separate execution control rather than provider page-size fields.
  it("explains how to bound Engine traversal for a paginated physical operation", () => {
    const result = searchDocs(testFixture(), { operationId: "github.listRepos" });
    // Exact physical detail is required before asserting its execution guidance.
    if (result.mode !== "operationId" || "error" in result || !("pagination" in result.operation)) {
      throw new Error("expected physical operation detail");
    }

    expect(result.operation.pagination).toEqual({
      supported: true,
      caller_bound_supported: true,
      engine_max_pages: 100,
      usage: "Engine follows provider pagination automatically. To accept a partial result, pass { pagination: { maxPages: N } } as call()'s third argument, where N is a positive integer lower than engine_max_pages. Provider page-size parameters do not bound total Engine traversal.",
    });
    // Discovery must not leak provider continuation internals through the public policy summary.
    for (const privatePolicyField of ["request", "response", "continuation", "termination"]) {
      // Discovery exposes only agent-owned controls, never provider continuation internals.
      expect(result.operation.pagination).not.toHaveProperty(privatePolicyField);
    }
  });

  // Query mode must carry the same operation-specific guidance used for immediate execution.
  it("returns distinct pagination guidance for mixed GET query results", () => {
    const result = searchDocs(testFixture(), { query: "repositories", limit: 2 });
    // Guidance assertions require ranked physical detail rather than schema-free list mode.
    if (result.mode !== "query") throw new Error("expected query mode");

    const guidance = Object.fromEntries(result.operations.map((operation) => [operation.operation_id, "pagination" in operation ? operation.pagination : undefined]));
    expect(guidance["github.listRepos"]).toMatchObject({ supported: true, caller_bound_supported: true, engine_max_pages: 100 });
    expect(guidance["github.getRepo"]).toMatchObject({ supported: false, caller_bound_supported: false });
  });

  // A one-page policy is automatic but cannot accept the strict lower bound used by caller controls.
  it("tells callers to omit pagination when the Engine limit is one page", () => {
    const operation: FixtureOperation = {
      ...listRepos,
      operation_id: "github.onePageRepos",
      pagination: { supported: true, caller_bound_supported: false, engine_max_pages: 1 },
    };
    const result = searchDocs(new Fixture([operation]), { operationId: operation.operation_id });
    // Exact physical detail is required before asserting the one-page policy instruction.
    if (result.mode !== "operationId" || "error" in result || !("pagination" in result.operation)) {
      throw new Error("expected physical operation detail");
    }

    expect(result.operation.pagination).toEqual({
      supported: true,
      caller_bound_supported: false,
      engine_max_pages: 1,
      usage: "Engine follows this operation's reviewed pagination contract up to engine_max_pages, but no positive maxPages value is lower than that limit. Omit the third call() argument's pagination option.",
    });
  });

  // Oversized schemas must remain absent as a whole instead of masquerading as complete detail.
  it("omits an oversized section whole and advertises lazy recovery", () => {
    const fixture = new Fixture([oversizedResponseOperation()]);
    const result = searchDocs(fixture, { operationId: "github.largeRepo" });
    // Exact detail must survive even when one section cannot be admitted.
    if (result.mode !== "operationId" || "error" in result || !("schema_status" in result.operation)) {
      throw new Error("expected bounded operation detail");
    }

    expect(result.operation.schema_status.complete).toBe(false);
    expect(result.operation.schema_status.available_sections).toContain("response:200");
    expect(result.operation.schema_status.included_sections).not.toContain("response:200");
    expect(result.operation).not.toHaveProperty("responses");
    expect(encodedBytes(result)).toBeLessThanOrEqual(SEARCH_DOCS_MAX_BYTES);
  });

  // Lazy retrieval returns the selected reviewed subtree without projection or rewriting.
  it("retrieves an exact JSON Pointer subtree from an advertised section", () => {
    const result = searchDocs(testFixture(), {
      operationId: "github.getRepo",
      section: "response:200",
      schemaPath: "/representations/0/schema/raw/properties/id",
    });
    // A section routing or validation failure must not be mistaken for an absent schema value.
    if (result.mode !== "section" || "error" in result) throw new Error("expected section result");

    expect(result.value).toEqual({ type: "string" });
    expect(result.schema_status.complete).toBe(true);
    expect(result.schema_path).toBe("/representations/0/schema/raw/properties/id");
  });

  // Oversized roots need exact navigation hints rather than silently shortened schema values.
  it("returns deterministic child pointers instead of a partial oversized value", () => {
    const result = searchDocs(new Fixture([oversizedResponseOperation()]), {
      operationId: "github.largeRepo",
      section: "response:200",
    });
    // The recovery path is a successful incomplete result, not an opaque size error.
    if (result.mode !== "section" || "error" in result) throw new Error("expected bounded section result");

    expect(result.schema_status.complete).toBe(false);
    expect(result).not.toHaveProperty("value");
    expect(result.available_schema_paths).toEqual(["/description", "/representations"]);
    expect(encodedBytes(result)).toBeLessThanOrEqual(SEARCH_DOCS_MAX_BYTES);
  });

  // Invalid selectors must not trigger broader discovery or expose fixture data in an error.
  it("rejects invalid or unknown exact requests with stable errors", () => {
    const missing = searchDocs(testFixture(), { operationId: "private.operation" });
    expect(missing).toEqual({ mode: "operationId", error: "no such operationId" });
    const invalidPointer = searchDocs(testFixture(), {
      operationId: "github.getRepo", section: "response:200", schemaPath: "not-absolute",
    });
    expect(invalidPointer).toEqual({ mode: "section", error: "schemaPath must be an RFC 6901 JSON Pointer" });
  });

  // Public Unified structure is sufficient for callers; Engine routing identities remain private.
  it("projects Unified documentation without Engine identities or mappings", () => {
    const result = searchDocs(new Fixture([], [syncRepos]), { operationId: "repos.sync" });
    // Leakage assertions inspect a successful public descriptor, not an unrelated error payload.
    if (result.mode !== "operationId" || "error" in result) throw new Error("expected Unified detail");

    const encoded = JSON.stringify(result.operation);
    expect(encoded).toContain("github.restoreRepos");
    for (const forbidden of ["service_id", "service_version_id", "endpoint_id", "private_mapping", "selectors"]) {
      // Public documentation must not reveal compiler or Engine routing identities.
      expect(encoded).not.toContain(forbidden);
    }
  });

  // Prose has an explicit loss marker; schemas never use the same shortening path.
  it("truncates only prose and marks that intentional change", () => {
    const operation = { ...listRepos, operation_id: "long.description", description: "é".repeat(600) };
    const result = searchDocs(new Fixture([operation]), {});
    // Prose bounds are asserted on the schema-free orientation branch.
    if (result.mode !== "list") throw new Error("expected list mode");

    expect(result.operations[0].description_truncated).toBe(true);
    expect(Buffer.byteLength(result.operations[0].description, "utf8")).toBeLessThanOrEqual(512);
  });

  // Equal lexical evidence must not depend on operation kind or fixture insertion order.
  it("uses query default three, maximum five, and deterministic cross-kind ties", () => {
    const physical = Array.from({ length: 6 }, (_, index) => ({
      ...listRepos,
      operation_id: `shared.physical${index}`,
      name: "Repository workflow",
      description: "Shared repository workflow",
    }));
    const unified = { ...syncRepos, name: "shared.logical", description: "Shared repository workflow" };
    const fixture = new Fixture(physical, [unified]);
    const defaulted = searchDocs(fixture, { query: "shared" });
    const clamped = searchDocs(fixture, { query: "shared", limit: 100 });

    // Both windows must come from the same ranking contract before comparing their sizes.
    if (defaulted.mode !== "query" || clamped.mode !== "query") throw new Error("expected query mode");
    expect(defaulted.operations).toHaveLength(3);
    expect(clamped.operations).toHaveLength(5);
    expect(defaulted.operations.map(({ operation_id }) => operation_id)).toEqual([
      "shared.logical", "shared.physical0", "shared.physical1",
    ]);
    expect(JSON.stringify(defaulted)).not.toContain("score");
  });

  // Whitespace has no intent evidence and therefore cannot rank every operation as a match.
  it("treats whitespace as list mode and keeps its summaries token-light", () => {
    const result = searchDocs(testFixture(), { query: " \n\t " });

    // The routing invariant distinguishes orientation from a ranked callable response.
    if (result.mode !== "list") throw new Error("expected list mode");
    expect(result).toMatchObject({ total: 2, truncated: false });
    expect(result.operations[0]).not.toHaveProperty("path");
    expect(result.operations[0]).not.toHaveProperty("schema_status");
  });

  // Byte-level admission prevents multi-byte text from bypassing the semantic result budget.
  it("counts multibyte UTF-8 exactly at the 64 KiB section boundary", () => {
    // The empty result measures fixed wrapper bytes so the payload can exercise the exact boundary.
    const resultFor = (value: string) => searchDocs(
      new Fixture([], [{ ...syncRepos, name: "unicode.boundary", input_schema: value }]),
      { operationId: "unicode.boundary", section: "input" },
    );
    const emptyBytes = encodedBytes(resultFor(""));
    const fittingValue = "é".repeat(Math.floor((SEARCH_DOCS_MAX_BYTES - emptyBytes) / 2));
    const fitting = resultFor(fittingValue);
    const overflowing = resultFor(`${fittingValue}é`);

    expect(encodedBytes(fitting)).toBeLessThanOrEqual(SEARCH_DOCS_MAX_BYTES);
    expect(fitting).toMatchObject({ mode: "section", schema_status: { complete: true } });
    expect(overflowing).toMatchObject({ mode: "section", schema_status: { complete: false }, available_schema_paths: [] });
    expect(encodedBytes(overflowing)).toBeLessThanOrEqual(SEARCH_DOCS_MAX_BYTES);
  });

  // Both large Unified schema kinds must keep their callable summary and safe recovery metadata.
  it("omits large Unified input and target schemas without leaking private identities", () => {
    const operation = {
      ...syncRepos,
      name: "repos.largeSync",
      input_schema: { type: "object", properties: largeProperties(900) },
      targets: [{
        ...syncRepos.targets[0],
        output_schema: { type: "object", properties: largeProperties(900) },
      }],
    } as unknown as FixtureUnifiedOperation;
    const result = searchDocs(new Fixture([], [operation]), { query: "large synchronize repositories" });

    // Query mode must retain the large callable rather than silently dropping its summary.
    if (result.mode !== "query") throw new Error("expected query mode");
    expect(result.operations[0].schema_status).toMatchObject({
      complete: false,
      available_sections: ["input", "targets", "output"],
    });
    expect(encodedBytes(result)).toBeLessThanOrEqual(SEARCH_DOCS_MAX_BYTES);
    for (const forbidden of ["service_id", "service_version_id", "endpoint_id", "private_mapping"]) {
      expect(JSON.stringify(result)).not.toContain(forbidden);
    }
  });

  // Request contracts take priority because callers need them to construct a valid execution.
  it("packs request data before a response that cannot then fit", () => {
    const operation = {
      ...getRepo,
      operation_id: "github.requestFirst",
      request_content: {
        required: true,
        representations: [{ media_type: "application/json", serialization: "json" as const, example: "r".repeat(45_000) }],
      },
      responses: { "200": { description: "s".repeat(30_000), representations: [] } },
    };
    const result = searchDocs(new Fixture([operation]), { operationId: operation.operation_id });
    // Budget pressure still produces exact detail with explicit section completeness.
    if (result.mode !== "operationId" || "error" in result) throw new Error("expected exact detail");

    expect(result.operation.schema_status.included_sections).toContain("request");
    expect(result.operation.schema_status.included_sections).not.toContain("response:200");
    expect(result.operation.schema_status.complete).toBe(false);
  });

  // Pointer validation stays deterministic and bounded across malformed and absent paths.
  it("rejects malformed, missing, and over-deep schema paths", () => {
    const fixture = testFixture();
    const paths = ["relative", "/bad~2escape", "/missing", "/x".repeat(33)];
    for (const schemaPath of paths) {
      const result = searchDocs(fixture, { operationId: "github.getRepo", section: "response:200", schemaPath });
      expect(result).toHaveProperty("error");
    }
  });

  // Escaped keys remain exact while inherited properties and array aliases stay inaccessible.
  it("resolves escaped schema keys without prototype traversal or array aliases", () => {
    const operation = { ...syncRepos, input_schema: { "a/b": { "~key": ["exact"] } } };
    const fixture = new Fixture([], [operation]);
    const exact = searchDocs(fixture, { operationId: operation.name, section: "input", schemaPath: "/a~1b/~0key/0" });
    const inherited = searchDocs(fixture, { operationId: operation.name, section: "input", schemaPath: "/constructor" });
    const alias = searchDocs(fixture, { operationId: operation.name, section: "input", schemaPath: "/a~1b/~0key/00" });

    expect(exact).toMatchObject({ mode: "section", value: "exact", schema_status: { complete: true } });
    expect(inherited).toHaveProperty("error");
    expect(alias).toHaveProperty("error");
  });
});
