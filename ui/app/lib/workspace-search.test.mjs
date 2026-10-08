import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import ts from 'typescript';
import { act, createElement, useState, useCallback } from 'react';
import { createRoot } from 'react-dom/client';
import { JSDOM } from 'jsdom';
const require = createRequire(import.meta.url);

// Exercise route effects and URL changes with only presentation and remote transport replaced.
test('workspace search handles partial text, clearing, pagination, history and stale responses', async () => {
  const dom = new JSDOM('<div id="root"></div>');
  const previous = { window: globalThis.window, document: globalThis.document, act: globalThis.IS_REACT_ACT_ENVIRONMENT };
  globalThis.window = dom.window; globalThis.document = dom.window.document; globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  let props, params, navigate, hold = false;
  const requests = [], pending = [], catalogQueries = [];
  // Retain URL state through real React renders, including navigation initiated outside the search field.
  function useSearchParams() {
    const [value, setValue] = useState(new URLSearchParams('page=2'));
    params = value; navigate = setValue;
    const update = useCallback(next => setValue(current => typeof next === 'function' ? next(current) : next), []);
    return [value, update];
  }
  // Capture the production route's list contract without importing unrelated card and modal UI.
  function List(value) {
    props = value;
    return createElement('div', { 'data-service-list': value.viewType });
  }
  // Unused drawers remain inert so the regression focuses on search orchestration.
  function Hidden() { return null; }
  const dependencies = {
    '@remix-run/react': { useSearchParams, useNavigate: () => () => {}, useRouteLoaderData: () => ({ isAuth: true }) },
    '~/lib/api': { api: {
      // Registry matches include an enabled service and one available to add; the UI retains membership markers.
      graphql: async (_document, { q }) => {
        catalogQueries.push(q);
        return { searchServices: [{ id: 'demo', name: 'Demo CRM' }, { id: 'available', name: 'Demo Billing' }] };
      },
      workspace: {
      // Membership is intentionally stable so result emptiness cannot change catalogue defaults.
      getServiceIds: async () => ['demo'],
      // Deferred pages reproduce the older-request-finishes-last failure deterministically.
      getServicesPage: async (limit, offset, names, q) => {
        requests.push({ limit, offset, names, q });
        const data = Array.from({ length: 25 }, (_, index) => ({ id: `local-${index}`, name: `${q || 'browse'}:${index}` }));
        const result = { data: data.slice(offset, offset + limit), total: data.length };
        if (hold) return new Promise(resolve => pending.push(() => resolve(result)));
        return result;
      },
    } } },
    '~/components/IntegrationsListTab': { default: List, fromActivatedService: value => value, fromService: value => value },
    '~/components/ExtractionWizard': { default: Hidden },
    '~/components/IntegrationsPendingTab': { default: Hidden },
    '~/components/DefineServiceDrawer': { DefineServiceDrawer: Hidden },
    '~/components/Toast': { useToast: () => ({}) },
    '~/lib/authorization-error': {},
    '~/lib/discovery-navigation': { discoveryNavigationFromQuery: () => ({ sessionID: null }) },
  };
  const source = readFileSync(new URL('../routes/integrations._index.tsx', import.meta.url), 'utf8');
  const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX } }).outputText;
  const exports = {};
  new Function('require', 'exports', compiled)(name => dependencies[name] ?? require(name), exports);
  const root = createRoot(document.getElementById('root'));
  // Flush the real debounce rather than bypassing the typing path with form submission.
  async function type(query) {
    await act(async () => props.setQuery(query));
    await act(async () => new Promise(resolve => setTimeout(resolve, 450)));
  }
  try {
    await act(async () => root.render(createElement(exports.default)));
    assert.equal(requests.at(-1).offset, 10);
    assert.equal(catalogQueries.length, 0, 'established workspaces keep catalogue browsing collapsed');
    assert.equal(props.searchPlaceholder, 'Search workspace and catalog');
    await type('DeMo');
    assert.deepEqual(requests.at(-1), { limit: 10, offset: 0, names: undefined, q: 'DeMo' });
    assert.equal(params.get('page'), '1');
    assert.equal(catalogQueries.at(-1), 'DeMo', 'one search automatically queries Registry as well as Engine');
    assert.equal(props.viewType, 'search');
    assert.equal(document.querySelectorAll('[data-service-list]').length, 1, 'both sources share one result list');
    assert.equal(props.totalItems, 26, 'existing Registry membership is not counted twice');
    assert.deepEqual(props.activeServiceIds, ['demo']);
    assert.equal(document.querySelector('[role="switch"]'), null, 'search does not require a separate catalogue toggle');
    await act(async () => props.onPageChange(2));
    assert.equal(requests.at(-1).offset, 10); assert.equal(requests.at(-1).q, 'DeMo');
    assert.equal(props.integrations.length, 10);
    await act(async () => props.onPageChange(3));
    assert.equal(props.integrations.length, 6, 'Registry matches fill the final workspace page');
    assert.equal(props.integrations[5].id, 'available');
    await type('');
    assert.equal(params.has('q'), false); assert.equal(requests.at(-1).q, ''); assert.equal(requests.at(-1).offset, 0);
    assert.equal(document.querySelector('[role="switch"]').getAttribute('aria-checked'), 'false', 'clearing search restores collapsed browsing');
    await act(async () => navigate(new URLSearchParams('q=slug&page=2')));
    assert.equal(props.query, 'slug'); assert.equal(requests.at(-1).offset, 10);
    hold = true;
    await type('old'); await type('new');
    assert.equal(pending.length, 2);
    await act(async () => pending[1]());
    assert.equal(props.integrations[0].name, 'new:0');
    await act(async () => pending[0]());
    assert.equal(props.integrations[0].name, 'new:0');
    assert.equal(props.hideEmptyState, false, 'search misses must have a visible empty state');
  } finally {
    await act(async () => root.unmount()); dom.window.close();
    globalThis.window = previous.window; globalThis.document = previous.document; globalThis.IS_REACT_ACT_ENVIRONMENT = previous.act;
  }
});

// The actual list must expose navigation for filtered workspace pages, not merely fetch them in route state.
test('workspace search keeps pagination controls visible', async () => {
  const { renderToStaticMarkup } = await import('react-dom/server');
  const source = readFileSync(new URL('../components/IntegrationsListTab.tsx', import.meta.url), 'utf8') + '\nexport { IntegrationPagination };';
  const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX } }).outputText;
  const exports = {};
  // Native select markup is enough to assert the page options without loading styling helpers.
  const dependencies = { './forms/Select.ts': { Select: 'select' }, '@remix-run/react': {}, '~/lib/api': {}, '~/lib/format': {}, '~/lib/service-navigation': {}, '~/components/ServiceIcon': {} };
  new Function('require', 'exports', compiled)(name => dependencies[name] ?? require(name), exports);
  const props = { loading: false, query: 'stripe', viewType: 'workspace', totalPages: 3, totalItems: 25, page: 2, onPageChange: () => {} };
  const html = renderToStaticMarkup(createElement(exports.IntegrationPagination, props));
  assert.match(html, /11-20 of 25/); assert.match(html, /aria-label="Next page"/);
  assert.match(renderToStaticMarkup(createElement(exports.IntegrationPagination, { ...props, viewType: 'search' })), /11-20 of 25/);
  assert.equal(renderToStaticMarkup(createElement(exports.IntegrationPagination, { ...props, viewType: 'catalog' })), '');
});
