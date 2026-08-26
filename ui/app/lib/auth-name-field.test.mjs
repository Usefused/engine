import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { AuthNameField, copyAuthName } from "../components/AuthNameField.ts";
import { StoredSecretKeys } from "../components/buckets/StoredSecretKeys.ts";

// The primary service view must identify the scheme without mistaking its name for a credential key.
test("auth names are explicit, exact, and copyable", async () => {
  const html = renderToStaticMarkup(createElement(AuthNameField, { name: "OAuth2" }));
  assert.match(html, /Authentication scheme name/);
  assert.match(html, /auth\.name/);
  assert.match(html, />OAuth2<\/code>/);
  assert.match(html, /aria-label="Copy auth name"/);
  assert.match(html, /data-track="copy_auth_name"/);
  const values = [];
  const status = await copyAuthName("OAuth2", { writeText: async (value) => values.push(value) });
  assert.deepEqual(values, ["OAuth2"]);
  assert.equal(status, "Auth name copied.");
});

// Bucket metadata names the persisted binding while retaining the same case-sensitive scheme identifier.
test("bucket auth names describe the stored binding rather than the scheme definition", () => {
  const html = renderToStaticMarkup(createElement(AuthNameField, { name: "OAuth2", context: "bucket" }));
  assert.match(html, /Stored auth name/);
  assert.match(html, /auth_name/);
  assert.match(html, />OAuth2<\/code>/);
  assert.match(html, /aria-label="Copy auth name"/);
  assert.doesNotMatch(html, /Authentication scheme name|Auth key name/);
});

// Legacy missing metadata cannot become a guessed OAuth name or a misleading copy action.
test("missing auth names are shown without a copy button", () => {
  for (const name of [undefined, null, ""]) {
    const html = renderToStaticMarkup(createElement(AuthNameField, { name }));
    assert.match(html, /Not provided/);
    assert.doesNotMatch(html, /<button/);
  }
});

// Provider metadata must be rendered as text even when it resembles HTML.
test("auth names are escaped without changing the clipboard value", async () => {
  const name = '<OAuth2 & "exact">';
  const html = renderToStaticMarkup(createElement(AuthNameField, { name }));
  assert.doesNotMatch(html, /<OAuth2/);
  assert.match(html, /&lt;OAuth2/);
  let copied;
  await copyAuthName(name, { writeText: async (value) => { copied = value; } });
  assert.equal(copied, name);
});

// Clipboard denial must remain honest and never echo browser errors or the copied identifier.
test("clipboard failures offer manual copying without reporting success", async () => {
  assert.match(await copyAuthName("OAuth2"), /Copy unavailable/);
  assert.match(await copyAuthName(""), /Copy unavailable/);
  const result = await copyAuthName("OAuth2", { writeText: async () => { throw new Error("private browser detail"); } });
  assert.match(result, /Copy failed/);
  assert.doesNotMatch(result, /copied|private browser detail|OAuth2/);
});

// The UI must consume the existing batched field and wire both active surfaces to the shared renderer.
test("service and bucket views expose the same auth identity without extra fetches", () => {
  const service = readFileSync(new URL("../routes/integrations.$id.tsx", import.meta.url), "utf8");
  const bucket = readFileSync(new URL("../components/buckets/BucketConnectedUsersTable.tsx", import.meta.url), "utf8");
  const queries = readFileSync(new URL("./buckets.ts", import.meta.url), "utf8");
  assert.match(service, /Authentication schemes · auth\.name/);
  assert.match(service, /<AuthNameField name=\{auth\.name\}/);
  assert.match(service, /label="HTTP scheme"/);
  assert.match(service, /label="Request key"/);
  assert.match(bucket, /<AuthNameField name=\{connection\.auth_name\} context="bucket"/);
  assert.match(bucket, /Connected user · <code>end_user_ref/);
  assert.match(queries, /items \{ id bucket_id service_id end_user_ref auth_type auth_name token_type/);
});

// Static storage names may differ from the scheme name, especially for paired Basic or mTLS credentials.
test("static secrets display their actual singular or grouped storage keys", () => {
  const single = renderToStaticMarkup(createElement(StoredSecretKeys, { secret: { key_name: "bearerToken" } }));
  assert.match(single, /Stored under/);
  assert.match(single, />bearerToken<\/code>/);
  const grouped = renderToStaticMarkup(createElement(StoredSecretKeys, { secret: {
    key_name: "unused-group-label",
    key_names: ["basicAuth_username", "basicAuth_password"],
  } }));
  assert.match(grouped, />basicAuth_username<\/code>/);
  assert.match(grouped, />basicAuth_password<\/code>/);
  assert.doesNotMatch(grouped, /unused-group-label|auth\.name|<button/);
});

// Empty group metadata falls back only to the stored singular key, never to a guessed auth type.
test("static storage metadata remains honest when keys are absent or contain markup", () => {
  const empty = renderToStaticMarkup(createElement(StoredSecretKeys, { secret: { key_name: "", key_names: [] } }));
  assert.match(empty, /Not provided/);
  const escaped = renderToStaticMarkup(createElement(StoredSecretKeys, { secret: { key_name: "<exact&key>", key_names: [] } }));
  assert.match(escaped, /&lt;exact&amp;key&gt;/);
  assert.doesNotMatch(escaped, /<exact/);
});

// The list must pass through its existing metadata projection while preserving the masked credential value.
test("static secret rows expose stored keys without fetching or revealing credentials", () => {
  const list = readFileSync(new URL("../components/buckets/BucketEntryList.tsx", import.meta.url), "utf8");
  const queries = readFileSync(new URL("./buckets.ts", import.meta.url), "utf8");
  assert.match(list, /storedSecret: \{ key_name: item\.key_name, key_names: item\.key_names \}/);
  assert.match(list, /<StoredSecretKeys secret=\{entry\.storedSecret\}/);
  assert.match(list, /value: "\*{8}"/);
  assert.match(queries, /items \{ id bucket_id service_id key_name key_names credential_type/);
});
