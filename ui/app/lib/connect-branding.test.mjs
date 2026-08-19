import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { URL } from "node:url";

import {
  connectBrandingInput,
  connectBrandingConfirmationSummary,
  connectBrandingPreviewName,
  emptyConnectBrandingInput,
  normalizeConnectBrandingInput,
  safeLogoPreviewURL,
  safePrimaryColour,
  validateConnectBrandingInput,
} from "./connect-branding.ts";

test("provides a controlled form with a valid fallback colour", () => {
  assert.deepEqual(emptyConnectBrandingInput(), {
    display_name: "",
    logo_url: "",
    primary_color: "#2563eb",
    support_url: "",
    privacy_url: "",
  });
});

test("removes response metadata and trims values before update", () => {
  const response = {
    display_name: " Fused Support ",
    logo_url: " https://assets.example/logo.png ",
    primary_color: " #123abc ",
    support_url: " https://example.com/help ",
    privacy_url: " https://example.com/privacy ",
    created_at: "2026-08-19T00:00:00Z",
    updated_at: "2026-08-19T00:01:00Z",
  };
  const input = connectBrandingInput(response);

  assert.equal("created_at" in input, false);
  assert.deepEqual(normalizeConnectBrandingInput(input), {
    display_name: "Fused Support",
    logo_url: "https://assets.example/logo.png",
    primary_color: "#123abc",
    support_url: "https://example.com/help",
    privacy_url: "https://example.com/privacy",
  });
});

// This helper contract prevents confirmation data from carrying customer-entered names or URL values.
test("confirmation summaries contain only change and presence facts", () => {
  const current = {
    display_name: "Fused",
    logo_url: "",
    primary_color: "#2563eb",
    support_url: "https://example.com/help",
    privacy_url: "",
  };
  const next = {
    ...current,
    display_name: "Customer app",
    logo_url: "https://private.example/logo.png?token=secret",
    support_url: "",
  };
  const summary = connectBrandingConfirmationSummary(current, next);

  assert.deepEqual(summary, {
    displayNameChanged: true,
    primaryColorChanged: false,
    logoChanged: true,
    logoPresent: true,
    supportURLChanged: true,
    supportURLPresent: false,
    privacyURLChanged: false,
    privacyURLPresent: false,
  });
  assert.doesNotMatch(JSON.stringify(summary), /private\.example|Customer app|secret/);
});

test("accepts HTTPS branding URLs and leaves optional links empty", () => {
  const errors = validateConnectBrandingInput({
    display_name: "Acme",
    logo_url: "https://cdn.example.com/acme.svg?v=2",
    primary_color: "#AABBCC",
    support_url: "",
    privacy_url: "https://example.com/privacy",
  });

  assert.deepEqual(errors, {});
  assert.deepEqual(validateConnectBrandingInput({
    display_name: "Fused",
    logo_url: "",
    primary_color: "#2563eb",
    support_url: "",
    privacy_url: "",
  }), {});
  assert.equal(safeLogoPreviewURL(""), null);
});

test("rejects unsafe logo sources, credentials, and malformed colours", () => {
  const errors = validateConnectBrandingInput({
    display_name: "",
    logo_url: "javascript:alert(1)",
    primary_color: "red",
    support_url: "https://user:secret@example.com/help",
    privacy_url: "not a url",
  });

  assert.deepEqual(Object.keys(errors).sort(), [
    "display_name",
    "logo_url",
    "primary_color",
    "privacy_url",
    "support_url",
  ]);
  assert.equal(safeLogoPreviewURL("http://assets.example/logo.png"), null);
  assert.equal(
    safeLogoPreviewURL("https://assets.example/logo.png"),
    "https://assets.example/logo.png",
  );
  assert.equal(connectBrandingPreviewName("  "), "Your app");
  assert.equal(safePrimaryColour("red"), "#2563eb");
  assert.equal(safePrimaryColour("#112233"), "#112233");
});

test("matches Engine display-name rune and control-character limits", () => {
  const valid = {
    display_name: "😀".repeat(100),
    logo_url: "",
    primary_color: "#2563eb",
    support_url: "",
    privacy_url: "",
  };

  assert.equal(validateConnectBrandingInput(valid).display_name, undefined);
  assert.match(
    validateConnectBrandingInput({ ...valid, display_name: `${valid.display_name}a` }).display_name,
    /1 to 100 visible characters/,
  );
  assert.match(
    validateConnectBrandingInput({ ...valid, display_name: "Fused\u0000Connect" }).display_name,
    /control characters/,
  );
});

test("rejects URL control, length, host, and port edge cases locally", () => {
  const invalidURLs = [
    `https://example.com/${"a".repeat(2048)}`,
    "https://example.com/a\nb",
    "https://bad_host.example/logo.png",
    "https://example.com./logo.png",
    "https://example.com:0/logo.png",
    "https://example.com:70000/logo.png",
    "https://éxample.com/logo.png",
    "https:///logo.png",
    "https://exa%6dple.com/logo.png",
    "https://example.com\\@evil.com/logo.png",
  ];

  // Every representative edge must be blocked both during save and before preview loading.
  for (const logo_url of invalidURLs) {
    const errors = validateConnectBrandingInput({
      display_name: "Fused",
      logo_url,
      primary_color: "#2563eb",
      support_url: "",
      privacy_url: "",
    });
    assert.ok(errors.logo_url, `expected ${logo_url.slice(0, 40)} to fail`);
    assert.equal(safeLogoPreviewURL(logo_url), null);
  }

  assert.equal(
    validateConnectBrandingInput({
      display_name: "Fused",
      logo_url: "https://xn--xample-9ua.com/logo.png",
      primary_color: "#2563eb",
      support_url: "",
      privacy_url: "",
    }).logo_url,
    undefined,
  );
  assert.equal(
    validateConnectBrandingInput({
      display_name: "Fused",
      logo_url: "HTTPS://example.com/logo%20file.png?version=a%20b",
      primary_color: "#2563eb",
      support_url: "",
      privacy_url: "",
    }).logo_url,
    undefined,
  );
});

test("settings uses the Engine branding endpoint and a non-referring image preview", () => {
  const api = readFileSync(new URL("./api.ts", import.meta.url), "utf8");
  const settings = readFileSync(
    new URL("../routes/integrations.settings.tsx", import.meta.url),
    "utf8",
  );
  const card = readFileSync(
    new URL("../components/settings/ConnectBrandingCard.tsx", import.meta.url),
    "utf8",
  );

  assert.match(api, /req<ConnectBranding>\("\/workspace\/connect-branding"\)/);
  assert.match(api, /method: "PUT"/);
  assert.match(settings, /<ConnectBrandingCard \/>/);
  assert.match(card, /referrerPolicy="no-referrer"/);
  assert.doesNotMatch(card, /dangerouslySetInnerHTML/);
});

// This source contract keeps PUT behind confirmation and prevents confirmation markup from reading raw draft URLs.
test("branding save requires a safe confirmation before PUT", () => {
  const card = readFileSync(
    new URL("../components/settings/ConnectBrandingCard.tsx", import.meta.url),
    "utf8",
  );
  const submitStart = card.indexOf("function handleSubmit");
  const confirmStart = card.indexOf("function handleConfirmSave");
  const cancelStart = card.indexOf("function handleCancelSave");
  const submitHandler = card.slice(submitStart, confirmStart);
  const confirmHandler = card.slice(confirmStart, cancelStart);
  const promptStart = card.indexOf("function BrandingConfirmation");
  const promptEnd = card.indexOf("function changeLabel");
  const prompt = card.slice(promptStart, promptEnd);

  assert.doesNotMatch(submitHandler, /api\.connectBranding\.update/);
  assert.match(confirmHandler, /api\.connectBranding\.update\(pendingSave\)/);
  assert.match(prompt, /role="alertdialog"/);
  assert.match(prompt, /autoFocus/);
  assert.doesNotMatch(prompt, /draft\.(?:logo_url|support_url|privacy_url)/);
  assert.match(card, /if \(pendingSave \|\| saving\) return/);
  assert.match(card, /if \(!pendingSave \|\| saving\) return/);
  assert.equal([...card.matchAll(/\{content\}/g)].length, 1);
});
