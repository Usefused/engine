import { dump, load, JSON_SCHEMA } from "js-yaml";
import { unifiedServiceSettings, type UnifiedDraft } from "./unified-app-contract.ts";
import type { UnifiedAppSource } from "./unified-app-edit.ts";

/** Renders portable config alongside app.ts without duplicating the TypeScript inside YAML. */
export function unifiedEditorYAML(draft: UnifiedDraft, name: string, version: string, bucket: string, saved?: UnifiedAppSource | null): string {
  const config: Record<string, unknown> = {
    ...(draft.configSettings ?? saved?.config), apiVersion: "fused/v1", kind: "unified_app", name, version,
    description: draft.description, bucket, source_path: "app.ts",
    services: Object.fromEntries(Object.entries(draft.services).map(([key, pin]) => [key, {
      ...unifiedServiceSettings(draft, key), version: pin.version, operations: pin.operations, webhooks: pin.webhooks ?? [],
    }])),
  };
  // A browser-owned source file supersedes compiled artifacts or previously embedded source.
  delete config.source; delete config.bundle_digest;
  // Removing an attachment must remain an intentional edit rather than reviving the saved value.
  delete config.webhook_attachment;
  if (draft.webhookAttachment) config.webhook_attachment = draft.webhookAttachment;
  return dump(config, { schema: JSON_SCHEMA, noRefs: true, lineWidth: 100 });
}

/** Rejects scalar/array config sections so malformed YAML cannot accidentally broaden an app scope. */
function mapping(value: unknown, label: string): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label} must be a mapping.`);
  return value as Record<string, unknown>;
}

/** Requires explicit strings because unquoted numeric versions are ambiguous across YAML parsers. */
function text(value: unknown, label: string): string {
  if (typeof value !== "string") throw new Error(`${label} must be a string; quote version numbers.`);
  return value;
}

/** Preserves exact operation/event names while rejecting select-all or malformed scalar shortcuts. */
function names(value: unknown, label: string): string[] {
  if (!Array.isArray(value) || value.some((item) => typeof item !== "string" || !item.trim())) throw new Error(`${label} must be a list of names.`);
  return value;
}

/** Keeps parsed YAML a bounded JSON tree so aliases cannot crash or amplify the editor's serialization. */
function validateConfigTree(config: Record<string, unknown>): void {
  const pending: unknown[] = [config], seen = new Set<object>();
  let nodes = 0;
  while (pending.length) {
    const value = pending.pop();
    // A compact alias graph must not expand into an unbounded plan or highlighted document.
    if (++nodes > 10000) throw new Error("YAML configuration is too complex.");
    if (!value || typeof value !== "object") continue;
    // Repeated object identities represent YAML aliases, including recursive ones.
    if (seen.has(value)) throw new Error("YAML aliases are not supported; use explicit config values.");
    seen.add(value); pending.push(...Object.values(value));
  }
}

/** Keeps YAML editing tied to reviewed provider pins; new providers and versions use the service picker. */
export function readUnifiedEditorYAML(raw: string, draft: UnifiedDraft, saved?: UnifiedAppSource | null) {
  // Config editing is bounded independently of the TypeScript source kept in the other view.
  if (new TextEncoder().encode(raw).length > 128 * 1024) throw new Error("YAML configuration exceeds 128 KiB.");
  const config = mapping(load(raw, { schema: JSON_SCHEMA }), "App configuration");
  validateConfigTree(config);
  const allowed = new Set(["apiVersion", "kind", "name", "version", "description", "bucket", "source_path", "services", "webhook_attachment", "mcp", "language", "generate", ...Object.keys(saved?.config ?? {})]);
  // Unknown fields should not look supported merely because a backend JSON decoder might ignore them.
  for (const key of Object.keys(config)) if (!allowed.has(key)) throw new Error(`Unsupported Unified App field: ${key}.`);
  if (config.apiVersion !== "fused/v1" || config.kind !== "unified_app") throw new Error("Use apiVersion: fused/v1 and kind: unified_app.");
  // The browser has one linked source editor and cannot read arbitrary local source files.
  if (config.source_path !== "app.ts" || "source" in config || "bundle_digest" in config) throw new Error("Keep source_path: app.ts and edit the source in the TypeScript view.");
  const name = text(config.name, "name"), version = text(config.version, "version"), bucket = text(config.bucket, "bucket");
  // Existing family identity and bucket bindings follow the same rules as the ordinary form.
  if (saved && (name !== saved.config.name || bucket !== saved.config.bucket)) throw new Error("An existing app keeps its name and default bucket.");
  const services: UnifiedDraft["services"] = {};
  const serviceSettings: NonNullable<UnifiedDraft["serviceSettings"]> = {};
  for (const [key, value] of Object.entries(mapping(config.services, "services"))) {
    const service = mapping(value, `services.${key}`), pin = draft.services[key];
    // Service resolution must not guess an ID or upgrade a version from YAML text.
    if (!pin || service.version !== pin.version) throw new Error(`Choose ${key} and its version using the service picker first.`);
    const fields = new Set(["version", "operations", "webhooks", "webhooks_select_all", "auth", "connect", "bucket", "injections"]);
    for (const field of Object.keys(service)) if (!fields.has(field)) throw new Error(`Unsupported service field: ${key}.${field}.`);
    // Auth configuration selects a named scheme; credentials themselves must remain in the bucket.
    if (service.auth !== undefined) {
      const auth = mapping(service.auth, `${key}.auth`);
      if (Object.keys(auth).some((field) => !["type", "name", "ref"].includes(field))) throw new Error("Auth accepts type, name and ref only. Store credential values in a bucket.");
      text(auth.type, `${key}.auth.type`); text(auth.name, `${key}.auth.name`);
    }
    services[key] = { ...pin, operations: names(service.operations, `${key}.operations`), webhooks: names(service.webhooks ?? [], `${key}.webhooks`) };
    serviceSettings[key] = { service_id: pin.service_id, config: service };
  }
  return { name, version, bucket, draft: { ...draft, configSettings: config, name, description: text(config.description ?? "", "description"), webhookAttachment: text(config.webhook_attachment ?? "", "webhook_attachment"), services, serviceSettings } };
}
