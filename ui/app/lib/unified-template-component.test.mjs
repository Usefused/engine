import { Select } from "../components/forms/Select.ts";
import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import ts from "typescript";
import { act, createElement } from "react";
import { createRoot } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import { JSDOM } from "jsdom";
import * as template from "./unified-template.ts";
const require = createRequire(import.meta.url);

/** Supplies exact private source with deployment fields that must never be published. */
function source(id) {
  return { app_id: id, owner_team: "private-team", config: { kind: "unified_app", name: id, version: "1.0.0", bucket: "private-bucket", source: `export default buildUnifiedApp({}); // ${id}`, services: { "@test/crm": { version: "v1", operations: ["lookup"], bucket: "private-provider-bucket" } } }, service_pins: [{ key: "@test/crm", service_id: "service", service_version_id: "version" }] };
}

/** Mounts the real publisher while replacing only remote transports and surrounding access context. */
async function mountPublisher(overrides = {}, props = {}) {
  const dom = new JSDOM('<div id="root"></div>');
  const previous = { window: globalThis.window, document: globalThis.document, act: globalThis.IS_REACT_ACT_ENVIRONMENT };
  globalThis.window = dom.window; globalThis.document = dom.window.document; globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  const publications = [], pages = [];
  const dependencies = {
    // Keep the real dropdown so selection events exercise production form semantics.
    "~/components/forms/Select": { Select },
    // The workspace back link renders in a separate portal outside this focused component fixture.
    "~/components/layout/PageBackLink": { PageBackLink: () => null },
    "@remix-run/react": { Link: ({ to, children, ...rest }) => createElement("a", { href: to, ...rest }, children) },
    "~/components/access/CurrentActorAccess": { useCurrentActorAccess: () => ({ access: {} }) },
    "~/lib/current-actor-access": { hasResourcePermission: () => true },
    "~/lib/unified-template": template,
    "~/lib/unified-template-api": {
      // Capture pagination scope while returning enough rows to exercise version selection and races.
      listTemplateApps: async (search, offset, archived) => { pages.push({ search, offset, archived }); return { items: ["one", "two"].map(id => ({ app_family_id: id, app_id: id, name: id, version: "1.0.0" })), total: 21 }; },
      readTemplateApp: async id => source(id),
      listTemplateAppVersions: async id => [{ app_id: id, version: "1.0.0" }, { app_id: `${id}-old`, version: "0.9.0" }],
      checkTemplate: async () => ({ blockers: [], publicBlockers: [] }),
      ...overrides,
    },
    "~/lib/unified-app-api": { publishUnifiedTemplate: async (draft, isPublic) => { publications.push({ draft, isPublic }); return { id: "published" }; } },
  };
  const component = readFileSync(new URL("../components/apps/UnifiedTemplatePublisher.tsx", import.meta.url), "utf8");
  const compiled = ts.transpileModule(component, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX } }).outputText;
  const exports = {};
  // Real hooks and DOM events retain race behavior that static markup tests would miss.
  new Function("require", "exports", compiled)(name => dependencies[name] ?? require(name), exports);
  const root = createRoot(document.getElementById("root"));
  await act(async () => root.render(createElement(exports.UnifiedTemplatePublisher, { initialAppID: "", canCreate: true, onPublished: () => {}, ...props })));
  return {
    publications, pages,
    // Accessible field names avoid reliance on layout order.
    field: name => document.querySelector(`[aria-label="${name}"]`),
    // App cards include version labels, so prefix matching models their visible names.
    click: async label => { const button = [...document.querySelectorAll("button")].find(item => item.textContent.startsWith(label)); assert.ok(button, label); assert.equal(button.disabled, false, label); await act(async () => Simulate.click(button)); },
    change: async (element, value) => { await act(async () => Simulate.change(element, { target: { value } })); },
    submit: async () => { await act(async () => Simulate.submit(document.querySelector("form"))); },
    // Restore globals even when an assertion fails so sibling tests remain isolated.
    close: async () => { await act(async () => root.unmount()); dom.window.close(); globalThis.window = previous.window; globalThis.document = previous.document; globalThis.IS_REACT_ACT_ENVIRONMENT = previous.act; },
  };
}

// Review and publishing are separate submits; both use the selected historical version rather than latest source.
test("publisher selects versions, preserves metadata, reviews source, and publishes only portable fields", async () => {
  const ui = await mountPublisher();
  try {
    assert.equal(document.querySelector('a').getAttribute('href'), '/integrations/unified-apps/new?returnTo=publish-template');
    await ui.click("Next apps");
    assert.equal(ui.pages.at(-1).offset, 20);
    await ui.click("one");
    assert.equal(ui.field("Search your Unified Apps"), null);
    assert.equal(ui.field("Template name").value, "one");
    await ui.change(ui.field("App version"), "one-old");
    assert.equal(ui.field("Template name").value, "one-old");
    await ui.change(ui.field("Template name"), "Reviewed title");
    assert.equal(document.querySelector('a[download]'), null);
    assert.equal(ui.field("Import template JSON"), null);
    await ui.submit();
    assert.equal(ui.publications.length, 0);
    assert.ok(document.querySelector('details[open] pre').textContent.includes('one-old'));
    await ui.submit();
    assert.equal(ui.publications.length, 1);
    assert.equal(ui.publications[0].draft.name, "Reviewed title");
    assert.equal(ui.publications[0].isPublic, false);
    assert.doesNotMatch(JSON.stringify(ui.publications), /private-/);
  } finally { await ui.close(); }
});

// A slow private-source request cannot overwrite a newer app choice or leak its source into review.
test("late source responses are discarded after choosing another app", async () => {
  let finish;
  const ui = await mountPublisher({ readTemplateApp: id => id === "one" ? new Promise(resolve => { finish = () => resolve(source(id)); }) : Promise.resolve(source(id)) });
  try {
    await ui.click("one");
    await ui.click("Change app");
    await ui.click("two");
    assert.equal(ui.field("Template name").value, "two");
    await act(async () => finish());
    assert.equal(ui.field("Template name").value, "two");
    await ui.submit();
    assert.doesNotMatch(document.querySelector('pre').textContent, /one/);
  } finally { await ui.close(); }
});

// Mutable visibility must be rechecked at submission and must not erase edited template metadata.
test("public blockers prevent publication while private templates remain reviewable", async () => {
  const ui = await mountPublisher({ checkTemplate: async () => ({ blockers: [], publicBlockers: ["CRM v1: the service version is private."] }) }, { initialAppID: "one" });
  try {
    await ui.change(ui.field("Template name"), "Keep this title");
    const visibility = [...document.querySelectorAll('input[type="checkbox"]')].find(item => item.parentElement.textContent.includes("Make publicly"));
    await act(async () => Simulate.change(visibility, { target: { checked: true } }));
    assert.equal(document.querySelector('button[type="submit"]').disabled, true);
    await ui.submit();
    assert.equal(ui.publications.length, 0);
    assert.match(document.querySelector('[role="alert"]').textContent, /service version is private/);
    await ui.click("Retry loading and checks");
    assert.equal(ui.field("Template name").value, "Keep this title");
    await act(async () => Simulate.change(visibility, { target: { checked: false } }));
    await ui.submit();
    assert.equal(ui.publications.length, 0);
    await ui.submit();
    assert.equal(ui.publications.length, 1);
  } finally { await ui.close(); }
});

// A successful empty catalogue offers only the useful creation action, without search or JSON controls.
test("empty app catalogue shows one creation action without selection chrome or JSON", async () => {
  const ui = await mountPublisher({ listTemplateApps: async () => ({ items: [], total: 0 }) });
  try {
    assert.match(document.body.textContent, /Create your first Unified App/);
    assert.ok(document.querySelector('a[href$="returnTo=publish-template"]'));
    assert.equal(document.querySelector('details'), null);
    assert.equal(document.querySelector('input'), null);
    assert.equal(document.querySelector('nav'), null);
    assert.equal(document.querySelectorAll('a').length, 1);
    assert.doesNotMatch(document.body.textContent, /JSON|Previous apps|Next apps|archived/);
    assert.equal(document.querySelector('form'), null);
  } finally { await ui.close(); }
});
