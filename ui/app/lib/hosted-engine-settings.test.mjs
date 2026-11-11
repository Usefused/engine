import test from "node:test";
import assert from "node:assert/strict";
import { build } from "esbuild";
import { JSDOM } from "jsdom";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { fileURLToPath, pathToFileURL } from "node:url";
import path from "node:path";
const app = fileURLToPath(new URL("..", import.meta.url));

// settingsHarness bundles the real component while replacing only network and application-context boundaries.
async function settingsHarness() {
  const result = await build({
    stdin: { contents: `export { HostedEngineSettings } from "./components/settings/HostedEngineSettings.tsx"; export { createRoot } from "react-dom/client"; export { act, createElement } from "react";`, resolveDir: app, loader: "tsx" },
    bundle: true, write: false, format: "esm", platform: "node", jsx: "automatic", define: { "process.env.NODE_ENV": '"development"' },
    plugins: [{ name: "settings-fixtures", setup(builder) {
      // Share Node's React runtime so async act uses ordinary timers and releases its scheduler handles.
      builder.onResolve({ filter: /^react(?:-dom)?(?:\/|$)/ }, ({ path: specifier }) => ({ path: fileURLToPath(import.meta.resolve(specifier)), external: true }));
      // Only context and transport imports become fixtures; rendering and upgrade logic stay real.
      builder.onResolve({ filter: /^~\// }, ({ path: specifier }) => {
        if (["~/lib/api", "~/components/access/CurrentActorAccess", "~/components/Toast"].includes(specifier)) return { path: specifier, namespace: "fixture" };
        return { path: path.join(app, `${specifier.slice(2)}.ts`) };
      });
      // These adapters use the existing API shapes without creating a provisioning Registry stub.
      builder.onLoad({ filter: /.*/, namespace: "fixture" }, ({ path: specifier }) => ({ contents: {
        "~/lib/api": "export const api = { hostedEngine: { state: () => globalThis.settingsFixture.state(), access: () => globalThis.settingsFixture.access(), upgrade: (intent) => globalThis.settingsFixture.upgrade(intent) } };",
        "~/components/access/CurrentActorAccess": "export function useCurrentActorAccess() { return { access: { workspace_id: 'test-workspace' } }; }",
        "~/components/Toast": "export function useToast() { return { confirm: () => globalThis.settingsFixture.confirm() }; }",
      }[specifier], loader: "js" }));
    } }],
  });
  const directory = mkdtempSync(path.join(tmpdir(), "fused-settings-test-"));
  const modulePath = path.join(directory, "settings.mjs");
  writeFileSync(modulePath, result.outputFiles[0].text);
  try { return await import(pathToFileURL(modulePath).href); }
  finally { rmSync(directory, { recursive: true }); }

}

// DOM interaction verifies that a lost response survives reload and an explicit Retry uses the same operation.
test("Settings confirms, retries the same intent, and waits for observed readiness", async () => {
  const dom = new JSDOM('<div id="root"></div>', { url: "https://engine.example.test" });
  globalThis.window = dom.window; globalThis.document = dom.window.document;
  globalThis.sessionStorage = dom.window.sessionStorage; globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  const { HostedEngineSettings, createRoot, act, createElement } = await settingsHarness();
  let state = { available: true, hosted: true, status: "ready", current_image: "engine:v1", target_image: "engine:v2", desired_generation: 1, observed_generation: 1, update_available: true };
  let manager = true;
  let confirmations = 0;
  const requests = [];
  globalThis.settingsFixture = {
    // Reads stay on the old release when the POST response is interrupted.
    state: async () => state,
    access: async () => ({ can_manage: manager, running_version: "v1" }),
    confirm: async () => { confirmations++; return true; },
    upgrade: async (intent) => { requests.push(intent); throw Error("Connection interrupted"); },
  };
  let root;
  // mount starts a new browser component lifecycle without clearing workspace-scoped session storage.
  async function mount() {
    root = createRoot(document.getElementById("root"));
    await act(async () => { root.render(createElement(HostedEngineSettings, { active: true })); });
  }
  try {
    await mount();
    await act(async () => { document.querySelector('[data-track="upgrade_hosted_engine"]').click(); });
    assert.equal(confirmations, 1); assert.equal(requests.length, 1);
    assert.equal(requests[0].expected_generation, 1);
    assert.match(document.body.textContent, /Connection interrupted/);
    await act(async () => { root.unmount(); }); await mount();
    assert.match(document.body.textContent, /Retry update/);
    // The retry succeeds but does not claim the new replica is ready yet.
    globalThis.settingsFixture.upgrade = async (intent) => {
      requests.push(intent);
      state = { ...state, status: "upgrading", current_image: "engine:v2", desired_generation: 2, update_available: false };
      return state;
    };
    await act(async () => { document.querySelector('[data-track="upgrade_hosted_engine"]').click(); });
    assert.deepEqual(requests[1], requests[0]); assert.equal(confirmations, 1);
    assert.equal(document.querySelector('[data-track="upgrade_hosted_engine"]').disabled, true);
    assert.doesNotMatch(document.body.textContent, /updated successfully/);
    await act(async () => { root.unmount(); });
    state = { ...state, status: "ready", observed_generation: 2 }; await mount();
    assert.match(document.body.textContent, /Engine updated successfully/);
    assert.equal(sessionStorage.getItem("fused:engine-upgrade:test-workspace"), null);
    await act(async () => { root.unmount(); }); manager = false; await mount();
    assert.equal(document.querySelector('[data-track="upgrade_hosted_engine"]'), null);
    assert.match(document.body.textContent, /Ask your account owner/);
  } finally {
    await act(async () => { root.unmount(); }); dom.window.close(); delete globalThis.settingsFixture;
  }
});
