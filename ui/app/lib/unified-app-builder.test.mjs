import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import ts from "typescript";
import { act, createElement } from "react";
import { createRoot } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import { JSDOM } from "jsdom";
import * as contract from "./unified-app-contract.ts";
import * as editing from "./unified-app-edit.ts";
import * as buckets from "./unified-app-runtime-buckets.ts";
import * as describing from "./app-describe-contract.ts";
import * as builder from "./app-builder-contract.ts";
import * as sourceDraft from "./unified-app-source-draft.ts";
import * as sourceConfig from "./unified-app-source-config.ts";

/** Runs the real authoring route and tab layout while isolating network, credential, and catalogue dependencies. */
test("compact builder retains edits, review state, and compilation behavior across keyboard and pointer tabs", async () => {
  const dom = new JSDOM('<div id="root"></div>', { url: "http://localhost" });
  const previous = { window: globalThis.window, document: globalThis.document, act: globalThis.IS_REACT_ACT_ENVIRONMENT };
  globalThis.window = dom.window; globalThis.document = dom.window.document; globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  const saved = {
    app_id: "billing-v1", owner_team: "billing",
    config: { kind: "unified_app", name: "fused-billing-customer", version: "1.0.1", bucket: "default", description: "Find a billing customer", source: 'export default buildUnifiedApp({ config: {"bucket":"default","version":"1.0.1"} });', services: { Stripe: { version: "v1", operations: ["getCustomer"] } } },
    service_pins: [{ key: "Stripe", service_id: "stripe", service_version_id: "stripe-v1" }],
  };
  const plans = [], opens = [];
  const resolutions = [];
  let registeredEditor;
  const agent = {
    // The same editor bridge used by the assistant must stay connected after a presentation-only tab change.
    registerEditor: (value) => { registeredEditor = value; return /** Bridge cleanup has no external resource to release in this fixture. */ () => {}; },
    // Opening the conversation records intent without sending a model request during this test.
    open: (prompt) => opens.push(prompt),
  };
  const params = new URLSearchParams("edit=billing-v1");
  const require = createRequire(import.meta.url);
  const dependencies = {
    "~/components/layout/PageBackLink": {
      // The route retains a real back destination without loading unrelated navigation chrome.
      PageBackLink: ({ to, children }) => createElement("a", { href: to }, children),
    },
    "~/components/apps/AppDetailChrome": {
      // The initial authorized source-loading state keeps its normal back destination.
      AppDetailBackLink: ({ to }) => createElement("a", { href: to }, "Back to app"),
    },
    "~/components/apps/AppCredentialWarning": {
      // This reviewed fixture has no credential warnings to acknowledge.
      AppCredentialWarning: () => null,
    },
    "@remix-run/react": {
      // Router state stays stable while the real route exercises its loading effects.
      useSearchParams: () => [params], useLocation: () => ({ hash: "" }),
      // Native anchors keep back and template destinations observable without mounting a router.
      Link: ({ to, ...props }) => createElement("a", { ...props, href: to }),
    },
    "~/lib/api": { api: { appConfig: {
      // Only saved source reads are supported; an accidental runtime mutation fails at this boundary.
      source: async () => structuredClone(saved),
    } } },
    "~/lib/unified-app-edit": editing,
    "~/lib/unified-app-contract": contract,
    "~/lib/unified-app-runtime-buckets": buckets,
    "~/lib/app-describe-contract": describing,
    "~/lib/app-builder-contract": builder,
    "~/lib/unified-app-source-config": sourceConfig,
    "~/lib/unified-app-source-draft": sourceDraft,
    "~/lib/unified-app-source-services": {
      // Hold new version lookups to prove older source edits cannot overwrite newer ones.
      resolveUnifiedSourceService: (key, version, retainedID) => new Promise(/** Expose only completion of the bounded read-only lookup. */ (resolve) => resolutions.push({key,version,retainedID,resolve})),
    },
    "~/lib/app-builder": {
      // Metadata hydration uses the selected bucket's existing identity.
      listAppBuildSelectors: async () => ({ items: [{ resource_id: "default", display_name: "default" }, { resource_id: "deployment-id", display_name: "deployment" }] }),
    },
    "~/lib/unified-app-api": {
      // Record exactly the configuration being reviewed without compiling or executing provider code.
      planUnifiedApp: async (config) => { plans.push(config); return { plan_id: "reviewed", source_hash: "sha256:reviewed", summary: { version: config.version } }; },
    },
    "~/components/access/CurrentActorAccess": {
      // Endpoint mocks still determine what data is returned; this fixture focuses on the authorized editor path.
      useCurrentActorAccess: () => ({ access: {} }),
    },
    "~/lib/current-actor-access": {
      // Creation gates do not block editing this already-authorized family.
      hasWorkspacePermission: () => true,
    },
    "~/components/agent/FusedAgentContext": {
      // Keep bridge identity stable so unrelated rerenders do not masquerade as navigation.
      useFusedAgent: () => agent,
    },
    "~/components/apps/useAppCredentialReview": {
      // Validation remains review-only and never invokes this deployment-time hook.
      useAppCredentialReview: () => ({}),
    },
    "~/components/buckets/useCredentialSetCreation": {
      // No credential creation control is needed when editing the family's immutable default.
      useCredentialSetCreation: () => ({}),
    },
    "~/components/apps/AppServiceBuilder": {
      // Catalogue discovery is independently tested; the saved capabilities remain mounted under the disclosure.
      AppOperationPicker: () => createElement("div", null, "Selected Stripe operations"),
    },
    "~/components/apps/AppServiceAuthFields": {
      // This fixture has no per-service credential override to render.
      AppServiceAuthFields: () => null,
    },
    "~/components/code/TypeScriptEditor": {
      // Expose the production code wrapper's comparison baseline to detect remounts that would erase change review.
      TypeScriptEditor: ({ id, value, baseline, onChange, disabled }) => createElement("textarea", { id, value, disabled, "aria-label": "TypeScript source", "data-baseline": baseline, onChange: /** Native edits still flow through the production wrapper's controlled source callback. */ (event) => onChange(event.target.value) }),
    },
  };
  const aliases = {
    "~/components/apps/UnifiedAppBuilderLayout": "../components/apps/UnifiedAppBuilderLayout.tsx",
    "~/components/apps/UnifiedAppCodeEditor": "../components/apps/UnifiedAppCodeEditor.tsx",
    "~/components/apps/UnifiedAppCompileAction": "../components/apps/UnifiedAppCompileAction.tsx",
    "~/components/apps/UnifiedAppBucketScope": "../components/apps/UnifiedAppBucketScope.tsx",
    "~/components/forms/Select": "../components/forms/Select.ts",
  };

  /** Resolves production authoring components and fails on unexpected dependencies used by this fixture. */
  function load(file) {
    const code = ts.transpileModule(readFileSync(new URL(file, import.meta.url), "utf8"), { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX } }).outputText;
    const exports = {};
    /** Keeps known production components real while unused external UI dependencies remain inert. */
    function resolve(name) {
      // Explicit fixtures override transport and catalogue modules.
      if (dependencies[name]) return dependencies[name];
      // The route's layout, code wrapper, and compile action execute their actual implementations.
      if (aliases[name]) return load(aliases[name]);
      // The shared searchable-select enhancement must resolve relative to its production component.
      if (name === "../../../branding/searchable_select.js") return require(new URL("../../branding/searchable_select.js", import.meta.url).pathname);
      // Unrendered success-only and optional controls cannot perform work during a review-only test.
      if (name.startsWith("~/")) return {};
      return require(name);
    }
    new Function("require", "exports", code)(resolve, exports);
    return exports;
  }
  const CreateUnifiedApp = load("../routes/integrations.unified-apps.new.tsx").default;
  const root = createRoot(document.getElementById("root"));

  /** Finds a tab by its accessible label instead of relying on toolbar order. */
  function tab(label) { return [...document.querySelectorAll('[role="tab"]')].find(/** Match the visible tab label rather than its generated identity. */ (element) => element.textContent.trim() === label); }
  /** Changes controlled editor inputs through the same React event handlers used by a browser. */
  async function change(element, value) { await act(async () => Simulate.change(element, { target: { value } })); }
  /** Activates an action and flushes its loading and review-state updates. */
  async function click(element) { await act(async () => Simulate.click(element)); }
  /** Independently reads the app's config property through TypeScript's parser. */
  function inlineConfig() {
    const parsed = ts.createSourceFile("app.ts", document.getElementById("unified-app-source").value, ts.ScriptTarget.Latest, true);
    const statement = parsed.statements.find(/** Only the default exported app owns current metadata. */ (item) => ts.isExportAssignment(item));
    const property = statement.expression.arguments[0].properties.find(/** Nested schema fields cannot supply metadata. */ (item) => item.name?.getText(parsed) === "config");
    return JSON.parse(property.initializer.getText(parsed));
  }

  try {
    await act(/** Hydrate the saved version through the production route's initial source effect. */ async () => root.render(createElement(CreateUnifiedApp)));
    assert.equal(tab("Config").getAttribute("aria-selected"), "true");
    assert.equal(document.querySelector('[aria-label="App settings"] input').value, saved.config.name);
    assert.equal(document.querySelector('[aria-label="App settings"] input').readOnly, true);
    assert.equal(document.querySelector('[aria-label="Services & events"] details'), null, 'capabilities have no outer accordion');
    const source = document.getElementById("unified-app-source");
    const baseline = source.getAttribute("data-baseline");
    assert.equal(source.closest('[role="tabpanel"]').hidden, true);
    assert.equal(inlineConfig().version, "1.0.2", 'the initial successor version is reflected in its visible source');
    await change(document.getElementById("unified-credential-bucket"), "__fused_runtime_bucket__");
    assert.deepEqual(inlineConfig().runtime_buckets, ["default"]);
    const bucketQuery = document.querySelector('input[placeholder="Add a bucket…"]');
    await change(bucketQuery, "deployment");
    await click([...document.querySelectorAll('[role="option"]')].find(/** Choose a permitted identity from the real bucket scope control. */ (element) => element.textContent === "deployment"));
    assert.deepEqual(inlineConfig().runtime_buckets, ["default", "deployment"], 'permitted bucket additions update source immediately');
    await click(document.querySelector('[aria-label="Remove deployment"]'));
    assert.deepEqual(inlineConfig().runtime_buckets, ["default"]);
    await change(document.getElementById("unified-credential-bucket"), "default");
    assert.deepEqual(inlineConfig().runtime_buckets, [], 'fixed routing clears the runtime allowlist in source');
    assert.equal(document.querySelector("footer").closest('[role="tabpanel"]'), null);
    await click(tab("Code"));
    assert.equal(source.closest('[role="tabpanel"]').hidden, false);
    await change(source, `${source.value}\n// Reviewed change`);
    await act(/** Keyboard selection must move both the active panel and focus to Config. */ async () => Simulate.keyDown(document.querySelector('[role="tablist"]'), { key: "ArrowLeft" }));
    assert.equal(document.activeElement, tab("Config"));
    assert.equal(source, document.getElementById("unified-app-source"), 'switching tabs preserves the editor instance');
    assert.equal(source.getAttribute("data-baseline"), baseline, 'change review retains the original source baseline');
    const version = document.querySelectorAll('[aria-label="App settings"] input')[1];
    await change(version, "1.0.3");
    assert.equal(inlineConfig().version, "1.0.3");
    await change(document.querySelector('[aria-label="App settings"] textarea'), "Updated customer lookup");
    assert.equal(inlineConfig().description, "Updated customer lookup", 'text fields update the same metadata declaration as bucket controls');
    await change(document.getElementById("unified-credential-bucket"), "__fused_runtime_bucket__");
    await change(document.querySelector('input[placeholder="Add a bucket…"]'), "deployment");
    await click([...document.querySelectorAll('[role="option"]')].find(/** Include the second permitted bucket in the configuration actually being compiled. */ (element) => element.textContent === "deployment"));
    await click([...document.querySelectorAll("button")].find(/** Compilation remains available outside the selected panel. */ (button) => button.textContent.includes("Validate and compile")));
    assert.equal(plans.length, 1);
    assert.equal(plans[0].version, "1.0.3");
    assert.deepEqual(plans[0].runtime_buckets, ["default", "deployment"]);
    assert.match(plans[0].source, /Reviewed change/);
    assert.equal(plans[0].source, source.value, 'validation submits exactly the file visible in Code');
    assert.match(document.querySelector("footer").textContent, /Ready to deploy/);
    await click(tab("Code"));
    assert.match(document.querySelector("footer").textContent, /Ready to deploy/, 'tab changes do not invalidate a reviewed plan');
    await change(source, `${source.value}\n// Another change`);
    assert.doesNotMatch(document.querySelector("footer").textContent, /Ready to deploy/);
    // Code edits must update the form and invalidate a reviewed plan without rewriting the buffer.
    const fromCode = { ...inlineConfig(), version: "1.0.4", description: "Edited in Code", runtime_buckets: ["default"], services: { Stripe: { version: "v1", operations: ["getCustomer"], auth: {type:"bearer",name:"bearerAuth"} } } };
    const changedSource = source.value.replace(JSON.stringify(inlineConfig(), null, 2), JSON.stringify(fromCode, null, 2));
    await change(source, changedSource);
    assert.equal(source.value, changedSource, 'valid code edits retain the exact authored text');
    assert.equal(document.querySelectorAll('[aria-label="App settings"] input')[1].value, "1.0.4");
    assert.equal(document.querySelector('[aria-label="App settings"] textarea').value, "Edited in Code");
    assert.deepEqual(inlineConfig().runtime_buckets, ["default"]);
    await click([...document.querySelectorAll("button")].find(/** Validate must consume code-authored settings as well as form-authored settings. */ (button) => button.textContent.includes("Validate and compile")));
    assert.equal(plans[1].version, "1.0.4");
    assert.equal(plans[1].description, "Edited in Code");
    assert.deepEqual(plans[1].services.Stripe.auth, {type:"bearer",name:"bearerAuth"});
    assert.equal(plans[1].source, changedSource);
    const completeSource = source.value;
    await change(source, 'export default buildUnifiedApp({ config: {"bucket":');
    assert.equal(source.value, 'export default buildUnifiedApp({ config: {"bucket":', 'incomplete metadata stays available for correction');
    assert.match(document.querySelector('[role="alert"]').textContent, /Config could not be synchronized/);
    assert.equal([...document.querySelectorAll("button")].find(/** Unsynchronized metadata cannot enable a plan that differs from the visible file. */ (button) => button.textContent.includes("Validate and compile")).disabled, true);
    await change(source, completeSource);
    // An outstanding exact-version lookup cannot take ownership back from a newer completed source edit.
    const resolvingConfig = { ...inlineConfig(), services: { Stripe: {version:"v2",operations:["getCustomer"]} } };
    await change(source, completeSource.replace(JSON.stringify(inlineConfig(), null, 2), JSON.stringify(resolvingConfig, null, 2)));
    assert.equal(resolutions.length, 1);
    assert.equal(resolutions[0].retainedID, "stripe", 'a changed version stays bound to the retained provider');
    await change(source, completeSource);
    await act(/** Finish the superseded lookup after the newer source already hydrated its form. */ async () => resolutions[0].resolve({service_id:"stripe",service_version_id:"stripe-v2",version:"v2",operations:[]}));
    assert.equal(source.value, completeSource);
    assert.equal(inlineConfig().services.Stripe.version, "v1");
    await click(tab("Config"));
    await click([...document.querySelectorAll("button")].find(/** The toolbar assistant must first reveal the source it will review. */ (button) => button.textContent.includes("Ask Fused")));
    assert.equal(tab("Code").getAttribute("aria-selected"), "true", 'assistant editing first reveals the connected source');
    assert.equal(registeredEditor.fieldID, source.id);
    assert.equal(opens.length, 1);
    assert.equal(saved.config.version, "1.0.1", 'reviewing never changes the deployed baseline');
  } finally {
    await act(/** Unmount production effects before releasing the test document. */ async () => root.unmount()); dom.window.close();
    globalThis.window = previous.window; globalThis.document = previous.document; globalThis.IS_REACT_ACT_ENVIRONMENT = previous.act;
  }
});
