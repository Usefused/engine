import assert from 'node:assert/strict';
import test from 'node:test';
import { createRequire } from 'node:module';
import { build } from 'esbuild';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { JSDOM } from 'jsdom';

// Compile the actual renderer, retaining React's shared instance for server-side hooks.
const compiled = await build({ entryPoints: [new URL('../components/agent/FusedAgentMarkdown.tsx', import.meta.url).pathname], bundle: true, write: false, platform: 'node', format: 'cjs', jsx: 'automatic', external: ['react', 'react/jsx-runtime'] });
const module = { exports: {} };
new Function('require', 'module', 'exports', compiled.outputFiles[0].text)(createRequire(import.meta.url), module, module.exports);
const { FusedAgentMarkdown } = module.exports;

/** Reads the production Markdown output as DOM so semantic boundaries and escaped code are verified. */
function render(text) {
  return new JSDOM(renderToStaticMarkup(React.createElement(FusedAgentMarkdown, { text }))).window.document;
}

// Copy controls belong only to complete code blocks; inline snippets retain their sentence context.
test('fenced code preserves indentation, highlights and provides one copy action', () => {
  const source = 'const name = "Fused";\n  console.log(name);\n';
  const doc = render('Use `name`:\n\n```ts\n' + source + '```');
  assert.equal(doc.querySelector('pre code').textContent, source);
  assert.equal(doc.querySelectorAll('button[aria-label="Copy code"]').length, 1);
  assert.ok(doc.querySelector('pre code span'));
  assert.equal(doc.querySelector('p code').textContent, 'name');
  assert.equal(doc.querySelector('[role="region"]').getAttribute('tabindex'), '0');
});

// Unknown languages and streaming fences must still render safe, readable source without dropping bytes.
test('unknown and unfinished fences render escaped text', () => {
  const source = '<script>alert("example")</script>\n';
  const doc = render('```custom\n' + source);
  assert.equal(doc.querySelector('pre code').textContent, source);
  assert.equal(doc.querySelector('script'), null);
  assert.equal(doc.querySelector('[role="region"]').getAttribute('aria-label'), 'custom code');
});

// Model-provided HTML, remote images and unsafe URLs must not acquire new privileges through rich rendering.
test('rich code rendering preserves Markdown safety boundaries', () => {
  const doc = render('<script>alert(1)</script>\n\n![alt](https://example.test/track)\n\n[bad](javascript:alert%281%29)');
  assert.equal(doc.querySelector('script, img'), null);
  assert.notEqual(doc.querySelector('a')?.getAttribute('href'), 'javascript:alert%281%29');
  assert.match(doc.body.textContent, /alt/);
});

// Go package examples should preserve source and use the registered grammar for either conventional label.
test('Go SDK code fences are highlighted without changing their contents', () => {
  for (const language of ['go', 'golang']) {
    const source = 'package main\n\nfunc main() { println("Fused") }\n';
    const doc = render('```' + language + '\n' + source + '```');
    assert.equal(doc.querySelector('pre code').textContent, source);
    assert.ok(doc.querySelector('pre code span'));
  }
});
