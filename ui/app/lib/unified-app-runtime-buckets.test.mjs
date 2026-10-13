import assert from "node:assert/strict";
import test from "node:test";
import { runtimeBucketNames, unifiedInvocationBody } from "./unified-app-runtime-buckets.ts";
import { readUnifiedEditorYAML, unifiedEditorYAML } from "./unified-app-yaml.ts";

// Disabled defaults must retain ordinary routing without manufacturing an explicit selector.
test("runtime bucket defaults stay static", () => {
  assert.deepEqual(runtimeBucketNames(undefined), []);
  assert.deepEqual(unifiedInvocationBody({ customer: "a" }, "", []), { operation: "execute", input: { customer: "a" } });
});
// A finite scope must reject wildcard, duplicate, whitespace and oversized configurations.
test("runtime bucket configuration is bounded and explicit", () => {
  for (const invalid of [null, "a", ["*"], [""], ["a", "a"], [" a"], Array(33).fill("a")]) assert.throws(() => runtimeBucketNames(invalid));
  assert.deepEqual(runtimeBucketNames(["tenant-a", "tenant-b"]), ["tenant-a", "tenant-b"]);
});
// Selected credentials belong in Engine controls rather than the authored input contract.
test("invocation sends only a permitted runtime bucket", () => {
  const buckets = [{ id: "bucket-a", name: "Tenant A" }];
  assert.deepEqual(unifiedInvocationBody({ a: 1 }, "bucket-a", buckets), { operation: "execute", input: { a: 1 }, bucket_id: "bucket-a" });
  assert.throws(() => unifiedInvocationBody({}, "bucket-b", buckets));
});
// Both browser editors must preserve the same reviewed scope when compiling new versions.
test("runtime bucket scope survives YAML round trips", () => {
  const draft = { name: "test", description: "Test app", source: "export default {};", services: {}, configSettings: { runtime_buckets: ["tenant-a"] } };
  const yaml = unifiedEditorYAML(draft, "test", "1.0.0", "default");
  assert.deepEqual(readUnifiedEditorYAML(yaml, draft).draft.configSettings.runtime_buckets, ["tenant-a"]);
  assert.throws(() => readUnifiedEditorYAML(yaml.replace("tenant-a", "'*'"), draft));
});
