import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";
import { webcrypto } from "node:crypto";
import * as contract from "./unified-app-contract.ts";
import * as describeContract from "./app-describe-contract.ts";
import * as authorization from "./authorization-error.ts";

// Load the real client and typed errors against a recorded transport without invoking a model or mutating an Engine.
function client(mock, module = "unified-app-api") {
  const source = readFileSync(new URL(`./${module}.ts`, import.meta.url), "utf8");
  const code = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText;
  const exports = {};
  // Only the production transport and pure contract module may cross this test boundary.
  const require = (name) => {
    // All authoring modules share the same recorded transport.
    if (name === "./app-describe-api") return client(mock, "app-describe-api");
    // Pure selection rules remain production code in these integration-style tests.
    if (name === "./app-describe-contract") return describeContract;
    if (name === "./api") return { api: mock };
    if (name === "./unified-app-contract") return contract;
    // Recovery must recognize the same typed error emitted by the production transport.
    if (name === "./authorization-error") return authorization;
    throw new Error(`Unexpected dependency ${name}`);
  };
  new Function("require", "exports", "crypto", code)(require, exports, webcrypto);
  return exports;
}
const source = 'export default buildUnifiedApp({ input: z.object({}), output: z.object({}), async execute() { return {}; } });';
const service = { service_id: "service-id", service_version_id: "version-id", version: "v1", operations: ["getCustomer"] };

// Editing uses the existing source and exact reviewed pins without rediscovering services or deploying anything.
test("AI source revision preserves the baseline and selected provider contracts", async () => {
  const mock = transport({ RevisePromptUnifiedApp: { draftPromptUnifiedApp: JSON.stringify({ source: source + "\n// revised" }) } });
  const result = await client(mock.api, "app-describe-api").draftAppSource("Fix the checkout error", { stripe: service }, () => {}, source);
  assert.equal(result, source + "\n// revised");
  assert.equal(mock.calls.length, 1);
  assert.equal(mock.calls[0].operation, "RevisePromptUnifiedApp");
  assert.equal(mock.calls[0].variables.source, source);
  assert.deepEqual(JSON.parse(mock.calls[0].variables.selections), [{ service: "stripe", service_id: service.service_id, version: "v1", operation: "getCustomer" }]);
});

// Failed or oversized revisions must leave the caller's draft unchanged and avoid unbounded paid requests.
test("AI source revision rejects oversized context and preserves clarification", async () => {
  const mock = transport({ RevisePromptUnifiedApp: { draftPromptUnifiedApp: JSON.stringify({ source: "", clarification: "Select the price lookup operation first." }) } });
  const drafter = client(mock.api, "app-describe-api");
  await assert.rejects(drafter.draftAppSource("Fix", { stripe: service }, () => {}, "x".repeat(128 * 1024 + 1)), /128 KiB/);
  assert.equal(mock.calls.length, 0);
  await assert.rejects(drafter.draftAppSource("Fix", { stripe: service }, () => {}, source), /Select the price lookup/);
});

// Runtime diagnoses must not become red generation errors or empty replacements for valid app code.
test("AI review accepts findings without changing valid Stripe array input", async () => {
  const explanation = "line_items matches the selected array contract. The pasted serialization error occurs before dispatch. Check the deployed Engine version and retry with its form-array fix.";
  const mock = transport({ RevisePromptUnifiedApp: { draftPromptUnifiedApp: JSON.stringify({ source: "", clarification: "", explanation }) } });
  const result = await client(mock.api, "app-describe-api").reviseAppSource("Investigate: deepObject parameter requires an object", { stripe: service }, () => {}, source);
  assert.deepEqual(result, { source: "", explanation });
  assert.equal(mock.calls.length, 1);
  assert.equal(mock.calls[0].variables.source, source);
  assert.match(mock.calls[0].variables.q, /deepObject/);
});

// Creation still needs executable source; optional review prose cannot weaken the source or response bounds.
test("AI review distinguishes changes, questions, malformed replies, and initial creation", () => {
  assert.deepEqual(contract.decodeUnifiedSourceRevision(JSON.stringify({ source, explanation: "Validated provider response fields." })), { source, explanation: "Validated provider response fields." });
  assert.deepEqual(contract.decodeUnifiedSourceRevision(JSON.stringify({ source })), { source, explanation: "" });
  assert.throws(() => contract.decodeUnifiedSource(JSON.stringify({ source: "", explanation: "No change needed" })), /buildUnifiedApp/);
  assert.throws(() => contract.decodeUnifiedSourceRevision(JSON.stringify({ source: "", clarification: "Which price?" })), contract.UnifiedSourceClarificationError);
  for (const result of [null, [], {}, { source: 3 }, { source: "" }, { source: " ", explanation: " " }, { source: "", explanation: 3 }, { source, explanation: "x".repeat(8193) }, { source: "not TypeScript", explanation: "Changed" }]) {
    assert.throws(() => contract.decodeUnifiedSourceRevision(JSON.stringify(result)));
  }
});

// Each fixture simulates the exact CLI response envelopes and records the selected transport.
function transport(overrides = {}) {
  const calls = [];
  const responses = {
    ParsePromptIntent: { parseSDKIntent: { name: "Customer", description: "Lookup", action: "create", services: [{ name: "Stripe", endpoint_queries: ["get customer"] }] } },
    UnifiedAppCandidates: { serviceCandidatesByRefs: [{ ref: "Stripe", candidates: [{ id: "service-id", name: "Stripe", slug: "stripe", provider: { handle: "stripe" } }] }] },
    UnifiedAppServiceVersion: { service: { service_versions: [{ id: "version-id", name: "v1", status: "active" }] } },
    ClassifyPromptOperation: { classifyPromptOperation: "getCustomer" },
    DraftPromptUnifiedApp: { draftPromptUnifiedApp: JSON.stringify({ source }) },
    ...overrides,
  };
  // Reject any unplanned query so a new side effect cannot hide in a successful describe test.
  function send(channel, query, variables) {
    const operation = query.match(/query\s+(\w+)/)?.[1];
    calls.push({ channel, operation, variables });
    if (!responses[operation]) throw new Error(`Unexpected operation ${operation}`);
    return Promise.resolve(responses[operation]);
  }
  return { calls, api: { graphql: (q, v) => send("registry", q, v), mcpGraphql: (q, v) => send("engine", q, v) } };
}

// Browser describe must use Registry intent/drafting and Engine classification with exact immutable pins.
test("describe uses the CLI API chain without activating or applying", async () => {
  const mock = transport();
  const draft = await client(mock.api).describeUnifiedApp("Get a Stripe customer", () => {});
  assert.equal(draft.source, source);
  assert.deepEqual(draft.services, { "@stripe/stripe": service });
  assert.deepEqual(mock.calls.map(({ channel, operation }) => [channel, operation]), [
    ["registry", "ParsePromptIntent"], ["registry", "UnifiedAppCandidates"], ["registry", "UnifiedAppServiceVersion"], ["engine", "ClassifyPromptOperation"], ["registry", "DraftPromptUnifiedApp"],
  ]);
  assert.deepEqual(JSON.parse(mock.calls.at(-1).variables.selections), [{ service: "@stripe/stripe", service_id: "service-id", version: "v1", operation: "getCustomer" }]);
});

// Inbound-only Unified App goals pass exact event evidence to Registry without granting a provider operation.
test("describe drafts a webhook triggered Unified App from an exact event", async () => {
  const mock = transport({
    ParsePromptIntent: { parseSDKIntent: { name: "payment-handler", webhook_requested: true, services: [{ name: "Stripe", event_queries: ["successful payments"] }] } },
    DescribeAppEvents: { service: { webhooks: [{ name: "payment.succeeded", description: "A payment succeeded" }] } },
  });
  const draft = await client(mock.api).describeUnifiedApp("Handle successful Stripe payments", () => {});
  assert.deepEqual(draft.services["@stripe/stripe"].operations, []);
  assert.deepEqual(draft.services["@stripe/stripe"].webhooks, ["payment.succeeded"]);
  assert.deepEqual(JSON.parse(mock.calls.at(-1).variables.selections), [{ service: "@stripe/stripe", service_id: "service-id", version: "v1", event: "payment.succeeded" }]);
  assert.equal(mock.calls.some((call) => call.operation === "ClassifyPromptOperation"), false);
});

// The reviewed app cart binds the selected event to one applied registration before Engine planning.
test("Unified App config includes its webhook trigger", () => {
  const draft = { source, description: "Handle payments", webhookAttachment: "payments-events", services: { stripe: { ...service, operations: [], webhooks: ["payment.succeeded"] } } };
  const config = contract.unifiedConfig(draft, "payment-handler", "1.0.0", "default");
  assert.equal(config.webhook_attachment, "payments-events");
  assert.deepEqual(config.services.stripe.webhooks, ["payment.succeeded"]);
  assert.throws(() => contract.unifiedConfig({ ...draft, webhookAttachment: "" }, "payment-handler", "1.0.0", "default"), /webhook registration/);
});

// Legacy provider metadata refreshes exact reviewed pins once, then reuses unchanged source and selection.
test("plan repairs missing provider identity once without changing the requested app", async () => {
  const calls = [];
  const requestConfig = { name: "Customer", version: "1.0.0", source };
  const mock = {
    workspace: {
      // An enabled version still needs metadata repair when created by an older Engine.
      getServices: async () => [{ service_id: service.service_id, enabled_versions: [{ service_version_id: service.service_version_id, status: "active" }] }],
      // Record exact pins rather than accepting an inferred latest version.
      refreshServiceContract: async (...args) => calls.push(["refresh", ...args]),
    },
    appConfig: {
      // The first admission fails before creating a plan; the second binds refreshed metadata.
      plan: async (kind, request) => {
        calls.push(["plan", kind, request]);
        // Trigger migration recovery only before the recorded refresh.
        if (calls.length === 1) throw new authorization.APIRequestError(409, { code: "service_provider_identity_unavailable" });
        return { plan_id: "repaired" };
      },
    },
  };
  const result = await client(mock).planUnifiedApp(requestConfig, { stripe: service, alias: service }, () => {});
  assert.equal(result.plan_id, "repaired");
  assert.deepEqual(calls[1], ["refresh", "service-id", "version-id"]);
  assert.deepEqual(calls[0], calls[2]);
  assert.equal(calls.length, 3);
});

// Admission errors never become unbounded retries or permission-bypassing recovery.
test("plan stops after one repair and never refreshes unrelated errors", async () => {
  for (const code of ["service_provider_identity_unavailable", "permission_denied"]) {
    let attempts = 0;
    let refreshes = 0;
    const mock = {
      workspace: {
        // Keep the existing immutable activation constant throughout the failed retry.
        getServices: async () => [{ service_id: service.service_id, enabled_versions: [{ service_version_id: service.service_version_id, status: "active" }] }],
        // Count repair mutations independently from plan attempts.
        refreshServiceContract: async () => { refreshes += 1; },
      },
      appConfig: {
        // A persistent server failure must escape the bounded recovery path.
        plan: async () => { attempts += 1; throw new authorization.APIRequestError(409, { code }); },
      },
    };
    await assert.rejects(client(mock).planUnifiedApp({ name: "Customer", version: "1" }, { stripe: service }, () => {}));
    // Only the explicit migration error authorizes the one repair attempt.
    assert.equal(refreshes, code === "service_provider_identity_unavailable" ? 1 : 0);
    assert.equal(attempts, 1 + refreshes);
  }
});

// Ambiguous goals stop before discovery or compilation and remain actionable in the form.
// The creation form must receive a typed question before any service discovery or source request.
test("clarification stops before service discovery", async () => {
  const mock = transport({ ParsePromptIntent: { parseSDKIntent: { clarification: "Which customer?" } } });
  await assert.rejects(client(mock.api).describeUnifiedApp("Get customer", () => {}), { name: "AppDescriptionClarificationError", message: "Which customer?" });
  assert.equal(mock.calls.length, 1);
});

// Private source compilation is the first lifecycle action, and only follows explicit UI review.
test("plan activates exact dependencies and submits the unified-app receipt contract", async () => {
  const calls = [];
  const mock = {
    workspace: {
      // Empty membership forces a reviewed exact activation before planning.
      getServices: async () => [],
      addService: async (...args) => calls.push(["activate", ...args]),
    },
    appConfig: {
      // Capture the real plan shape while prohibiting accidental apply in this boundary.
      plan: async (kind, input) => { calls.push(["plan", kind, input]); return { plan_id: "p", source_hash: input.source_hash }; },
    },
  };
  const config = contract.unifiedConfig({ source, services: { "@stripe/stripe": service } }, "Customer", "1.0.0", "bucket-id");
  const plan = await client(mock).planUnifiedApp(config, { "@stripe/stripe": service }, () => {});
  assert.deepEqual(calls[0], ["activate", "service-id", "@stripe/stripe", "v1", "version-id"]);
  assert.equal(calls[1][1], "unified-app");
  assert.equal(calls[1][2].config_key, "unified_app:Customer:1.0.0");
  assert.match(plan.source_hash, /^sha256:[a-f0-9]{64}$/);
  assert.equal(config.kind, "unified_app");
  assert.equal(config.source, source);
});

// Bad model output must never turn into a successful empty source draft.
test("source validation mirrors CLI ambiguity and size checks", () => {
  assert.throws(() => contract.decodeUnifiedSource('{"clarification":"Specify the mapping"}'), /Specify the mapping/);
  assert.throws(() => contract.decodeUnifiedSource('{"source":""}'), /buildUnifiedApp/);
  assert.throws(() => contract.decodeUnifiedSource(JSON.stringify({ source: "buildUnifiedApp" + "x".repeat(65536) })), /bounded/);
  assert.throws(() => contract.decodeUnifiedSource("not JSON"));
});

// SDK and MCP resolve capabilities without drafting hosted TypeScript.
test("delivery adapters share discovery and stop before source drafting", async () => {
  for (const kind of ["sdk", "mcp", "api", "app"]) {
    const mock = transport();
    const draft = await client(mock.api, "app-describe-api").describeApp("Get a Stripe customer", kind, () => {});
    assert.deepEqual(draft.services, { "@stripe/stripe": service });
    assert.equal(draft.source, undefined);
    assert.equal(mock.calls.some((call) => call.operation === "DraftPromptUnifiedApp"), false);
  }
});

// Explicit all-operation requests bypass classification but remain unavailable for hosted source.
test("select-all is retained only for delivery adapters", async () => {
  const overrides = { ParsePromptIntent: { parseSDKIntent: { name: "stripe-sdk", services: [{ name: "Stripe", select_all_operations: true }] } } };
  const mock = transport(overrides);
  const draft = await client(mock.api, "app-describe-api").describeApp("All Stripe operations", "sdk", () => {});
  assert.equal(draft.services["@stripe/stripe"].select_all, true);
  assert.deepEqual(draft.services["@stripe/stripe"].operations, []);
  assert.equal(mock.calls.some((call) => call.operation === "ClassifyPromptOperation"), false);
  await assert.rejects(client(transport(overrides).api).describeUnifiedApp("All Stripe operations", () => {}), /specific operations/);
});

// Sequential intent cannot silently become independent callable operations.
test("SDK and MCP reject sequential workflows before discovery", async () => {
  for (const kind of ["sdk", "mcp"]) {
    const mock = transport({ ParsePromptIntent: { parseSDKIntent: { sequential: true, services: [{ name: "Stripe", endpoint_queries: ["get customer"] }] } } });
    await assert.rejects(client(mock.api, "app-describe-api").describeApp("Find a customer then create a task", kind, () => {}), /Unified App/);
    assert.equal(mock.calls.length, 1);
  }
});

// Inbound-only description preserves event scope without granting operation access.
test("MCP event-only goals resolve exact events", async () => {
  const mock = transport({
    ParsePromptIntent: { parseSDKIntent: { webhook_requested: true, services: [{ name: "Stripe", event_queries: ["successful payments"] }] } },
    DescribeAppEvents: { service: { webhooks: [{ name: "payment.succeeded", description: "A payment succeeded" }, { name: "payment.failed", description: "A payment failed" }] } },
  });
  const draft = await client(mock.api, "app-describe-api").describeApp("Receive successful Stripe payments", "mcp", () => {});
  assert.deepEqual(draft.services["@stripe/stripe"].operations, []);
  assert.deepEqual(draft.services["@stripe/stripe"].webhooks, ["payment.succeeded"]);
  assert.deepEqual(mock.calls.at(-1).variables, { id: "service-id", version: "v1" });
});

// Multiple mentions of one provider contribute to one immutable capability set.
test("duplicate service mentions preserve every operation", async () => {
  const mock = transport({ ParsePromptIntent: { parseSDKIntent: { services: [{ name: "Stripe", endpoint_queries: ["first"] }, { name: "Stripe", endpoint_queries: ["second"] }] } } });
  let count = 0;
  // Distinct classifications simulate two intents for the same canonical service.
  mock.api.mcpGraphql = async () => ({ classifyPromptOperation: ++count === 1 ? "getCustomer" : "createCustomer" });
  const draft = await client(mock.api, "app-describe-api").describeApp("Get and create Stripe customers", "sdk", () => {});
  assert.deepEqual(draft.services["@stripe/stripe"].operations, ["getCustomer", "createCustomer"]);
});

// CLI event matching gives exact names precedence and rejects tied candidates.
test("event matching fails closed on ambiguity", () => {
  const events = [{ name: "payment.created", description: "Payment created" }, { name: "payment.updated", description: "Payment updated" }];
  assert.throws(() => describeContract.matchDescribeEvents(["payment"], events), /exact webhook event/);
  assert.deepEqual(describeContract.matchDescribeEvents(["payment.created"], events), ["payment.created"]);
  assert.deepEqual(describeContract.matchDescribeEvents([], events), ["payment.created", "payment.updated"]);
});

// Hydration proves named operations still belong to the exact immutable pin.
test("selection hydration rejects missing operations and mismatched versions", async () => {
  // An empty operation catalogue cannot satisfy a non-empty proposal.
  const api = { graphql: async () => ({ service: { id: "service-id", webhooks: [] }, serviceVersions: [{ id: "version-id", name: "v1" }], serviceOperations: [] }) };
  await assert.rejects(client(api, "app-describe-selection").loadDescribedSelection({ stripe: service }), /unavailable/);
  // A same-named version with another immutable ID must not substitute for the selected version.
  api.graphql = async () => ({ service: { id: "service-id", webhooks: [] }, serviceVersions: [{ id: "different-version", name: "v1" }], serviceOperations: [{ id: "op-id", name: "getCustomer" }] });
  await assert.rejects(client(api, "app-describe-selection").loadDescribedSelection({ stripe: service }), /unavailable/);
});

// Hydrated IDs connect described operations and events to the existing manual picker.
test("selection hydration preserves operation and event states", async () => {
  const calls = [];
  // Capture exact-version reads while returning a bounded provider fixture.
  const api = { graphql: async (query, variables) => {
    calls.push(variables);
    return { service: { id: "service-id", webhooks: [{ id: "event-id", name: "payment.succeeded" }] }, serviceVersions: [{ id: "version-id", name: "v1" }], serviceOperations: [{ id: "op-id", name: "getCustomer" }] };
  } };
  const hydrated = await client(api, "app-describe-selection").loadDescribedSelection({ stripe: { ...service, webhooks: ["payment.succeeded"] } });
  assert.deepEqual(calls, [{ id: "service-id", version: "v1" }]);
  assert.deepEqual([...hydrated.selections["service-id"]], ["op-id"]);
  assert.deepEqual([...hydrated.webhookSelections["service-id"]], ["event-id"]);
  assert.equal(hydrated.versionSelections["service-id"], "version-id");
});

// Duplicate service names must pause every adapter until an explicit publisher is chosen.
test("provider choice resumes the same intent with only the chosen service", async () => {
  const candidates = [
    { id: "service-id", name: "Stripe", slug: "stripe", provider: { handle: "stripe" } },
    { id: "other-id", name: "Stripe", slug: "stripe", provider: { handle: "another" } },
  ];
  for (const kind of ["sdk", "mcp", "api", "unified_app"]) {
    const mock = transport({ UnifiedAppCandidates: { serviceCandidatesByRefs: [{ ref: "Stripe", candidates }] } });
    // A deferred choice proves that classification cannot run before user input.
    let resolveChoice;
    const chosen = new Promise((resolve) => { resolveChoice = resolve; });
    let showChoice;
    const shown = new Promise((resolve) => { showChoice = resolve; });
    // Record the exact choices shown without selecting a default provider.
    const choose = async (reference, options) => {
      assert.equal(reference, "Stripe");
      assert.deepEqual(options, candidates);
      showChoice();
      return chosen;
    };
    // Exercise the hosted wrapper as well as the shared delivery adapter client.
    const pending = kind === "unified_app"
      ? client(mock.api).describeUnifiedApp("Get a Stripe customer", () => {}, choose)
      : client(mock.api, "app-describe-api").describeApp("Get a Stripe customer", kind, () => {}, choose);
    await shown;
    assert.deepEqual(mock.calls.map((call) => call.operation), ["ParsePromptIntent", "UnifiedAppCandidates"]);
    resolveChoice("other-id");
    const proposal = await pending;
    assert.deepEqual(Object.keys(proposal.services), ["@another/stripe"]);
    assert.equal(proposal.services["@another/stripe"].service_id, "other-id");
    assert.equal(mock.calls.filter((call) => call.operation === "ParsePromptIntent").length, 1);
    assert.equal(mock.calls.find((call) => call.operation === "ClassifyPromptOperation").variables.service_id, "other-id");
  }
});

// Invalid, cancelled, and absent choices cannot trigger operation discovery or draft generation.
test("unresolved provider choices fail before classification", async () => {
  const candidates = [
    { id: "one", name: "Stripe", slug: "stripe", provider: { handle: "one" } },
    { id: "two", name: "Stripe", slug: "stripe", provider: { handle: "two" } },
  ];
  for (const choose of [undefined, async () => "unlisted", async () => { throw new DOMException("Cancelled", "AbortError"); }]) {
    const mock = transport({ UnifiedAppCandidates: { serviceCandidatesByRefs: [{ ref: "Stripe", candidates }] } });
    await assert.rejects(client(mock.api).describeUnifiedApp("Get a Stripe customer", () => {}, choose), /Choose a service|no longer available|Cancelled/);
    assert.equal(mock.calls.length, 2);
  }
});

// Discovery misses need a different name rather than a nonexistent publisher choice.
test("missing service gives a distinct actionable error", async () => {
  const mock = transport({ UnifiedAppCandidates: { serviceCandidatesByRefs: [{ ref: "Stripe", candidates: [] }] } });
  await assert.rejects(client(mock.api).describeUnifiedApp("Get a Stripe customer", () => {}, async () => assert.fail("No choices exist")), /No service found/);
  assert.equal(mock.calls.length, 2);
});

// A canonical publisher reference should bypass ambiguity even when discovery returns nearby services.
test("provider-qualified references prefer the exact service without asking", async () => {
  const mock = transport({
    ParsePromptIntent: { parseSDKIntent: { name: "Customer", services: [{ name: "@stripe/stripe", endpoint_queries: ["get customer"] }] } },
    UnifiedAppCandidates: { serviceCandidatesByRefs: [{ ref: "@stripe/stripe", candidates: [
      { id: "service-id", name: "Stripe", slug: "stripe", provider: { handle: "stripe" } },
      { id: "other-id", name: "Stripe", slug: "stripe", provider: { handle: "another" } },
    ] }] },
  });
  const proposal = await client(mock.api).describeUnifiedApp("Get a @stripe/stripe customer", () => {}, async () => assert.fail("Exact reference needs no clarification"));
  assert.deepEqual(proposal.services, { "@stripe/stripe": service });
});

// A delayed source response must not hide already grounded capabilities from the shared review flow.
test("describe reports stages and exposes selection before source is ready", async () => {
  const mock = transport();
  const send = mock.api.graphql;
  let finishDraft;
  const pendingDraft = new Promise((resolve) => { finishDraft = resolve; });
  let drafting;
  const started = new Promise((resolve) => { drafting = resolve; });
  // Only source generation is delayed, leaving real client discovery order observable.
  mock.api.graphql = async (query, variables) => {
    if (query.includes("query DraftPromptUnifiedApp")) { drafting(); return pendingDraft; }
    return send(query, variables);
  };
  const stages = [];
  let preview;
  const pending = client(mock.api).describeUnifiedApp("Get a Stripe customer", (message, stage) => stages.push(stage), undefined, (proposal) => { preview = { ...proposal }; });
  await started;
  assert.deepEqual(preview.services, { "@stripe/stripe": service });
  assert.equal(preview.source, undefined);
  assert.deepEqual([...new Set(stages)], ["intent", "services", "operations", "source"]);
  finishDraft({ draftPromptUnifiedApp: JSON.stringify({ source }) });
  assert.equal((await pending).source, source);
  assert.equal(stages.at(-1), "review");
});

// Requests across different services must share the same finite pool, not one pool per operation group.
test("describe scheduler caps concurrency and preserves request order", async () => {
  const schedule = describeContract.createDescribeScheduler();
  let active = 0, maximum = 0;
  const releases = [];
  // Hold each admitted request so accidental eager scheduling is visible without timing assumptions.
  const jobs = Array.from({ length: 9 }, (_, index) => schedule(async () => {
    active++; maximum = Math.max(maximum, active);
    await new Promise((resolve) => releases.push(resolve));
    active--; return index;
  }));
  assert.equal(releases.length, 4);
  // Release successive batches while allowing promise continuations to admit waiting work.
  for (let batch = 0; batch < 3; batch++) {
    releases.splice(0).forEach((release) => release());
    await new Promise((resolve) => setImmediate(resolve));
  }
  assert.deepEqual(await describeContract.settleDescribe(jobs), [0,1,2,3,4,5,6,7,8]);
  assert.equal(maximum, 4);
});

// A failed paid request cancels queued work and drains admitted siblings before another attempt can start.
test("describe scheduler stops queued work on failure", async () => {
  const schedule = describeContract.createDescribeScheduler();
  let started = 0;
  const releases = [];
  const failure = new Error("provider unavailable");
  const jobs = Array.from({ length: 8 }, (_, index) => schedule(async () => {
    started++;
    await new Promise((resolve) => releases.push(resolve));
    // One provider failure invalidates this complete discovery snapshot.
    if (index === 0) throw failure;
    return index;
  }));
  const settled = assert.rejects(describeContract.settleDescribe(jobs), /provider unavailable/);
  releases.splice(0).forEach((release) => release());
  await settled;
  assert.equal(started, 4);
});

// Clarification must request a revised selection rather than endlessly retrying the same incomplete pins.
// Both stages share question handling while retaining the source-specific retry behavior.
test("source clarification is distinguishable from a retryable transport failure", () => {
  assert.throws(() => contract.decodeUnifiedSource('{"clarification":"Which lookup should be used?"}'), contract.UnifiedSourceClarificationError);
  assert.throws(() => contract.decodeUnifiedSource('{"clarification":"Which lookup should be used?"}'), contract.AppDescriptionClarificationError);
  assert.throws(() => contract.decodeUnifiedSource('{"source":""}'), (error) => !(error instanceof contract.UnifiedSourceClarificationError));
});

// A plain-language answer supplements the user's original goal without inventing endpoint names or losing earlier decisions.
test("description follow-ups preserve the original goal and ordered answers", () => {
  const goal = "Create a payment link for the subscription they picked using their email and name.";
  const answers = [{ question: "Is this a new subscription or an existing one?", answer: "A new subscription" }];
  const continued = describeContract.describeGoalWithAnswers(goal, answers);
  assert.ok(continued.startsWith(goal));
  assert.ok(continued.endsWith(JSON.stringify(answers)));
  assert.equal(describeContract.describeGoalWithAnswers(goal, []), goal);
  assert.deepEqual(answers, [{ question: "Is this a new subscription or an existing one?", answer: "A new subscription" }]);
  assert.throws(() => describeContract.describeGoalWithAnswers("界".repeat(6000), []), /too long/);
});

// Editing an existing team-owned app must carry its immutable owner into the shared plan endpoint.
test("successor planning retains the saved owner team", async () => {
  let submitted;
  const mock = {
    workspace: {
      // Already-enabled dependencies require no workspace mutation during the successor plan.
      getServices: async () => [{ service_id: service.service_id, enabled_versions: [{ service_version_id: service.service_version_id, status: "active" }] }],
    },
    appConfig: {
      // Record the exact request crossing the production management boundary.
      plan: async (kind, request) => { submitted = { kind, request }; return { plan_id: "successor" }; },
    },
  };
  await client(mock).planUnifiedApp({ name: "Customer", version: "1.0.1", source }, { stripe: service }, () => {}, "payments");
  assert.equal(submitted.kind, "unified-app");
  assert.equal(submitted.request.owner_team, "payments");
  assert.equal(submitted.request.config_key, "unified_app:Customer:1.0.1");
});
