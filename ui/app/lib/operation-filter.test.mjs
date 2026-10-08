import assert from "node:assert/strict";
import test from "node:test";
import { filterOperationMethods } from "./operation-filter.ts";

// Method matching must not confuse a path or description containing another HTTP verb with its type.
test("operation type filter matches exact methods case insensitively", () => {
  const operations = [{ id: "read", method: "get", path: "/post" }, { id: "write", method: "POST" }, { id: "missing" }, { id: "graphql", method: "GRAPHQL" }];
  assert.deepEqual(filterOperationMethods(operations, "GET"), [operations[0]]);
  assert.deepEqual(filterOperationMethods(operations, "GRAPHQL"), [operations[3]]);
  assert.deepEqual(filterOperationMethods(operations, "DELETE"), []);
  assert.equal(filterOperationMethods(operations, "all"), operations);
});

// Search filtering and pagination can apply the same predicate independently without mutating source rows.
test("operation filtering retains page order and version identity", () => {
  const operations = [{ id: "one", method: "GET", version: "v1" }, { id: "two", method: "POST", version: "v1" }, { id: "three", method: "GET", version: "v2" }];
  const snapshot = structuredClone(operations);
  assert.deepEqual(filterOperationMethods(operations, "GET").map((item) => item.id), ["one", "three"]);
  assert.deepEqual(operations, snapshot);
});
