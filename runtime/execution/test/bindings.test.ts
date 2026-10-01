import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import vm from "node:vm";
import { execFileSync } from "node:child_process";
import { test } from "node:test";
import { buildExecutionBundle, type SelectedOperation } from "../src/bundle";
import { generateExecutionBindingsDeclaration } from "../src/bindings";
import { operationAliases } from "../src/aliases";

const root = path.resolve(__dirname, "../..");
const selection: SelectedOperation = {
  service: "@public/stripe", operation: "GetCustomers",
  serviceId: "11111111-1111-4111-8111-111111111111", serviceVersionId: "22222222-2222-4222-8222-222222222222", endpointId: "33333333-3333-4333-8333-333333333333",
  inputSchema: { type: "object", properties: { email: { type: "string" } }, required: ["email"], additionalProperties: false },
  outputSchema: { type: "object", properties: { count: { type: "number" } }, required: ["count"], additionalProperties: false },
};

// Registry and compiler consume the same collision and reserved-name cases.
test("operation aliases match the Registry call-map contract", () => {
  const cases = JSON.parse(fs.readFileSync(path.join(root, "test/aliases.json"), "utf8"));
  for (const item of cases) {
    assert.deepEqual(Object.fromEntries(operationAliases(item.values, item.service)), item.expected);
    assert.deepEqual(Object.fromEntries(operationAliases([...item.values].reverse(), item.service)), item.expected);
  }
});

// Exercise the real bundler and bridge so method ergonomics cannot change provider identities or routing.
test("SDK-style methods preserve Engine dispatch, options and connected-user scope", async () => {
  const directory = fs.mkdtempSync(path.join(root, ".bindings-test-"));
  try {
    const source = `import {z} from "zod";
import {buildUnifiedApp} from "@fused/unified-app";
import {fused} from "@fused/operations";
export default buildUnifiedApp({input:z.object({email:z.string()}),output:z.object({count:z.number()}),
// All variants share the same selected operation and provider execution path.
async execute({input}) {
 await fused.stripe.getCustomers(input, {pagination:{maxPages:2}, selector:{environment:"prod"}});
 await fused.forUserRef("alice").stripe.getCustomers(input);
 const result = await fused.forServiceUserRefs({"@public/stripe":"bob"}).stripe.getCustomers(input);
 await fused.db.set({count:result.count});
 return result;
}});`;
    const entryFile = path.join(directory, "app.ts");
    fs.writeFileSync(entryFile, source);
    const bundle = await buildExecutionBundle({ entryFile, selectedOperations: [selection] });
    const requests: any[] = [];
    const sandbox: any = { __fusedHost: {
      // Collect only the bridge request; no provider credentials or network client exist in authored code.
      async fetch(raw: string) { requests.push(JSON.parse(raw)); return JSON.stringify({ count: 3 }); },
      // The facade must preserve the existing execution data helper as well.
      async dbSet(raw: string) { assert.equal(raw, '{"count":3}'); },
    } };
    vm.runInNewContext(bundle.code, sandbox, { timeout: 5000 });
    assert.deepEqual(JSON.parse(JSON.stringify(await sandbox.FusedUnifiedApp.execute({ input: { email: "test@example.com" } }))), { count: 3 });
    assert.equal(requests.length, 3);
    for (const request of requests) {
      assert.equal(request.service, selection.service);
      assert.equal(request.operation, selection.operation);
      assert.deepEqual(request.input, { email: "test@example.com" });
    }
    assert.deepEqual(requests[0].pagination, { maxPages: 2 });
    assert.equal(requests[0].selector.environment, "prod");
    assert.equal(requests[1].selector.endUserRef, "alice");
    assert.equal(requests[2].selector.endUserRef, "bob");
    assert.deepEqual(JSON.parse(JSON.stringify(sandbox.FusedExecutionManifest.selectedOperations[0])), { service: selection.service, operation: selection.operation, serviceId: selection.serviceId, serviceVersionId: selection.serviceVersionId, endpointId: selection.endpointId });
    // Provider failures must propagate unchanged and stop subsequent authored calls.
    sandbox.__fusedHost.fetch = async () => { throw new Error("provider failure"); };
    await assert.rejects(sandbox.FusedUnifiedApp.execute({ input: { email: "test@example.com" } }), /provider failure/);
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
});

// Compile a real authoring file to prove selected methods and option types remain discoverable and bounded.
test("SDK-style declarations reject incorrect inputs and unselected methods", () => {
  const directory = fs.mkdtempSync(path.join(root, ".bindings-test-"));
  try {
    fs.writeFileSync(path.join(directory, "operations.d.ts"), generateExecutionBindingsDeclaration([selection]));
    fs.writeFileSync(path.join(directory, "usage.ts"), `import {fused} from "@fused/operations";
fused.stripe.getCustomers({email:"user"}, {pagination:{maxPages:2}}).then(result => result.count.toFixed());
fused.forUserRef("alice").stripe.getCustomers({email:"user"});
// @ts-expect-error Inputs must match the reviewed provider schema.
fused.stripe.getCustomers({email:42});
// @ts-expect-error Unselected operations must not become available.
fused.stripe.deleteCustomer({email:"user"});
// @ts-expect-error Call options cannot override exact provider identity.
fused.stripe.getCustomers({email:"user"}, {service:"other"});
`);
    fs.writeFileSync(path.join(directory, "tsconfig.json"), JSON.stringify({ extends: path.join(root, "tsconfig.json"), compilerOptions: { noEmit: true }, include: ["usage.ts", "operations.d.ts"] }));
    execFileSync(process.execPath, [path.join(root, "node_modules/typescript/bin/tsc"), "-p", path.join(directory, "tsconfig.json")], { stdio: "pipe" });
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
});
