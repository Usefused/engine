import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import ts from 'typescript';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { hasResourcePermission } from './current-actor-permissions.ts';

const require = createRequire(import.meta.url);
let access = null;
let calls = 0;
/** Loads production components with only the access snapshot and issuance transport replaced. */
function load(path, dependencies = {}) {
  const source = readFileSync(new URL(path, import.meta.url), 'utf8');
  const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX } }).outputText;
  const exports = {};
  new Function('require', 'exports', compiled)(name => dependencies[name] ?? require(name), exports);
  return exports;
}
const { AppExecutionTokens } = load('../components/apps/AppExecutionTokens.tsx', {
  '~/lib/api': { api: { appTokens: { generate: async () => { calls++; throw new Error('Unexpected issuance'); } } } },
  '~/lib/current-actor-access': { hasResourcePermission },
  '~/components/access/CurrentActorAccess': { useCurrentActorAccess: () => ({ access, loading: false, failed: false }) },
  '~/components/CopyValue': load('../components/CopyValue.tsx'),
  '~/components/forms/FieldLabel': load('../components/forms/FieldLabel.ts'),
  '~/components/forms/Select': load('../components/forms/Select.ts'),
});

// App managers, sibling-family grants, and another app type's token grants must never unlock issuance.
test('execution token controls require the exact typed family permission', () => {
  for (const kind of ['sdk', 'mcp', 'api', 'unified_app']) {
    for (const grant of [null,
      { permission: `app.${kind}.manage`, resource_type: 'APP', resource_id: 'family' },
      { permission: `app.${kind}.tokens.manage`, resource_type: 'APP', resource_id: 'sibling' },
      { permission: 'app.webhook.tokens.manage', resource_type: 'APP', resource_id: 'family' },
      { permission: `app.${kind}.tokens.manage`, resource_type: 'WORKSPACE', resource_id: 'other-workspace' },
    ]) {
      access = { subject_id: 'reader', workspace_id: 'workspace', grants: grant ? [grant] : [] };
      const html = renderToStaticMarkup(createElement(AppExecutionTokens, { familyID: 'family', kind }));
      assert.doesNotMatch(html, /<button/);
      assert.match(html, /need permission/);
    }
    for (const resource_type of ['APP', 'WORKSPACE']) {
      access = { subject_id: 'manager', workspace_id: 'workspace', grants: [{ permission: `app.${kind}.tokens.manage`, resource_type, resource_id: resource_type === 'APP' ? 'family' : 'workspace' }] };
      const html = renderToStaticMarkup(createElement(AppExecutionTokens, { familyID: 'family', kind }));
      assert.match(html, /Generate token/);
      assert.doesNotMatch(html, /<form/);
    }
  }
  assert.equal(calls, 0, 'rendering any detail page must never issue a token');
});
