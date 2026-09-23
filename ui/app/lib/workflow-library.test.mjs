import test from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import {
  composeWorkflowApp,
  workflowDependencies,
  decodeWorkflow,
  workflowSelectionURL,
} from "./workflow-library.ts";

// Fixture workflows share one exact provider version while exposing distinct logical methods.
function workflow(id, method, physical = method, version = "v1") {
  return {
    id,
    publisher: "example",
    hash: `sha256:${"a".repeat(64)}`,
    public: true,
    template: {
      schema_version: 1,
      slug: method,
      version: "1.0.0",
      name: method,
      description: "Fixture",
      category: "Support",
      requirements: [],
      services: {
        "@example/issues": {
          service_id: "service",
          service_version_id: `version-${version}`,
          version,
          operations: [physical],
        },
      },
      unified_operations: {
        [method]: {
          input: { type: "object" },
          bindings: {
            issue: { service: "@example/issues", operation: physical },
          },
        },
      },
    },
  };
}

// The base includes private routing so extension tests detect accidental reconstruction or erasure.
function base() {
  return {
    apiVersion: "fused/v1",
    kind: "sdk",
    name: "support",
    version: "2.0.0",
    language: "python",
    bucket: "private",
    services: {
      "@example/issues": {
        version: "v1",
        operations: ["existing"],
        auth: { type: "oauth", name: "custom" },
        injections: [
          {
            location: "server_variable",
            name: "tenant",
            value: "${bucket.values.tenant}",
          },
        ],
      },
    },
  };
}

// Composition must preserve the existing app while combining least-privilege dependencies.
test("multiple workflows share a provider and preserve existing routing", () => {
  const original = base();
  const config = composeWorkflowApp(original, [
    workflow("a", "create"),
    workflow("b", "read"),
  ]);
  assert.deepEqual(config.services["@example/issues"].operations, [
    "create",
    "existing",
    "read",
  ]);
  assert.deepEqual(
    config.services["@example/issues"].auth,
    original.services["@example/issues"].auth
  );
  assert.deepEqual(
    config.services["@example/issues"].injections,
    original.services["@example/issues"].injections
  );
  assert.equal(config.bucket, "private");
  assert.equal(config.language, "python");
  assert.equal(config.workflow_sources.length, 2);
  assert.equal(original.unified_operations, undefined);
});

// Exact repeats must be idempotent and conflicting definitions must stop before workspace mutation.
test("composition rejects incompatible pins and methods and accepts an identical reinstall", () => {
  const selected = workflow("a", "create");
  const config = composeWorkflowApp(base(), [selected]);
  assert.deepEqual(composeWorkflowApp(config, [selected]), config);
  assert.throws(
    () => composeWorkflowApp(base(), [workflow("b", "read", "get", "v2")]),
    /different version/
  );
  assert.throws(
    () => workflowDependencies([selected, workflow("b", "read", "get", "v2")]),
    /incompatible versions/
  );
  assert.throws(
    () =>
      composeWorkflowApp(base(), [selected, workflow("b", "create", "delete")]),
    /conflicts/
  );
});

// Corrupt Registry content cannot enter the installer's trusted composition state.
test("release digest verifies exact UTF-8 authoring bytes", async () => {
  const template = JSON.stringify(workflow("a", "create").template);
  const release = {
    id: "a",
    publisher: "example",
    public: true,
    hash: `sha256:${createHash("sha256").update(template).digest("hex")}`,
    template,
  };
  assert.equal((await decodeWorkflow(release)).template.slug, "create");
  await assert.rejects(
    decodeWorkflow({
      ...release,
      template: template.replace("create", "delete"),
    }),
    /verification failed/
  );
});

// Runtime limits and selection URLs must be deterministic across browsing order.
test("selection remains bounded and duplicate URL selections collapse", () => {
  assert.throws(() => composeWorkflowApp(base(), []), /Select between/);
  assert.throws(
    () =>
      composeWorkflowApp({ ...base(), language: "go" }, [
        workflow("a", "create"),
      ]),
    /TypeScript or Python/
  );
  assert.equal(
    workflowSelectionURL("/install", ["a", "b", "a"]),
    "/install?workflow=a&workflow=b"
  );
});
