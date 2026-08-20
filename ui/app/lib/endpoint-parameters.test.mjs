import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { URL } from "node:url";

const sidebarSource = readFileSync(
  new URL("../components/EndpointDetailsSidebar.tsx", import.meta.url),
  "utf8"
);
const parameterSection = sidebarSource.slice(
  sidebarSource.indexOf("type EndpointParameter"),
  sidebarSource.indexOf("// RequestSchemaSection")
);

// This contract keeps the parameter table responsive and prevents ordinary
// parameters from displaying a redundant default encoding column.
test("parameter table fits the sidebar without a default encoding column", () => {
  assert.doesNotMatch(parameterSection, /min-w-\[640px\]|overflow-x-auto/);
  assert.doesNotMatch(parameterSection, /["']Encoding["']|path_encoding \|\| ["']default["']/);
  assert.match(parameterSection, /table-fixed/);
  assert.match(parameterSection, /break-all/);
  assert.match(parameterSection, />Location</);
  assert.match(parameterSection, />Requirement</);
});

// This contract retains meaningful non-default path serialization details in
// readable copy rather than silently dropping an execution-sensitive rule.
test("parameter rows retain explicit path encoding and required state", () => {
  assert.match(parameterSection, /pathEncoding === ["']preserve_slashes["']/);
  assert.match(parameterSection, /Preserves slashes/);
  assert.match(parameterSection, /Required/);
  assert.match(parameterSection, /Optional/);
  assert.match(parameterSection, /title=\{parameter\.name\}/);
});
