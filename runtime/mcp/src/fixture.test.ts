import { writeFileSync } from "node:fs";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { describe, expect, it } from "vitest";
import { Fixture, loadFixture } from "./fixture.js";

describe("Fixture", () => {
  it("resolves a registered operationId", () => {
    const f = new Fixture([
      { operation_id: "a", service_id: "s", name: "A", method: "GET", path: "/a", responses: {} },
    ]);
    expect(f.resolve("a")?.path).toBe("/a");
  });

  it("does not resolve an unregistered operationId -- tier 1 enforcement point", () => {
    const f = new Fixture([
      { operation_id: "a", service_id: "s", name: "A", method: "GET", path: "/a", responses: {} },
    ]);
    expect(f.resolve("b")).toBeUndefined();
  });

  it("rejects an operation with no operation_id at construction time", () => {
    expect(
      () =>
        new Fixture([
          // @ts-expect-error -- deliberately malformed for the test
          { service_id: "s", name: "A", method: "GET", path: "/a", responses: {} },
        ]),
    ).toThrow(/operation_id/);
  });

  it("rejects duplicate operation_ids at construction time", () => {
    expect(
      () =>
        new Fixture([
          { operation_id: "dup", service_id: "s", name: "A", method: "GET", path: "/a", responses: {} },
          { operation_id: "dup", service_id: "s", name: "B", method: "GET", path: "/b", responses: {} },
        ]),
    ).toThrow(/duplicate operation_id/);
  });

  it("rejects exact physical and Unified name collisions", () => {
    expect(
      () => new Fixture(
        [{ operation_id: "sync", service_id: "s", name: "Sync", method: "POST", path: "/sync", responses: {} }],
        [{ name: "sync", input_schema: {}, targets: [] }],
      ),
    ).toThrow(/collision/);
  });
});

describe("loadFixture", () => {
  it("reads and parses a real fixture.json file", () => {
    const dir = mkdtempSync(path.join(tmpdir(), "fixture-test-"));
    const file = path.join(dir, "fixture.json");
    writeFileSync(
      file,
      JSON.stringify({
        operations: [
          { operation_id: "x", service_id: "s", name: "X", method: "GET", path: "/x", responses: {} },
        ],
      }),
    );

    const f = loadFixture(file);
    expect(f.resolve("x")?.method).toBe("GET");
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
      operations: [],
      unified_operations: { schema_version: 3, operations: [{ name: "sync", input_schema: { type: "object" }, targets: [] }] },
    }));
    expect(loadFixture(file).resolveUnified("sync")?.input_schema).toEqual({ type: "object" });
  });
});
