import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import test from "node:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import ts from "typescript";

const require = createRequire(import.meta.url);
const source = readFileSync(new URL("../components/ImportDiagnostics.tsx", import.meta.url), "utf8");
const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, jsx: ts.JsxEmit.ReactJSX } }).outputText;
const exported = {};
new Function("exports", "require", compiled)(exported, require);

// Rendering the real component proves corrections and unknown schemas are visually distinct.
test("shows corrected and unspecified schemas without presenting unknown information as fixed", () => {
  const diagnostics = [
    { code: "source_scalar_body_required", disposition: "repaired", message: "Moved the required flag.", pointer: "/paths/~1cover/put/requestBody", correction: { rule: "scalar_body_required", version: "1" } },
    { code: "source_missing_error_schema", disposition: "incomplete", message: "Error body remains unspecified.", correction: { rule: "missing_error_schema", version: "1" } },
    { code: "other_warning", message: "<script>provider example</script>" },
  ];
  const html = renderToStaticMarkup(React.createElement(exported.ImportDiagnostics, { diagnostics }));
  assert.match(html, /1 corrected · 1 unspecified/);
  assert.match(html, /Corrected/);
  assert.match(html, /Unspecified/);
  assert.match(html, /View source location/);
  assert.match(html, /&lt;script&gt;/);
  assert.doesNotMatch(html, /<script>/);
});

// Empty plans and older servers do not introduce a blank review panel.
test("hides an empty or absent diagnostics panel", () => {
  assert.equal(renderToStaticMarkup(React.createElement(exported.ImportDiagnostics)), "");
  assert.equal(renderToStaticMarkup(React.createElement(exported.ImportDiagnostics, { diagnostics: [] })), "");
});

// A report-only rejection must never be displayed as a completed correction.
test("report-only renders suggested corrections and retains a safe default selector", () => {
  const html = renderToStaticMarkup(React.createElement(exported.ImportDiagnostics, { diagnostics: [{ code: "repair", disposition: "suggested", message: "Fix the source", correction: { rule: "required", version: "1" } }] }));
  assert.match(html, /Suggested correction/);
  assert.doesNotMatch(html, />Corrected</);
  const control = renderToStaticMarkup(React.createElement(exported.ImportRepairMode, { onChange() {} }));
  assert.match(control, /value="safe" selected/);
  assert.match(control, /Report only/);
});

// Nested API diagnostics remain available after normalization for rejected plans.
test("extracts structured report-only diagnostics without interpreting provider content", () => {
  const expected = { message: "Change this field", code: "repair", disposition: "suggested" };
  assert.deepEqual(exported.importErrorDiagnostics({ details: { diagnostics: [expected, null, "bad"] } }), [expected]);
  assert.deepEqual(exported.importErrorDiagnostics(new Error("network")), []);
});
