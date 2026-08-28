import { writeFileSync } from "node:fs";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { describe, expect, it } from "vitest";
import { Fixture, loadFixture } from "./fixture.js";

const noPagination = { supported: false, caller_bound_supported: false } as const;
const testServer = {
  name: "fixture-test", title: "Fixture test", version: "1.0.0",
  description: "Exercise the selected fixture operations.",
} as const;

describe("Fixture", () => {
  it("resolves a registered operationId", () => {
    const f = new Fixture([
      { operation_id: "a", service_id: "s", name: "A", method: "GET", path: "/a", responses: {}, pagination: noPagination },
    ], [], {}, testServer);
    expect(f.resolve("a")?.path).toBe("/a");
    expect(f.server.description).toBe(testServer.description);
  });

  it("does not resolve an unregistered operationId -- tier 1 enforcement point", () => {
    const f = new Fixture([
      { operation_id: "a", service_id: "s", name: "A", method: "GET", path: "/a", responses: {}, pagination: noPagination },
    ], [], {}, testServer);
    expect(f.resolve("b")).toBeUndefined();
  });

  it("rejects an operation with no operation_id at construction time", () => {
    expect(
      () =>
        new Fixture([
          // @ts-expect-error -- deliberately malformed for the test
          { service_id: "s", name: "A", method: "GET", path: "/a", responses: {}, pagination: noPagination },
        ], [], {}, testServer),
    ).toThrow(/operation_id/);
  });

  it("rejects duplicate operation_ids at construction time", () => {
    expect(
      () =>
        new Fixture([
          { operation_id: "dup", service_id: "s", name: "A", method: "GET", path: "/a", responses: {}, pagination: noPagination },
          { operation_id: "dup", service_id: "s", name: "B", method: "GET", path: "/b", responses: {}, pagination: noPagination },
        ], [], {}, testServer),
    ).toThrow(/duplicate operation_id/);
  });

  it("rejects exact physical and Unified name collisions", () => {
    expect(
      () => new Fixture(
        [{ operation_id: "sync", service_id: "s", name: "Sync", method: "POST", path: "/sync", responses: {}, pagination: noPagination }],
        [{ name: "sync", input_schema: {}, targets: [] }],
        {},
        testServer,
      ),
    ).toThrow(/collision/);
  });

  it("rejects physical operations without explicit pagination metadata", () => {
    expect(
      () => new Fixture([
        // @ts-expect-error -- omission must fail runtime admission as well as static typing.
        { operation_id: "missing", service_id: "s", name: "Missing", method: "GET", path: "/missing", responses: {} },
      ], [], {}, testServer),
    ).toThrow(/pagination metadata/);
  });

  it("rejects missing server metadata", () => {
    expect(() => new Fixture([], [], {}, undefined)).toThrow(/server metadata/);
  });
});

describe("loadFixture", () => {
  it("reads and parses a real fixture.json file", () => {
    const dir = mkdtempSync(path.join(tmpdir(), "fixture-test-"));
    const file = path.join(dir, "fixture.json");
    writeFileSync(
      file,
      JSON.stringify({
        server: {
          name: "mail-assistant",
          title: "Mail assistant",
          version: "1.2.3",
          description: "Read, search, summarize, draft, and send email through the connected mailbox.",
        },
        operations: [
          { operation_id: "x", service_id: "s", name: "X", method: "GET", path: "/x", responses: {}, pagination: { supported: true, caller_bound_supported: true, engine_max_pages: 12 } },
        ],
      }),
    );

    const f = loadFixture(file);
    expect(f.resolve("x")?.method).toBe("GET");
    expect(f.resolve("x")?.pagination).toEqual({ supported: true, caller_bound_supported: true, engine_max_pages: 12 });
    expect(f.server).toEqual({
      name: "mail-assistant",
      title: "Mail assistant",
      version: "1.2.3",
      description: "Read, search, summarize, draft, and send email through the connected mailbox.",
    });
  });

  it("throws a clear error when the operations array is missing", () => {
    const dir = mkdtempSync(path.join(tmpdir(), "fixture-test-"));
    const file = path.join(dir, "fixture.json");
    writeFileSync(file, JSON.stringify({}));

    expect(() => loadFixture(file)).toThrow(/operations/);
  });

  it("loads the shared Unified descriptor without reshaping its schemas", () => {
    const dir = mkdtempSync(path.join(tmpdir(), "fixture-test-"));
    const file = path.join(dir, "fixture.json");
    writeFileSync(file, JSON.stringify({
      server: testServer,
      operations: [],
      unified_operations: { schema_version: 3, operations: [{ name: "sync", input_schema: { type: "object" }, targets: [] }] },
    }));
    expect(loadFixture(file).resolveUnified("sync")?.input_schema).toEqual({ type: "object" });
  });
});
