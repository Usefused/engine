import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";
import { webcrypto } from "node:crypto";
import ts from "typescript";
import * as contract from "./webhook-discovery-contract.ts";
import { readAllBoundedPages } from "./bounded-pages.ts";

/** Runs the production adapter against a recorded transport instead of provisioning real ingress. */
function adapter(api) {
  const source = readFileSync(new URL("./webhook-discovery-api.ts", import.meta.url), "utf8");
  const code = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText;
  const exports = {};
  /** Reject dependencies outside this explicit test boundary. */
  function require(name) {
    // The actual transport is replaced so tests cannot modify the user's Engine.
    if (name === "./api") return { api };
    // Validation and pagination still execute production implementations.
    if (name === "./webhook-discovery-contract") return contract;
    if (name === "./bounded-pages") return { readAllBoundedPages };
    throw new Error(`Unexpected dependency ${name}`);
  }
  new Function("require", "exports", "crypto", code)(require, exports, webcrypto);
  return exports;
}
const draft = { name: "stripe-events", service: "stripe", secret: "${bucket.default.secret.signing}", baseURL: "https://engine.example" };

// Canonical service tags must remain accurate across Registry-backed and older cached service metadata.
test("service tags preserve provider identity and are searchable", () => {
  assert.equal(contract.webhookServiceTag({service_name:"Payments",canonical_ref:"@stripe/payments",service_slug:"legacy"}),"@stripe/payments");
  assert.equal(contract.webhookServiceTag({service_name:"Payments",service_slug:"@stripe/payments"}),"@stripe/payments");
  assert.equal(contract.webhookServiceTag({service_name:"Payments",service_slug:"payments",provider:{handle:"stripe"}}),"@stripe/payments");
  assert.equal(contract.webhookServiceTag({service_name:"Payments",service_slug:"payments"}),"Payments");
  assert.equal(contract.matchesWebhook({label:"Orders",service_name:"Payments",service_ref:"@stripe/payments",callback_url:"",slug:"orders"},"@stripe/"),true);
});

// The browser must share named config identity and the exact reviewed hash with CLI APIs.
test("webhook creation plans first and applies only the reviewed receipt", async () => {
  const calls = [];
  const api = { appConfig: {
    /** Record planning without implicitly applying it. */
    plan: async (kind, input) => { calls.push({kind,input}); return {plan_id:"plan",source_hash:input.source_hash,summary:{create_webhook:true}}; },
    /** Observe the exact immutable receipt sent to apply. */
    apply: async (kind, input) => { calls.push({kind,input}); },
  }};
  const client = adapter(api), plan = await client.planWebhook(draft,"ops");
  assert.equal(calls.length,1);
  assert.equal(calls[0].kind,"webhook");
  assert.equal(calls[0].input.config_key,"webhook:stripe-events");
  assert.equal(calls[0].input.owner_team,"ops");
  assert.equal(calls[0].input.config.services.stripe.secret,draft.secret);
  await client.createWebhook(plan);
  assert.deepEqual(calls[1],{kind:"webhook",input:{plan_id:"plan",source_hash:plan.source_hash}});
});

// Creation cannot accidentally reconcile an existing registration bundle to one service.
test("creation rejects an existing webhook name", async () => {
  const client = adapter({appConfig:{plan: async () => ({summary:{create_webhook:false}})}});
  await assert.rejects(client.planWebhook(draft,""),/already exists/);
});

// The UI never offers a managed receiver, credential-bearing URL, or relative path as a provider address.
test("only direct HTTP destinations are copyable", () => {
  const registration = {delivery_mode:"direct",callback_url:"https://engine.example/webhook/stripe"};
  assert.equal(contract.copyableWebhookURL(registration),registration.callback_url);
  for (const change of [{delivery_mode:"managed"},{callback_url:"/webhook/stripe"},{callback_url:"javascript:alert(1)"},{callback_url:"https://user:secret@example.com"}]) {
    assert.equal(contract.copyableWebhookURL({...registration,...change}),null);
  }
});

// Provider routing and secret references must be explicit, with the Engine retaining final validation authority.
test("configuration rejects raw signing secrets and unsafe destinations", () => {
  assert.throws(() => contract.webhookConfiguration({...draft,secret:"raw-signing-secret"}),/reference/);
  for (const baseURL of ["", "http://public.example", "https://user:secret@engine.example", "https://engine.example?token=x"]) {
    assert.throws(() => contract.webhookConfiguration({...draft,baseURL}));
  }
  assert.equal(contract.webhookConfiguration({...draft,secret:""}).services.stripe.secret,undefined);
});

// Bounded fan-out must report partial failure rather than incorrectly showing an empty catalogue.
test("discovery retains readable registrations and identifies failed services", async () => {
  const client=adapter({mcpGraphql: async (_query, variables) => {
    // One denied service must not discard another service's permitted receiving URLs.
    if (variables.serviceId === "denied") throw new Error("denied");
    return {workspaceWebhooks:[{label:"events",slug:"route",callback_url:"https://engine.example/webhook/route",delivery_mode:"direct",signature:"set",created_at:""}]};
  }});
  const result=await client.webhookListings([{service_id:"ok",service_name:"Stripe"},{service_id:"denied",service_name:"GitHub"}]);
  assert.equal(result.items.length,1); assert.equal(result.items[0].service_name,"Stripe"); assert.deepEqual(result.failed,["GitHub"]);
});
