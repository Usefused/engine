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

// One page must issue one direct query with server filters, even when the total spans many pages.
test("discovery sends filters once and returns only the requested server page", async () => {
  const calls = [];
  const page = {items:[{service_id:"stripe",service_name:"Stripe",slug:"route",label:"events"}],total:1000};
  const client = adapter({
    // Any attempted service catalogue request is a regression back to discovery fan-out.
    workspace: {getServicesPage: async () => { throw new Error("Unexpected service catalogue request"); }},
    // Record the direct Engine query without automatically fetching subsequent pages.
    mcpGraphql: async (query,variables,options) => { calls.push({query,variables,options}); return {workspaceWebhookPage:page}; },
  });
  const filters = {limit:20,offset:40,serviceId:"stripe",search:"invoice"};
  assert.deepEqual(await client.webhookListings(filters),page);
  assert.equal(calls.length,1);
  assert.deepEqual(calls[0].variables,filters);
  assert.match(calls[0].query,/workspaceWebhookPage/);
  assert.doesNotMatch(calls[0].query,/workspaceServices|auth_options/);
  assert.ok(calls[0].options.signal instanceof AbortSignal);
});

// A stale load must stop its network work rather than keep a service scan running after navigation.
test("discovery cancellation reaches the transport and pre-aborted loads issue no request", async () => {
  const controller = new AbortController();
  let calls = 0;
  const client = adapter({
    // Emulate fetch cancellation while leaving control of navigation with the test.
    mcpGraphql: async (_query,_variables,{signal}) => {
      calls++;
      return new Promise((_resolve,reject) => {
        // Fetch rejects once the shared page signal is cancelled.
        signal.addEventListener("abort",() => reject(signal.reason),{once:true});
      });
    },
  });
  const pending = client.webhookListings({limit:20,offset:0},controller.signal);
  controller.abort();
  await assert.rejects(pending,{name:"AbortError"});
  await assert.rejects(client.webhookListings({limit:20,offset:0},controller.signal),{name:"AbortError"});
  assert.equal(calls,1);
});
