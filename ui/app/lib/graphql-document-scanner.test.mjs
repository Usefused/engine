import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import process from "node:process";
import { spawnSync } from "node:child_process";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { RATE_LIMIT_GRAPHQL_FIELDS } from "./rate-limit.ts";
import { RETRY_GRAPHQL_FIELDS } from "./retry-policy.ts";

const uiRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const scanner = path.join(uiRoot, "scripts/extract-graphql-documents.mjs");
const manifest = path.join(uiRoot, "testdata/graphql-documents.json");
const registryManifest = path.resolve(uiRoot, "../../backend/internal/registry/graph/testdata/ui-graphql-documents.json");
let currentUIResult;

// runScanner executes the provider-free scanner with an optional fixture source root.
function runScanner(appRoot, args = []) {
  const env = { ...process.env };
  if (appRoot) env.FUSED_UI_GRAPHQL_APP_ROOT = appRoot;
  return spawnSync(process.execPath, [scanner, ...args], { cwd: uiRoot, env, encoding: "utf8" });
}

// scanCurrentUIResult scans once so contract assertions do not repeat compiler startup.
function scanCurrentUIResult() {
  currentUIResult ??= runScanner();
  assert.equal(currentUIResult.status, 0, currentUIResult.stderr);
  return currentUIResult;
}

// scanCurrentUI returns the deterministic document inventory for assertions below.
function scanCurrentUI() {
  return JSON.parse(scanCurrentUIResult().stdout);
}

// normalizedGraphQL compares projections independently of formatting.
function normalizedGraphQL(value) {
  return value.replace(/\s+/g, " ").trim();
}

test("matches the checked-in schema-validation manifest byte for byte", () => {
  const result = scanCurrentUIResult();
  const expected = fs.readFileSync(manifest, "utf8");
  if (result.stdout !== expected) {
    assert.fail("UI GraphQL manifest is stale; run `npm run graphql:manifest` and review the schema-test diff");
  }
});

test("keeps the Registry repo-boundary manifest synchronized when present", (t) => {
  if (!fs.existsSync(registryManifest)) {
    t.skip("sibling Registry checkout is not present");
    return;
  }
  const result = runScanner(undefined, ["--endpoint=registry"]);
  assert.equal(result.status, 0, result.stderr);
  if (result.stdout !== fs.readFileSync(registryManifest, "utf8")) {
    assert.fail("Registry UI GraphQL manifest is stale; run `npm run graphql:manifest` from the combined checkout");
  }
});

test("accounts for every current UI GraphQL call and document variant", () => {
  const scan = scanCurrentUI();
  assert.equal(scan.call_count, 83);
  assert.equal(scan.calls.length, 83);
  assert.equal(scan.document_count, 100);
  assert.equal(scan.documents.length, 100);
  assert.equal(scan.documents.filter(({ endpoint }) => endpoint === "registry").length, 18);
  assert.equal(scan.documents.filter(({ endpoint }) => endpoint === "engine").length, 82);
  assert.equal(scan.calls.reduce((count, call) => count + call.document_count, 0), 100);
});

test("resolves imported fragments and expands conditional and map variants", () => {
  const scan = scanCurrentUI();
  const serviceDocuments = scan.documents.filter(({ file, document }) => (
    file === "app/routes/integrations.$id.tsx" && document.includes("rate_limit")
  ));
  assert.ok(serviceDocuments.length >= 4);
  assert.ok(serviceDocuments.every(({ document }) => document.includes("identity { inputs")));
  assert.ok(serviceDocuments.every(({ document }) => document.includes("retry_config")));
  assert.equal(scan.calls.filter(({ file, document_count }) => (
    file === "app/routes/integrations.$id.tsx" && document_count === 2
  )).length, 2);
  assert.ok(scan.calls.some(({ file, document_count }) => file === "app/lib/people.ts" && document_count > 1));
  assert.ok(scan.calls.some(({ file, document_count }) => file === "app/lib/teams.ts" && document_count > 1));
});

test("keeps bucket connection roots independent across permission boundaries", () => {
  const documents = scanCurrentUI().documents.filter(({ file }) => file === "app/lib/buckets.ts");
  const connection = documents.find(({ document }) => /\bauthConnectionPage\s*\(/.test(document));
  const services = documents.find(({ document }) => /connectionServicePage:\s*bucketServicePage/.test(document));
  const summary = documents.find(({ document }) => /connectSummary:\s*bucketConnectSummary/.test(document));
  assert.ok(connection);
  assert.ok(services);
  assert.ok(summary);
  assert.doesNotMatch(connection.document, /bucketServicePage|bucketConnectSummary/);
  assert.doesNotMatch(services.document, /authConnectionPage|bucketConnectSummary/);
  assert.doesNotMatch(summary.document, /authConnectionPage|bucketServicePage/);
});

test("models runtime scalar interpolation without accepting structural interpolation", (t) => {
  const scan = scanCurrentUI();
  assert.ok(scan.documents.every(({ document }) => !document.includes("UI_DYNAMIC_VALUE")));
  assert.ok(scan.documents.some(({ file, document }) => (
    file === "app/routes/integrations.mcp.tsx" && document.includes("query($limit: Int!, $offset: Int!)")
  )));
  assert.ok(scan.documents.some(({ file, document }) => (
    file === "app/routes/integrations.mcp.tsx" && document.includes("deprecateApp(app_id: $appId")
  )));
  assert.ok(scan.documents
    .filter(({ file }) => file === "app/routes/integrations.mcp.tsx")
    .every(({ document }) => !document.includes("UI_DYNAMIC_VALUE")));

  const scalarRoot = fs.mkdtempSync(path.join(os.tmpdir(), "fused-ui-graphql-scalar-"));
  t.after(() => fs.rmSync(scalarRoot, { recursive: true, force: true }));
  fs.writeFileSync(path.join(scalarRoot, "scalar.ts"), `
    function run(id: string) {
      return api.graphql(\`query { service(id: "\${id}") { id } }\`);
    }
  `);
  const rejectedScalarResult = runScanner(scalarRoot);
  assert.notEqual(rejectedScalarResult.status, 0);
  assert.match(rejectedScalarResult.stderr, /runtime GraphQL interpolation must use variables/);
  const scalarResult = runScanner(scalarRoot, ["--allow-runtime-scalar-interpolation"]);
  assert.equal(scalarResult.status, 0, scalarResult.stderr);
  assert.ok(JSON.parse(scalarResult.stdout).documents[0].document.includes('id: "UI_DYNAMIC_VALUE"'));

  const fixtureRoot = fs.mkdtempSync(path.join(os.tmpdir(), "fused-ui-graphql-scan-"));
  t.after(() => fs.rmSync(fixtureRoot, { recursive: true, force: true }));
  fs.writeFileSync(path.join(fixtureRoot, "unresolved.ts"), `
    function run(field: string) {
      return api.graphql(\`query { service { \${field} } }\`);
    }
  `);
  const result = runScanner(fixtureRoot);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /unresolved structural template interpolation/);
});

test("keeps every Registry service policy document on the complete shared v3 projection", () => {
  const registryDocuments = scanCurrentUI().documents.filter(({ endpoint }) => endpoint === "registry");
  const rateDocuments = registryDocuments.filter(({ document }) => /\brate_limit\s*\{/.test(document));
  const retryDocuments = registryDocuments.filter(({ document }) => /\bretry_config\s*\{/.test(document));
  assert.ok(rateDocuments.length > 0);
  assert.ok(retryDocuments.length > 0);

  const rateProjection = normalizedGraphQL(`rate_limit { ${RATE_LIMIT_GRAPHQL_FIELDS} }`);
  const retryProjection = normalizedGraphQL(`retry_config { ${RETRY_GRAPHQL_FIELDS} }`);
  for (const { document } of rateDocuments) {
    const normalized = normalizedGraphQL(document);
    assert.ok(normalized.includes(rateProjection));
    assert.doesNotMatch(normalized, /\bscope\b|\bdefault_cost\b|\boperation_costs\b|\bresponse_headers\b|\bretry_after\b/);
  }
  for (const { document } of retryDocuments) {
    const normalized = normalizedGraphQL(document);
    assert.ok(normalized.includes(retryProjection));
    assert.doesNotMatch(normalized, /\bmax_retries\b|\bbackoff_ms\b|\bretry_after\b/);
  }
});

test("fails closed when UI code bypasses the owned GraphQL transports", (t) => {
  const fixtureRoot = fs.mkdtempSync(path.join(os.tmpdir(), "fused-ui-graphql-bypass-"));
  t.after(() => fs.rmSync(fixtureRoot, { recursive: true, force: true }));
  fs.writeFileSync(path.join(fixtureRoot, "bypass.ts"), `
    export function run() {
      return fetch("/engine/graphql", { method: "POST" });
    }
  `);
  const result = runScanner(fixtureRoot);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /direct GraphQL transport bypasses api\.graphql or api\.mcpGraphql/);
});
