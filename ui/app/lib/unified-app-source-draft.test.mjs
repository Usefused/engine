import assert from "node:assert/strict";
import test from "node:test";
import { unifiedAppDraftFromSource } from "./unified-app-source-draft.ts";
const pin = { service_id: "crm", service_version_id: "crm-v1", version: "v1", operations: ["getCustomer"] };
const draft = { name: "customer", description: "Before", source: "before", services: { crm: pin } };
const metadata = { name: "customer", version: "1.0.2", bucket: "default", description: "After", runtime_buckets: ["default", "deployment"], webhook_attachment: "customer-events", services: { crm: { version: "v1", operations: [], webhooks: ["customer.created"], auth: { type: "bearer", name: "token" } } } };
const buckets = [{ display_name: "default", resource_id: "bucket-default" }, { display_name: "deployment", resource_id: "bucket-deployment" }];
/** Unchanged provider identities must never require a network call merely because metadata was edited. */
async function unexpectedResolution() { throw new Error("Unexpected provider lookup"); }

/** Source-authored scope and credentials must reach the same draft fields used by every form control. */
test("source edits hydrate configuration and preserve exact service identity", async () => {
  const result = await unifiedAppDraftFromSource(draft, "authored source", metadata, buckets, unexpectedResolution);
  assert.equal(result.bucket, "bucket-default");
  assert.equal(result.version, "1.0.2");
  assert.equal(result.draft.description, "After");
  assert.equal(result.draft.webhookAttachment, "customer-events");
  assert.deepEqual(result.draft.configSettings.runtime_buckets, ["default", "deployment"]);
  assert.deepEqual(result.draft.services.crm, { ...pin, operations: [], webhooks: ["customer.created"] });
  assert.deepEqual(result.draft.serviceSettings.crm.config.auth, { type: "bearer", name: "token" });
  assert.equal(result.draft.source, "authored source");
  assert.equal(draft.source, "before");
});

/** New versions must resolve exact immutable IDs while removal must clear prior settings and capabilities. */
test("source edits resolve changed service versions and remove old selections", async () => {
  const calls = [];
  const changed = { ...metadata, bucket: "deployment", runtime_buckets: [], services: { billing: { version: "v2", operations: ["getInvoice"] } } };
  const result = await unifiedAppDraftFromSource(draft, "source", changed, buckets, /** Record the precise authored service and version without choosing a fallback. */ async (key, version) => { calls.push([key, version]); return { ...pin, service_id: "billing", service_version_id: "billing-v2", version }; });
  assert.deepEqual(calls, [["billing", "v2"]]);
  assert.equal(result.bucket, "bucket-deployment");
  assert.deepEqual(Object.keys(result.draft.services), ["billing"]);
  assert.deepEqual(Object.keys(result.draft.serviceSettings), ["billing"]);
});

/** Invalid source must not relax immutable identity or silently choose broader buckets and service scope. */
test("invalid metadata leaves the valid draft untouched", async () => {
  for (const changed of [{ name: "another" }, { bucket: "deployment" }, { source: "recursive" }, { kind: "sdk" }, { runtime_buckets: ["deployment"] }, { services: {crm: {version:"v1", operations:["*"]}} }]) {
    await assert.rejects(unifiedAppDraftFromSource(draft, "invalid source", { ...metadata, ...changed }, buckets, unexpectedResolution, {name:"customer",bucket:"default"}));
  }
  assert.equal(draft.name, "customer");
  assert.equal(draft.source, "before");
});
