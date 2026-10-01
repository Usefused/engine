import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";
import { buildExecutionBundle, type SelectedOperation } from "../src/bundle";

const selected: SelectedOperation = {
  service: "@public/stripe", operation: "GetCustomers",
  serviceId: "11111111-1111-4111-8111-111111111111", serviceVersionId: "22222222-2222-4222-8222-222222222222", endpointId: "33333333-3333-4333-8333-333333333333",
  inputSchema: { type: "object", properties: { email: { type: "string" } }, required: ["email"], additionalProperties: false },
  outputSchema: { type: "object", properties: { count: { type: "number" } }, required: ["count"], additionalProperties: false },
};

// Compile semantic and target-identity mistakes through the production admission path.
test("compilation rejects semantic errors before producing a bundle", async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "fused-typecheck-"));
  const entryFile = path.join(directory, "app.ts");
  const cases = [
    ["return { count: 'wrong' };", /TS2322/],
    ["return await fused.stripe.inventedOperation(input);", /inventedOperation/],
    ["return await fused.inventedService.getCustomers(input);", /inventedService/],
    ["await fused.fetch({service:'other',operation:'GetCustomers',input:{}}); return {count:1};", /other.GetCustomers is not selected/],
    ["await fused.fetch({service:'@public/stripe',operation:'DeleteCustomers',input:{}}); return {count:1};", /DeleteCustomers is not selected/],
    ["await fused.forUserRef('test').fetch({service:'other',operation:'GetCustomers',input:{}}); return {count:1};", /other.GetCustomers is not selected/],
    ["await fused.fetch({service:input.email,operation:'GetCustomers',input:{}}); return {count:1};", /Cannot verify dynamic/],
    ["const target={service:input.email}; await fused.fetch({service:'@public/stripe',operation:'GetCustomers',input:{},...target}); return {count:1};", /TS2783|Cannot verify dynamic/],
    ["return await fused.stripe.getCustomers({ email: 42 });", /TS2322/],
    ["return await fused.stripe.getCustomers({});", /email/],
    ["return { count: document.title };", /document/],
  ];
  try {
    for (const [body, expected] of cases) {
      fs.writeFileSync(entryFile, `import {z} from 'zod';\nimport {buildUnifiedApp} from '@fused/unified-app';\nimport {fused} from '@fused/operations';\nexport default buildUnifiedApp({input:z.object({email:z.string()}),output:z.object({count:z.number()}),async execute({input}) {${body}}});`);
      await assert.rejects(buildExecutionBundle({entryFile, selectedOperations:[selected]}), expected as RegExp);
    }
  } finally {
    fs.rmSync(directory, {recursive:true, force:true});
  }
});

// Reference directives must fail before the compiler follows local paths or loads ambient Node/browser types.
test("compilation rejects TypeScript reference directives", async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "fused-reference-check-"));
  const entryFile = path.join(directory, "app.ts");
  try {
    // Cover every reference kind that can expand the compiler filesystem or ambient-type graph.
    for (const directive of ['path="./private.d.ts"', 'types="node"', 'lib="dom"']) {
      fs.writeFileSync(entryFile, `/// <reference ${directive} />\nexport default {};`);
      await assert.rejects(buildExecutionBundle({ entryFile, selectedOperations: [selected] }), /cannot use TypeScript reference directives/);
    }
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
});
