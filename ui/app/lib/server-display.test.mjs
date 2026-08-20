import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { URL } from "node:url";

const componentSource = readFileSync(
  new URL("../components/integration-details/ServerDisplay.tsx", import.meta.url),
  "utf8"
);

// This contract keeps complete primary and alternate URLs available to mouse
// and keyboard users even though their visible labels remain truncated.
test("server URLs expose exact tooltip and copy labels", () => {
  assert.match(componentSource, /title=\{url\}/);
  assert.match(componentSource, /aria-label=\{`Copy URL \$\{url\}`\}/);
  assert.match(componentSource, /title=\{server\.url\}/);
  assert.match(componentSource, /aria-label=\{`Copy URL \$\{server\.url\}`\}/);
  assert.doesNotMatch(componentSource, /title="Click to copy URL"/);
});

// This contract keeps both URL controls on the shared copy path so feedback
// and picker-closing behavior do not drift between environments.
test("server URL controls share one copy implementation", () => {
  assert.match(componentSource, /function copyServerURL\(url: string, closeDropdown = false\)/);
  assert.match(componentSource, /onCopy=\{copyServerURL\}/);
  assert.match(componentSource, /onCopy=\{\(url\) => copyServerURL\(url, true\)\}/);
});
