import assert from "node:assert/strict";
import test from "node:test";
import { templateFromApp, parseTemplate, checkTemplateDependency, templatePublishPath } from "./unified-template.ts";

/** Includes sensitive deployment settings to exercise the publication allowlist. */
function savedApp() {
  return {
    app_id: "app-one", owner_team: "private-team",
    config: { kind: "unified_app", name: "Customer lookup", version: "1.2.3", description: "Find a customer", source: "export default buildUnifiedApp({});", bucket: "private-bucket", token: "private-token", runtime_buckets: ["private-id"],
      services: { "@crm/customer": { version: "v1", operations: ["getCustomer"], auth: { secret: "private-secret" }, bucket: "private-override" } } },
    service_pins: [{ key: "@crm/customer", service_id: "service-one", service_version_id: "version-one" }],
  };
}

// Extraction must preserve source and immutable pins without copying deployment-only material.
test("app extraction preserves exact source and pins while excluding private configuration", () => {
  const saved = savedApp(), before = structuredClone(saved);
  const result = templateFromApp(saved);
  assert.equal(result.slug, "customer-lookup");
  assert.equal(result.version, "1.2.3");
  assert.equal(result.source, saved.config.source);
  assert.deepEqual(result.services, { "@crm/customer": { service_id: "service-one", service_version_id: "version-one", version: "v1", operations: ["getCustomer"] } });
  assert.doesNotMatch(JSON.stringify(result), /private-/);
  assert.deepEqual(saved, before);
  assert.deepEqual(parseTemplate(JSON.stringify(result)), result);
});

// Applied display labels must not replace the names compiled into the original source.
test("extraction restores immutable authored keys without rewriting source", () => {
  const saved = savedApp();
  saved.config.services = { "Customer CRM": saved.config.services["@crm/customer"] };
  saved.service_pins[0].key = "Customer CRM";
  saved.service_pins[0].authored_key = "@crm/customer";
  const result = templateFromApp(saved);
  assert.deepEqual(Object.keys(result.services), ["@crm/customer"]);
  assert.equal(result.source, saved.config.source);
  assert.equal(result.services["@crm/customer"].service_version_id, "version-one");
});

// Unsupported dependencies cannot disappear from a template that appears to be a working app.
test("extraction rejects missing source, missing pins, webhook and child-app dependencies", () => {
  const saved = savedApp();
  assert.throws(() => templateFromApp({ ...saved, config: { ...saved.config, source: undefined } }), /no saved TypeScript/);
  assert.throws(() => templateFromApp({ ...saved, service_pins: [] }), /identity/);
  assert.throws(() => templateFromApp({ ...saved, config: { ...saved.config, unified_apps: { child: { name: "child", version: "1" } } } }), /do not yet support/);
  assert.throws(() => templateFromApp({ ...saved, config: { ...saved.config, webhook_attachment: "events" } }), /do not yet support/);
  saved.config.services["@crm/customer"].webhooks = ["created"];
  assert.throws(() => templateFromApp(saved), /do not yet support/);
});

// Publication validation must retain the same credential-free envelope after metadata changes.
test("publication validation rejects private fields, malformed metadata, and oversized scopes", () => {
  const draft = templateFromApp(savedApp());
  assert.throws(() => parseTemplate(JSON.stringify({ ...draft, bucket: "private" })), /without credentials/);
  const nested = structuredClone(draft);
  nested.services["@crm/customer"].auth = { secret: "private" };
  assert.throws(() => parseTemplate(JSON.stringify(nested)), /without credential settings/);
  assert.throws(() => parseTemplate(JSON.stringify({ ...draft, name: " " })), /Enter a name/);
  assert.throws(() => parseTemplate(JSON.stringify({ ...draft, requirements: [42] })), /requirements/);
  assert.throws(() => parseTemplate(JSON.stringify({ ...draft, source: "buildUnifiedApp" + "x".repeat(262144) })), /256 KiB/);
});

// Public admission requires both service and exact version visibility; ordinary private publication does not.
test("dependency checks distinguish private visibility from invalid pinned contracts", () => {
  const pin = templateFromApp(savedApp()).services["@crm/customer"];
  const data = { service: { id: "service-one", canonical_ref: "@crm/customer", is_public: false }, serviceVersions: [{ id: "version-one", name: "v1", is_public: false, status: "active" }], serviceOperations: [{ name: "getCustomer" }] };
  assert.deepEqual(checkTemplateDependency("@crm/customer", pin, data).blockers, []);
  assert.equal(checkTemplateDependency("@crm/customer", pin, data).publicBlockers.length, 2);
  assert.match(checkTemplateDependency("alias", pin, data).blockers[0], /canonical service name/);
  data.serviceVersions[0].status = "deprecated";
  data.serviceOperations = [];
  assert.equal(checkTemplateDependency("@crm/customer", pin, data).blockers.length, 2);
  data.serviceVersions[0].id = "replacement-version";
  assert.match(checkTemplateDependency("@crm/customer", pin, data).blockers[0], /pinned service version is unavailable/);
});

// Creation can resume only the fixed publisher route and carries the exact immutable app ID.
test("publish return links encode app identity and non-Latin names get an editable slug", () => {
  assert.equal(templatePublishPath(), "/integrations/unified-apps/templates?publish=1");
  assert.equal(templatePublishPath("id&public=true"), "/integrations/unified-apps/templates?publish=1&app=id%26public%3Dtrue");
  const saved = savedApp();
  saved.config.name = "顧客検索";
  assert.equal(templateFromApp(saved).slug, "unified-app");
});
