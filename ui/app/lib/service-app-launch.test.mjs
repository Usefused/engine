import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import ts from 'typescript';
import { act, createElement } from 'react';
import { JSDOM } from 'jsdom';
import { serviceAppHref, findServiceAppSource } from './service-app-launch.ts';
import { hasWorkspacePermission } from './current-actor-permissions.ts';

/** Deep links preserve the chosen app kind and encode names without granting an operation scope. */
test('service launch links retain exact identity for all three builders', () => {
  const service = { id: 'exact-id', name: 'Mail & Calendar / personal' };
  for (const path of ['/integrations/builder?tab=mcp', '/integrations/builder?tab=sdk', '/integrations/unified-apps/new']) {
    const url = new URL(serviceAppHref(path, service), 'https://fused.example');
    assert.equal(url.searchParams.get('serviceId'), service.id);
    assert.equal(url.searchParams.get('serviceName'), service.name);
    assert.equal(url.searchParams.get('mode'), 'manual');
    assert.equal(url.pathname, path.split('?')[0]);
    assert.equal(url.searchParams.get('tab'), new URL(path, 'https://fused.example').searchParams.get('tab'));
    assert.equal(url.searchParams.has('operations'), false);
    assert.equal(serviceAppHref(path), path);
  }
});

/** Identical names and later selector pages must still resolve only the explicitly requested service. */
test('service launch checks every candidate by ID within authorized selector pages', async () => {
  const requested = { resource_id: 'chosen', display_name: 'Mail', resource_type: 'SERVICE' };
  const offsets = [];
  const found = await findServiceAppSource('chosen', async (limit, offset) => {
    // Simulate a partial page followed by the exact authorized service.
    offsets.push(offset); assert.equal(limit, 100);
    return { total: 2, items: offset === 0 ? [{ ...requested, resource_id: 'other' }] : [requested] };
  });
  assert.equal(found, requested);
  assert.deepEqual(offsets, [0, 1]);
  await assert.rejects(findServiceAppSource('chosen', async () => {
    // A readable public service is not necessarily in the app owner's usable workspace scope.
    return { total: 1, items: [{ ...requested, resource_id: 'other' }] };
  }), /not available for the selected app owner/);
  await assert.rejects(findServiceAppSource('chosen', async () => {
    // Stalled pagination must fail without looping indefinitely or inventing authorization.
    return { total: 2, items: [] };
  }), /not available for the selected app owner/);
});

/** Exercises actual disclosure links with per-kind creation grants and no backend mutations. */
test('Use in App offers permitted destinations and hides when none are allowed', async () => {
  const require = createRequire(import.meta.url);
  const dom = new JSDOM('<div id="root"></div>', { url: 'http://localhost' });
  const previous = { window: globalThis.window, document: globalThis.document, act: globalThis.IS_REACT_ACT_ENVIRONMENT };
  globalThis.window = dom.window; globalThis.document = dom.window.document; globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  const { createRoot } = await import('react-dom/client');
  let access = { subject_id: 'reader', workspace_id: 'workspace', grants: [] };
  const source = readFileSync(new URL('../components/apps/CreateAppMenu.tsx', import.meta.url), 'utf8');
  const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX } }).outputText;
  const exports = {};
  /** Stubs route navigation while retaining the menu's actual focus, permission, and dismissal logic. */
  function resolve(name) {
    const dependencies = {
      // Header styling is irrelevant to this permission and navigation contract.
      '~/components/layout/CataloguePageHeader': { catalogueActionClassName: '' },
      '~/components/access/CurrentActorAccess': { useCurrentActorAccess: () => { /* Supply the currently tested actor. */ return { access }; } },
      '~/lib/current-actor-access': { hasWorkspacePermission },
      '~/lib/service-app-launch': { serviceAppHref },
      '@remix-run/react': { Link: require('react').forwardRef(function Link({ to, children, ...props }, ref) {
        // Inspect native link destinations without starting an unrelated router navigation.
        return createElement('a', { ...props, href: to, ref }, children);
      }) },
    };
    // Application aliases are replaced explicitly; React and icons remain production dependencies.
    return dependencies[name] ?? require(name);
  }
  new Function('require', 'exports', compiled)(resolve, exports);
  const container = document.getElementById('root');
  const root = createRoot(container);
  const service = { id: 'mail-id', name: 'Mail' };
  /** Updates the permission snapshot and mounts the real service menu. */
  async function render(kinds) {
    access = { ...access, grants: kinds.map((kind) => {
      // Each destination needs its independent workspace creation grant.
      return { permission: `app.${kind}.create`, resource_type: 'WORKSPACE', resource_id: 'workspace' };
    }) };
    await act(async () => { /* Flush React permission changes before inspecting destinations. */ root.render(createElement(exports.CreateAppMenu, { service })); });
  }
  try {
    await render(['sdk', 'mcp', 'unified_app', 'api']);
    assert.match(container.textContent, /Use in App/);
    await act(async () => { /* Open through the same button the service page renders. */ container.querySelector('button').click(); });
    const links = [...container.querySelectorAll('[role="menuitem"]')];
    assert.equal(links.length, 3);
    assert.deepEqual(links.map((link) => link.querySelector('.font-semibold').textContent), ['MCP', 'Unified App', 'SDK']);
    for (const link of links) assert.equal(new URL(link.href).searchParams.get('serviceId'), 'mail-id');
    await render(['sdk']);
    assert.equal(container.querySelectorAll('[role="menuitem"]').length, 1);
    assert.match(container.querySelector('a').textContent, /SDK/);
    await render([]);
    assert.equal(container.querySelector('button'), null);
  } finally {
    await act(async () => { /* Dispose dismissal listeners before restoring the host document. */ root.unmount(); });
    dom.window.close();
    globalThis.window = previous.window; globalThis.document = previous.document; globalThis.IS_REACT_ACT_ENVIRONMENT = previous.act;
  }
});
