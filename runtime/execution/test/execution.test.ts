import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import vm from "node:vm";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { z } from "zod";
import { buildUnifiedApp, fused, runUnifiedApp, toPublicSchema, validateSearchablePaths } from "../src/index";
import { buildExecutionBundle, inspectExecutionBundle } from "../src/bundle";
import { generateExecutionClientSource } from "../src/client_generator";
import { renderPublicType } from "../src/client_schema";
import { generateExecutionBindingsDeclaration } from "../src/bindings";

const selectedOperations = [{
  service: "crm", operation: "create", serviceId: "11111111-1111-4111-8111-111111111111",
  serviceVersionId: "22222222-2222-4222-8222-222222222222", endpointId: "33333333-3333-4333-8333-333333333333",
}];

// Verify the builder rejects unsafe or ambiguous search declarations.
test("searchable data paths are bounded and unambiguous", () => {
  assert.deepEqual(validateSearchablePaths(["customer.id"]), ["customer.id"]);
  assert.throws(() => validateSearchablePaths(["customer.id", "customer.id"]), /Duplicate/);
  assert.throws(() => validateSearchablePaths(["__proto__.id"]), /Invalid/);
  assert.throws(() => validateSearchablePaths(["customer..id"]), /Invalid/);
  assert.throws(() => validateSearchablePaths(Array.from({ length: 17 }, (_, i) => `field${i}`)), /at most 16/);
});

// Verify the local document cap and JSON validation before data reaches Engine.
test("fused.db rejects oversized or non-JSON data", async () => {
  let stored = "";
  globalThis.__fusedHost = {
    // Store the bridge payload exactly as Engine receives it.
    async fetch() { return "null"; },
    // Read the last serialized JSON document.
    async dbGet() { return stored || "null"; },
    // Capture the serialized JSON document.
    async dbSet(value) { stored = value; },
  };
  try {
    await fused.db.set({ value: "é" });
    assert.deepEqual(await fused.db.get(), { value: "é" });
    await assert.rejects(fused.db.set({ value: "x".repeat(512 * 1024) }), /exceeds/);
    await assert.rejects(fused.db.set({ value: Number.NaN }), /non-finite/);
  } finally {
    globalThis.__fusedHost = undefined;
  }
});

// Verify authored JavaScript cannot bypass the provider parameter object contract.
test("fused.fetch rejects non-object provider input", async () => {
  await assert.rejects(fused.fetch({ service: "crm", operation: "create", input: [] as unknown as Record<string, never> }), /JSON object/);
});

// Verify page bounds and a fixed connected-user reference reach the Engine bridge without repeated author inputs.
test("fused.fetch forwards pagination and a bound user ref", async () => {
  const requests: unknown[] = [];
  globalThis.__fusedHost = {
    // Capture the exact trusted-boundary request for the author's provider call.
    async fetch(requestJson) { requests.push(JSON.parse(requestJson)); return "null"; },
    // Provider-only calls do not read execution data in this test.
    async dbGet() { return "null"; },
    // Provider-only calls do not write execution data in this test.
    async dbSet() {},
  };
  try {
    const provider = fused.forUserRef("user-42");
    await provider.fetch({ service: "crm", operation: "list", input: {}, pagination: { maxPages: 2 }, selector: { authType: "oauth" } });
    assert.deepEqual(requests, [{
      service: "crm", operation: "list", input: {}, pagination: { maxPages: 2 },
      selector: { authType: "oauth", endUserRef: "user-42" },
    }]);
    await assert.rejects(provider.fetch({ service: "crm", operation: "list", input: {}, selector: { endUserRef: "another-user" } }), /bound user reference/);
    await assert.rejects(provider.fetch({ service: "crm", operation: "list", input: {}, pagination: { maxPages: 0 } }), /positive integer/);
    assert.throws(() => fused.forUserRef(" user-42 "), /unpadded/);
    assert.equal(requests.length, 1);
  } finally {
    globalThis.__fusedHost = undefined;
  }
});

// Verify one executor can use separate OAuth user references for separate selected services.
test("fused.fetch binds user refs by service", async () => {
  const requests: Array<{ service: string; selector: { endUserRef: string } }> = [];
  globalThis.__fusedHost = {
    // Capture each selected service and its connected-user routing identity.
    async fetch(requestJson) { requests.push(JSON.parse(requestJson)); return "null"; },
    // This test exercises provider routing without execution data reads.
    async dbGet() { return "null"; },
    // This test exercises provider routing without execution data writes.
    async dbSet() {},
  };
  try {
    const refs = { crm: "crm-user", billing: "billing-user" };
    const provider = fused.forServiceUserRefs(refs);
    refs.crm = "changed-after-binding";
    await provider.fetch({ service: "crm", operation: "customers.list", input: {} });
    await provider.fetch({ service: "billing", operation: "invoices.list", input: {} });
    assert.deepEqual(requests.map(({ service, selector }) => [service, selector.endUserRef]), [
      ["crm", "crm-user"], ["billing", "billing-user"],
    ]);
    await assert.rejects(provider.fetch({ service: "mail", operation: "send", input: {} }), /no user reference/);
    await assert.rejects(provider.fetch({ service: "crm", operation: "customers.list", input: {}, selector: { endUserRef: "billing-user" } }), /bound user reference/);
    assert.equal(requests.length, 2);
  } finally {
    globalThis.__fusedHost = undefined;
  }
});

// Verify the authored return value is parsed against its declared Zod output.
test("execution validates input and output", async () => {
  const app = buildUnifiedApp({
    input: z.object({ value: z.string() }),
    output: z.object({ value: z.number() }),
    // This deliberately violates the output contract to prove runtime validation.
    execute: async () => ({ value: "wrong" as unknown as number }),
  });
  await assert.rejects(runUnifiedApp(app, { value: 3 }), /expected string/);
  await assert.rejects(runUnifiedApp(app, { value: "valid" }), /expected number/);
});

// Verify public schemas cannot silently omit an authored custom validation rule.
test("public schema rejects unrepresentable Zod checks", () => {
  assert.throws(() => toPublicSchema(z.object({ value: z.string().refine((text) => text.length > 2) })), /custom Zod refinements/);
  assert.throws(() => toPublicSchema(z.object({ value: z.string().transform((text) => text.toUpperCase()) })), /Transforms cannot be represented/);
});

// Verify the one self-contained bundle exports one public manifest and app runtime.
test("bundle exports a singular Engine contract", async () => {
  const entryFile = path.resolve(__dirname, "../../test/fixture.ts");
  const bundle = await buildExecutionBundle({ entryFile, selectedOperations });
  assert.ok(bundle.code.length > 1000);
  const sandbox: Record<string, unknown> = { Promise, JSON, Object, Array, String, Number, Error, Set, Map, Symbol, Date, RegExp, encodeURIComponent };
  sandbox.globalThis = sandbox;
  let stored = "null";
  sandbox.__fusedHost = {
    // Return one workspace operation result via the JSON bridge.
    async fetch(requestJson: string) {
      assert.deepEqual(JSON.parse(requestJson), { service: "crm", operation: "create", input: { name: "Jane" }, selector: { environment: "prod" } });
      return JSON.stringify({ id: "cus_123" });
    },
    // Read the execution's single document.
    async dbGet() { return stored; },
    // Capture the data written by the authored execute function.
    async dbSet(valueJson: string) { stored = valueJson; },
  };
  vm.runInNewContext(bundle.code, sandbox, { timeout: 5000 });
  const manifest = sandbox.FusedExecutionManifest as { schemaVersion: number; searchable: string[]; selectedOperations: unknown[] };
  assert.equal(manifest.schemaVersion, 1);
  assert.deepEqual([...manifest.searchable], ["customerId"]);
  assert.equal(manifest.selectedOperations.length, 1);
  const app = sandbox.FusedUnifiedApp as { input: z.ZodType; output: z.ZodType; execute(context: { input: unknown }): Promise<unknown> };
  const parsedInput = app.input.parse({ name: "Jane" });
  const parsedOutput = app.output.parse(await app.execute({ input: parsedInput }));
  assert.deepEqual(JSON.parse(JSON.stringify(parsedOutput)), { customerId: "cus_123" });
  assert.deepEqual(JSON.parse(stored), { customerId: "cus_123" });
  const inspected = await inspectExecutionBundle(bundle.code, async () => JSON.parse(JSON.stringify(manifest)));
  assert.deepEqual(inspected.searchable, ["customerId"]);
});

// Verify selected provider targets are exact before bundle admission.
test("bundle rejects incomplete and non-UUID operation bindings", async () => {
  const entryFile = path.resolve(__dirname, "../../test/fixture.ts");
  await assert.rejects(buildExecutionBundle({ entryFile, selectedOperations: [] }), /at least one selected operation/);
  await assert.rejects(buildExecutionBundle({ entryFile, selectedOperations: Array.from({ length: 65 }, (_, index) => ({ ...selectedOperations[0], operation: `create${index}` })) }), /at most 64/);
  await assert.rejects(buildExecutionBundle({ entryFile, selectedOperations: [{ ...selectedOperations[0], serviceId: "" }] }), /incomplete/);
  await assert.rejects(buildExecutionBundle({ entryFile, selectedOperations: [{ ...selectedOperations[0], serviceId: "service-id" }] }), /non-UUID/);
});

// Verify Engine compilation never reads a tenant-selected local file or Node module into its bundle.
test("bundle rejects imports outside the pinned authoring modules", async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "fused-execution-imports-"));
  try {
    for (const imported of ["node:fs", "./private.json", "/etc/passwd", "other-package"]) {
      const entryFile = path.join(directory, "app.ts");
      fs.writeFileSync(entryFile, `import value from ${JSON.stringify(imported)};\nexport default value;\n`);
      // A compiler rejection must happen before an arbitrary import can become executable artifact bytes.
      await assert.rejects(buildExecutionBundle({ entryFile, selectedOperations }), /Unified App source cannot import/);
    }
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
});

// Verify manifest inspection rejects malformed evaluator results before persistence.
test("manifest inspection rejects invalid public data", async () => {
  await assert.rejects(inspectExecutionBundle("", async () => ({ schemaVersion: 2, selectedOperations: [] })), /Invalid/);
});

// Verify recursive and mixed Zod schemas remain safe generated TypeScript types.
test("public type renderer preserves recursive references", () => {
  const declaration = renderPublicType({ type: "object", properties: { next: { $ref: "#" } } }, "NodeOutput");
  assert.match(declaration, /"next"\?: NodeOutput/);
  const mixed = renderPublicType({ type: "object", properties: { id: { type: "string" } }, additionalProperties: { type: "number" } }, "MixedOutput");
  assert.match(mixed, /"id"\?: string/);
  assert.match(mixed, /\[key: string\]: unknown/);
});

// Verify reviewed operation hints type methods while the immutable manifest keeps only IDs.
test("selected operation methods compile and omit local schema hints", async () => {
  const entryFile = path.resolve(__dirname, "../../examples/live-greeting.ts");
  const spec = JSON.parse(fs.readFileSync(path.resolve(__dirname, "../../examples/live-greeting.spec.json"), "utf8"));
  const bundle = await buildExecutionBundle({ entryFile, selectedOperations: spec.selectedOperations });
  const sandbox: Record<string, unknown> = { Promise, JSON, Object, Array, String, Number, Error, Set, Map, Symbol, Date, RegExp, encodeURIComponent };
  sandbox.globalThis = sandbox;
  let stored = "null";
  sandbox.__fusedHost = {
    // The typed method still carries only its selected author keys to Engine.
    async fetch(requestJson: string) {
      assert.deepEqual(JSON.parse(requestJson), { service: "greeting", operation: "greet", input: { name: "Jane" } });
      return JSON.stringify({ greeting: "Hello Jane" });
    },
    // The example stores one searchable field in the execution document.
    async dbGet() { return stored; },
    // Preserve the authored document for the result assertion.
    async dbSet(valueJson: string) { stored = valueJson; },
  };
  vm.runInNewContext(bundle.code, sandbox, { timeout: 5000 });
  const app = sandbox.FusedUnifiedApp as { execute(context: { input: unknown }): Promise<unknown> };
  assert.deepEqual(JSON.parse(JSON.stringify(await app.execute({ input: { name: "Jane" } }))), { greeting: "Hello Jane" });
  assert.deepEqual(JSON.parse(stored), { name: "Jane" });
  const manifest = sandbox.FusedExecutionManifest as { selectedOperations: Array<Record<string, unknown>> };
  assert.deepEqual(Object.keys(manifest.selectedOperations[0]).sort(), ["endpointId", "operation", "service", "serviceId", "serviceVersionId"]);
  const declaration = generateExecutionBindingsDeclaration(spec.selectedOperations);
  assert.match(declaration, /"greet": \(input: Operation0Input, options\?: OperationOptions\) => Promise<Operation0Output>/);
  const compiler = path.resolve(__dirname, "../../node_modules/typescript/bin/tsc");
  execFileSync(process.execPath, [compiler, "-p", path.resolve(__dirname, "../../examples/tsconfig.json")]);
});

// Verify compilation emits exact bytes and bindings without executing authored declarations.
test("CLI compiles without evaluating top-level source", () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "fused-execution-test-"));
  const specFile = path.join(directory, "spec.json");
  const bundleFile = path.join(directory, "bundle.js");
  const manifestFile = path.join(directory, "manifest.json");
  const clientFile = path.join(directory, "client.ts");
  const bindingsFile = path.join(directory, "operations.d.ts");
  try {
    const sourceFile = path.join(directory, "app.ts");
    fs.writeFileSync(sourceFile, `throw new Error("top-level code must not execute in Node");\n` + fs.readFileSync(path.resolve(__dirname, "../../test/fixture.ts"), "utf8"));
    fs.writeFileSync(specFile, JSON.stringify({ entryFile: sourceFile, selectedOperations }));
    execFileSync(process.execPath, [path.resolve(__dirname, "../src/cli.js"), "--config", specFile, "--out", bundleFile, "--bundle-only", "true", "--bindings", bindingsFile]);
    assert.ok(fs.readFileSync(bundleFile, "utf8").includes("FusedUnifiedApp"));
    assert.equal(fs.existsSync(manifestFile), false);
    const digest = JSON.parse(fs.readFileSync(bundleFile + ".digest.json", "utf8")).bundle_digest;
    assert.equal(digest, "sha256:" + createHash("sha256").update(fs.readFileSync(bundleFile)).digest("hex"));
    assert.equal(fs.existsSync(clientFile), false);
    // Without a supported Engine inspector the CLI must fail, never evaluate source in Node.
    assert.throws(() => execFileSync(process.execPath, [path.resolve(__dirname, "../src/cli.js"), "--config", specFile, "--out", bundleFile, "--manifest", manifestFile, "--inspector", path.join(directory, "missing-inspector")], { stdio: "pipe" }), /Confined manifest inspection failed/);
    assert.equal(fs.existsSync(manifestFile), false);
    assert.ok(fs.readFileSync(bindingsFile, "utf8").includes("declare module \"@fused/operations\""));
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
});

// Verify generated client types and lifecycle methods use the shared Engine REST route.
test("generated client compiles and routes execution lifecycle calls", async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "fused-client-test-"));
  try {
    const source = generateExecutionClientSource({
      schemaVersion: 1,
      inputSchema: toPublicSchema(z.object({ name: z.string(), company: z.string().optional() })),
      outputSchema: toPublicSchema(z.object({ customerId: z.string() })),
      searchable: ["customerId"], selectedOperations,
    });
    const clientFile = path.join(directory, "client.ts");
    const usageFile = path.join(directory, "usage.ts");
    fs.writeFileSync(clientFile, source);
    fs.writeFileSync(usageFile, `import { createFusedExecutionClient } from "./client";\nconst client = createFusedExecutionClient({baseUrl:"http://engine",appFamilyId:"app",token:"token"});\nclient.execute({name:"Jane"});\nclient.search({"data.customerId":"cus_123"});\n// @ts-expect-error Invalid input must be rejected by the generated contract.\nclient.execute({name:42});\n// @ts-expect-error Unlisted data fields are not searchable.\nclient.search({"data.unlisted":"x"});\n`);
    const compiler = path.resolve(__dirname, "../../node_modules/typescript/bin/tsc");
    execFileSync(process.execPath, [compiler, "--ignoreConfig", "--strict", "--target", "ES2020", "--module", "CommonJS", "--moduleResolution", "node", "--ignoreDeprecations", "6.0", "--lib", "ES2020", "--outDir", path.join(directory, "build"), clientFile, usageFile]);
    const generated = require(path.join(directory, "build/client.js")) as { createFusedExecutionClient(options: unknown): any };
    const calls: Array<{ url: string; init: { method: string; headers: Record<string, string>; body?: string } }> = [];
    // Mock the public execution envelope without the private stored search document.
    const client = generated.createFusedExecutionClient({ baseUrl: "http://engine/", appFamilyId: "app-id", token: "secret", async fetcher(url: string, init: { method: string; headers: Record<string, string>; body?: string }) {
      calls.push({ url, init });
      // Search is the only route whose body is a page of records.
      const payload = url.includes("?where=") ? { items: [] } : { executionId: "run-id", appFamilyId: "app-id", version: "v1", status: "succeeded", mode: "live", readHandle: "a".repeat(64), output: { customerId: "cus_123" }, createdAt: "2026-09-28T00:00:00Z" };
      return { ok: true, status: 200, async json() { return payload; } };
    } });
    await client.execute({ name: "Jane" });
    await client.fetch("run-id", "a".repeat(64));
    await client.search({ "data.customerId": "cus_123" }, 10);
    await client.replay("run-id", "a".repeat(64));
    await client.rerun("run-id", "a".repeat(64), "new-key");
    assert.equal(calls.length, 5);
    assert.equal(calls[0].url, "http://engine/v1/apps/app-id/executions");
    assert.equal(calls[0].init.headers.Authorization, "Bearer secret");
    assert.equal(calls[0].init.body, JSON.stringify({ operation: "execute", input: { name: "Jane" } }));
    assert.equal(calls[1].init.headers["X-Execution-Read-Handle"], "a".repeat(64));
    assert.match(calls[2].url, /\?where=/);
    assert.ok(calls[3].url.endsWith("/replay"));
    assert.equal(calls[4].init.headers["Idempotency-Key"], "new-key");
    const denied = generated.createFusedExecutionClient({ baseUrl: "http://engine", appFamilyId: "app-id", token: "secret", async fetcher() {
      // Preserve the Engine's bounded public error code without copying private details.
      return { ok: false, status: 403, async json() { return { error: { code: "access_denied", message: "search denied" } }; } };
    } });
    await assert.rejects(denied.search({}), (error: unknown) => error instanceof Error && (error as { code?: string }).code === "access_denied");
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
});

// Source-map identities must remain stable across temporary compile directories and omit private source text.
test("bundles retain stable private TypeScript source maps", async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "fused-diagnostic-map-"));
  const source = `import {z} from 'zod';
import {buildUnifiedApp} from '@fused/unified-app';
export default buildUnifiedApp({input:z.object({}),output:z.object({}),fetch:{searchable:[]},
async execute(){throw new Error('private map sentinel');}});`;
  try {
    fs.mkdirSync(path.join(directory, "a")); fs.mkdirSync(path.join(directory, "b"));
    const firstPath = path.join(directory, "a", "app.ts");
    const secondPath = path.join(directory, "b", "app.ts");
    fs.writeFileSync(firstPath, source); fs.writeFileSync(secondPath, source);
    const first = await buildExecutionBundle({entryFile:firstPath,selectedOperations});
    const second = await buildExecutionBundle({entryFile:secondPath,selectedOperations});
    assert.equal(first.bundleDigest,second.bundleDigest);
    const encoded = first.code.match(/sourceMappingURL=data:application\/json;base64,([^\s]+)/)?.[1];
    assert.ok(encoded);
    const map = JSON.parse(Buffer.from(encoded,"base64").toString("utf8"));
    assert.ok(map.sources.includes("unified-app.ts"));
    assert.equal(map.sourcesContent,undefined);
    assert.ok(!JSON.stringify(map).includes(directory));
  } finally {
    fs.rmSync(directory,{recursive:true,force:true});
  }
});
