import test from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import {
  composeWorkflowApp,
  composeBuilderWorkflows,
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

// Manual operations and workflow dependencies on the same exact version share one provider entry.
test("builder combines physical selections and workflows without losing operations", () => {
  const physical = base();
  physical.services.issues = physical.services["@example/issues"];
  delete physical.services["@example/issues"];
  const result = composeBuilderWorkflows(physical, [workflow("a", "create")], [{ key: "issues", service_id: "service", service_version_id: "version-v1" }]);
  assert.deepEqual(Object.keys(result.services), ["@example/issues"]);
  assert.deepEqual(result.services["@example/issues"].operations, ["create", "existing"]);
  assert.equal(result.services["@example/issues"].auth.name, "custom");
  assert.ok(physical.services.issues);
});

// Immutable provider IDs are authoritative even if human-facing version labels match.
test("builder rejects a physical snapshot that conflicts with a workflow pin", () => {
  assert.throws(() => composeBuilderWorkflows(base(), [workflow("a", "create")], [{ key: "@example/issues", service_id: "service", service_version_id: "different-id" }]), /Selected service version conflicts/);
});

// Ordinary builds do not acquire workflow metadata or extra permission requirements.
test("builder leaves service-only configs untouched", () => {
  const config = base();
  assert.equal(composeBuilderWorkflows(config, [], []), config);
});

// Two existing aliases must never be collapsed by silently replacing private routing.
test("builder rejects ambiguous aliases instead of overwriting routing", () => {
  const config = base();
  config.services.issues = { version: "v1", operations: ["other"] };
  assert.throws(() => composeBuilderWorkflows(config, [workflow("a", "create")], [{ key: "issues", service_id: "service", service_version_id: "version-v1" }]), /Conflicting service aliases/);
});

// Engine-owned alias pins repair older display-name state without touching private expressions or step namespaces.
test("builder extends saved source with display-name and authored graph aliases", () => {
  const original = base();
  original.services["Issue tracker"] = original.services["@example/issues"];
  delete original.services["@example/issues"];
  original.unified_operations = {
    existing: { bindings: {
      issues: { operation: "existing", input: { token: "${bucket.values.private}" }, rollback: { operation: "undo" } },
      later: { service: "issues", operation: "existing", depends_on: ["issues"], input: { id: "${results.issues.id}" } },
    } },
  };
  const pins = ["Issue tracker", "issues"].map((key) => ({ key, service_id: "service", service_version_id: "version-v1" }));
  const result = composeBuilderWorkflows(original, [workflow("a", "create")], pins);
  assert.deepEqual(Object.keys(result.services), ["@example/issues"]);
  assert.deepEqual(result.services["@example/issues"].auth, original.services["Issue tracker"].auth);
  const bindings = result.unified_operations.existing.bindings;
  assert.equal(bindings.issues.service, "@example/issues");
  assert.equal(bindings.later.service, "@example/issues");
  assert.deepEqual(bindings.later.depends_on, ["issues"]);
  assert.equal(bindings.later.input.id, "${results.issues.id}");
  assert.deepEqual(bindings.issues.rollback, { operation: "undo" });
  assert.equal(bindings.issues.input.token, "${bucket.values.private}");
  assert.equal(original.unified_operations.existing.bindings.issues.service, undefined);
});

// A workflow on one provider must not invalidate existing graphs on another saved provider.
test("builder preserves unrelated saved graph selectors during an addition", () => {
  const original = base();
  original.services.Other = { version: "v1", operations: ["read"] };
  original.unified_operations = { other: { bindings: { read: { service: "@other/api", operation: "read" } } } };
  const result = composeBuilderWorkflows(original, [workflow("a", "create")], [
    { key: "Other", service_id: "other", service_version_id: "other-version" },
    { key: "@other/api", service_id: "other", service_version_id: "other-version" },
  ]);
  assert.equal(result.unified_operations.other.bindings.read.service, "Other");
  assert.deepEqual(result.services.Other, original.services.Other);
  assert.equal(original.unified_operations.other.bindings.read.service, "@other/api");
});
