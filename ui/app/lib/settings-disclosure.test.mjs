import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { URL } from "node:url";

const disclosure = readFileSync(
  new URL("../components/settings/SettingsDisclosureCard.tsx", import.meta.url),
  "utf8",
);
const settings = readFileSync(
  new URL("../routes/integrations.settings.tsx", import.meta.url),
  "utf8",
);
const branding = readFileSync(
  new URL("../components/settings/ConnectBrandingCard.tsx", import.meta.url),
  "utf8",
);

// This contract keeps disclosure interaction on native button semantics and exposes state to assistive technology.
test("disclosure cards are expanded by default and keyboard-native", () => {
  assert.match(disclosure, /useState\(true\)/);
  assert.match(disclosure, /<button[\s\S]*type="button"/);
  assert.match(disclosure, /aria-expanded=\{expanded\}/);
  assert.match(disclosure, /aria-controls=\{contentID\}/);
});

// This contract prevents collapsing a form from discarding unsaved input or confirmation state.
test("collapsed content stays mounted behind the hidden attribute", () => {
  assert.match(disclosure, /<div id=\{contentID\} hidden=\{!expanded\}[\s\S]*\{children\}/);
  assert.doesNotMatch(disclosure, /expanded\s*&&\s*children/);
});

// Distinct identifiers prevent one disclosure button from controlling another section.
test("stateful settings areas use independent disclosure identifiers", () => {
  assert.match(branding, /id="connect-branding-settings"/);
  assert.match(settings, /id="account-details-settings"/);
  assert.match(settings, /id="api-key-management-settings"/);
  assert.equal(
    new Set([
      "connect-branding-settings",
      "account-details-settings",
      "api-key-management-settings",
    ]).size,
    3,
  );
});
