import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import ts from "typescript";
import { act, createElement, useState } from "react";
import { createRoot } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import { JSDOM } from "jsdom";
const require = createRequire(import.meta.url);

/** Loads the actual route and catalogue components with transport, shared popover chrome, and app-shell boundaries replaced. */
function loadUI(file, dependencies) {
  const source = readFileSync(new URL(file, import.meta.url), "utf8");
  const code = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, jsx: ts.JsxEmit.ReactJSX } }).outputText;
  const exports = {};
  new Function("require", "exports", code)(name => dependencies[name] ?? require(name), exports);
  return exports;
}

/** Uses mixed visibility releases to exercise inspection without changing public discoverability. */
function release(id, isPublic) {
  return { id, public: isPublic, is_owner: true, publisher: "preview", template: { name: id, version: "1.0.0", category: "", description: "A reusable app", requirements: [], services: {}, source: "export default buildUnifiedApp({});" } };
}

// Real navigation and inspection must keep both visibility states together across search and keep code out of the catalogue.
test("catalogue preserves private filtering across pages and resets paging for searches", async () => {
  const dom = new JSDOM('<div id="root"></div>');
  const before = { window: globalThis.window, document: globalThis.document, act: globalThis.IS_REACT_ACT_ENVIRONMENT };
  globalThis.window = dom.window; globalThis.document = dom.window.document; globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  // JSDOM lacks native dialog behavior; live-browser checks cover focus and Escape handling.
  dom.window.HTMLDialogElement.prototype.showModal = function () { this.setAttribute("open", ""); };
  dom.window.HTMLDialogElement.prototype.close = function () { this.removeAttribute("open"); };
  // More than two pages makes slicing before filtering observable.
  const calls = [], rows = [release("Public starter", true), release("Private starter", false), ...Array.from({ length: 43 }, (_, index) => release(`Example ${index}`, index % 2 === 0))];
  const remix = {
    Link: ({ to, children, ...props }) => createElement("a", { href: to, ...props }, children),
    // Route URL updates stay real React state so effect ordering and scope preservation are exercised.
    useSearchParams: () => { const [params, setParams] = useState(new URLSearchParams("visibility=private")); return [params, next => setParams(new URLSearchParams(next))]; },
  };
  const components = loadUI("../components/apps/UnifiedTemplateCatalogue.tsx", { "@remix-run/react": remix });
  const route = loadUI("../routes/integrations.unified-apps.templates.tsx", {
    "@remix-run/react": remix,
    "~/components/forms/Select": loadUI("../components/forms/Select.ts", {}),
    "~/components/forms/AnchoredPopover": { AnchoredPopover: ({ children }) => children },
    "~/components/layout/CataloguePageHeader": loadUI("../components/layout/CataloguePageHeader.tsx", {}),
    "~/components/layout/PageBackLink": { PageBackLink: () => null },
    "~/components/apps/UnifiedTemplatePublisher": { UnifiedTemplatePublisher: () => createElement("p", {}, "Publish an existing app") },
    "~/components/apps/UnifiedTemplateCatalogue": components,
    "~/components/access/CurrentActorAccess": { useCurrentActorAccess: () => ({ access: {} }) },
    "~/lib/current-actor-access": { hasWorkspacePermission: () => true },
    "~/components/Toast": { useToast: () => ({ success() {} }) },
    "~/lib/unified-app-api": {
      // Model server-side filtering and paging so the route must preserve the actual request state.
      listUnifiedTemplates: async (search, offset, ids, privateOnly) => {
        calls.push({ search, offset, privateOnly });
        const matches = rows.filter(row => (!privateOnly || !row.public) && row.template.name.toLowerCase().includes(search.toLowerCase()));
        return { items: matches.slice(offset, offset + 20), total: matches.length };
      },
    },
  }).default;
  const root = createRoot(document.getElementById("root"));
  // Accessible labels exercise the same controls used by keyboard and browser users.
  const click = async label => { const button = [...document.querySelectorAll("button")].find(item => (item.getAttribute("aria-label") ?? item.textContent) === label); assert.ok(button, label); await act(async () => Simulate.click(button)); };
  try {
    await act(async () => root.render(createElement(route)));
    assert.equal(document.querySelectorAll("article").length, 20);
    assert.equal(document.querySelector("pre"), null);
    await click("View template: Private starter");
    assert.ok(document.querySelector("dialog[open]"));
    assert.equal(document.querySelector("dialog details").open, false);
    assert.equal(document.querySelectorAll("article").length, 20);
    await click("Close template details");
    assert.equal(document.querySelector("dialog"), null);
    assert.equal(document.querySelector('nav[aria-label="Template library"]'), null);
    assert.deepEqual(calls.at(-1), { search: "", offset: 0, privateOnly: false });
    assert.match(document.querySelector('nav[aria-label="Template pages"]').textContent, /1–20 of 45/);
    await click("Next page");
    assert.deepEqual(calls.at(-1), { search: "", offset: 20, privateOnly: false });
    // Selecting the last page must use its server offset rather than reslicing the first page.
    await act(async () => Simulate.change(document.querySelector('select[aria-label="Template page"]'), { target: { value: "3" } }));
    assert.equal(document.querySelectorAll("article").length, 5);
    assert.equal(document.querySelector('[aria-label="Next page"]').disabled, true);
    const filter = document.querySelector('input[type="checkbox"]');
    await act(async () => Simulate.change(filter, { target: { checked: true } }));
    assert.deepEqual(calls.at(-1), { search: "", offset: 0, privateOnly: true });
    assert.match(document.querySelector('nav[aria-label="Template pages"]').textContent, /1–20 of 22/);
    await click("Next page");
    assert.deepEqual(calls.at(-1), { search: "", offset: 20, privateOnly: true });
    assert.equal(document.querySelectorAll("article").length, 2);
    const input = document.querySelector('input[aria-label="Search Unified App templates"]');
    await act(async () => Simulate.change(input, { target: { value: "starter" } }));
    await act(async () => Simulate.submit(document.querySelector('form')));
    assert.deepEqual(calls.at(-1), { search: "starter", offset: 0, privateOnly: true });
    assert.equal(document.querySelectorAll("article").length, 1);
    assert.equal(document.querySelector('[aria-label="Previous page"]').disabled, true);
    assert.equal(document.querySelector('[aria-label="Next page"]').disabled, true);
    // Clearing the simple filter restores both visibilities while preserving the search.
    await act(async () => Simulate.change(filter, { target: { checked: false } }));
    assert.deepEqual(calls.at(-1), { search: "starter", offset: 0, privateOnly: false });
    assert.equal(document.querySelectorAll("article").length, 2);
    await click("New");
    assert.ok(document.querySelector('[aria-label="New template or app"]'));
    assert.equal(document.querySelector('a[href="/integrations/unified-apps/new"]').textContent, "Create app");
    await click("Publish template");
    assert.match(document.body.textContent, /Publish an existing app/);

  } finally {
    await act(async () => root.unmount()); dom.window.close();
    globalThis.window = before.window; globalThis.document = before.document; globalThis.IS_REACT_ACT_ENVIRONMENT = before.act;
  }
});
