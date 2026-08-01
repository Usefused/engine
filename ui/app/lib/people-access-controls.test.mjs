import assert from "node:assert/strict";
import test from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { PersonalCredentialPanel } from "../components/access/PersonalCredentialPanel.ts";
import { TeamMembersControls } from "../components/access/TeamMembersControls.ts";

test("personal key component shows a raw key once with an explicit warning", () => {
  const html = renderToStaticMarkup(createElement(PersonalCredentialPanel, {
    credentials: [{ id: "credential-1", name: "Laptop", key_prefix: "fsk_abcd", expires_at: null, last_used_at: "2026-08-01T12:00:00Z", revoked_at: null, created_at: "now" }],
    truncated: true,
    issuedSecret: "fsk_once_only",
    onIssue() {},
    onRevoke() {},
    onClearSecret() {},
  }));
  assert.equal((html.match(/fsk_once_only/g) || []).length, 1);
  assert.match(html, /will not be shown again/i);
  assert.match(html, /Keep it secret/i);
  assert.match(html, /fsk_abcd/);
  assert.match(html, /Last used/i);
  assert.match(html, /100 most recent personal keys/i);
  assert.match(html, /Revoke/);
});

test("team members component uses nontechnical add-by-email controls", () => {
  const html = renderToStaticMarkup(createElement(TeamMembersControls, {
    members: [{ user_id: "user-1", email: "ada@example.test", display_name: "Ada", status: "ACTIVE", membership_role: "MANAGER", created_at: "now" }],
    onAdd() {},
    onRemove() {},
  }));
  for (const label of ["Add by email", "Member", "Manager", "ada@example.test", "Remove"]) {
    assert.match(html, new RegExp(label, "i"));
  }
  assert.doesNotMatch(html, /permission|binding|subject/i);
});

test("personal key component distinguishes expired credentials from active ones", () => {
  const html = renderToStaticMarkup(createElement(PersonalCredentialPanel, {
    credentials: [{ id: "credential-expired", name: "Old laptop", key_prefix: "fsk_old", expires_at: "2020-01-01T00:00:00Z", last_used_at: null, revoked_at: null, created_at: "now" }],
    issuedSecret: null,
    onIssue() {},
    onRevoke() {},
    onClearSecret() {},
  }));
  assert.match(html, /Expired/);
  assert.doesNotMatch(html, /fsk_old… · Active/);
});
