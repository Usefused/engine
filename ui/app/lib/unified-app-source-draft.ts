import type { UnifiedDraft, UnifiedServicePin } from "./unified-app-contract.ts";
import { runtimeBucketNames } from "./unified-app-runtime-buckets.ts";
import type { AppBuildSelector } from "./app-builder-contract.ts";

/** Accepts JSON records without allowing arrays or null to masquerade as configuration objects. */
function record(value: unknown, label: string): Record<string, unknown> {
  // A malformed record must leave the last valid form intact while the source remains editable.
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label} must be an object.`);
  return value as Record<string, unknown>;
}

/** Checks authored scalar fields without silently coercing incomplete values into different deployment settings. */
function text(value: unknown, label: string): string {
  // Explicit strings preserve the user's empty draft fields for ordinary form validation.
  if (typeof value !== "string") throw new Error(`${label} must be a string.`);
  return value;
}

/** Keeps event and operation scope finite and explicit rather than accepting wildcard selections. */
function names(value: unknown, label: string): string[] {
  // Omitted capability lists mean no selected capabilities of that type.
  if (value === undefined) return [];
  // Duplicate or nonliteral names would disagree with the operation picker's exact-name selections.
  if (!Array.isArray(value) || value.some(/** Capability identifiers must remain explicit strings. */ (item) => typeof item !== "string" || !item || item === "*") || new Set(value).size !== value.length) throw new Error(`${label} must be a list of distinct exact names.`);
  return value;
}

export type ResolveSourceService = (key: string, version: string, retainedID?: string) => Promise<UnifiedServicePin>;

/** Rejects competing source authorities and alternate app kinds before a code edit becomes form state. */
function validateSourceOwnership(config: Record<string, unknown>): void {
  // Source transport belongs to Engine and must never recursively enter its own metadata.
  if (["source", "source_path", "bundle_digest", "type"].some(/** Reject competing transport authorities even when their value is empty. */ (key) => Object.hasOwn(config, key))) throw new Error("config must not declare source, source_path, bundle_digest, or type.");
  // Fixed wire discriminators cannot redirect a Unified App into another deployment kind.
  if ((config.apiVersion !== undefined && config.apiVersion !== "fused/v1") || (config.kind !== undefined && config.kind !== "unified_app")) throw new Error("config must describe a fused/v1 Unified App.");
}

/** Checks portable metadata and immutable family invariants before any read-only catalogue resolution. */
function sourceSettings(metadata: Record<string, unknown>, buckets: AppBuildSelector[], locked?: { name: string; bucket: string }) {
  const config = record(metadata, "config");
  const name = text(config.name, "config.name"), version = text(config.version, "config.version"), bucketName = text(config.bucket, "config.bucket");
  // An immutable app family cannot be renamed or rebound by bypassing the form's read-only controls.
  if (locked && (name !== locked.name || bucketName !== locked.bucket)) throw new Error("The existing app's name and default bucket cannot change. Create another app to use a different identity.");
  validateSourceOwnership(config);
  const runtimeBuckets = runtimeBucketNames(config.runtime_buckets);
  // The fallback must be permitted whenever runtime selection is enabled.
  if (runtimeBuckets.length && !runtimeBuckets.includes(bucketName)) throw new Error("runtime_buckets must include the default bucket.");
  const selectedBucket = buckets.find(/** Names are portable in source; form selectors retain authorized resource identities. */ (item) => item.display_name === bucketName);
  // Missing buckets are surfaced rather than silently replacing the requested credential scope.
  if (!locked && !selectedBucket) throw new Error(`Bucket “${bucketName}” is unavailable. Choose an available bucket in Config.`);
  return { config, name, version, bucket: locked?.bucket ?? selectedBucket!.resource_id };
}

/** Hydrates valid source metadata into the same draft fields consumed by the form and deployment planner. */
export async function unifiedAppDraftFromSource(draft: UnifiedDraft, source: string, metadata: Record<string, unknown>, buckets: AppBuildSelector[], resolve: ResolveSourceService, locked?: { name: string; bucket: string }) {
  const { config, name, version, bucket } = sourceSettings(metadata, buckets, locked);
  const services = record(config.services ?? {}, "config.services");
  const entries = await Promise.all(Object.entries(services).map(/** Retain exact immutable pins for unchanged services and resolve new versions through the catalogue. */ async ([key, value]) => {
    const service = record(value, `services.${key}`);
    const serviceVersion = text(service.version, `services.${key}.version`);
    // Unified Apps require exact operations; selecting all would broaden the authored code's authority.
    if (service.select_all === true) throw new Error("Choose exact operations instead of select_all.");
    const operations = names(service.operations, `services.${key}.operations`), webhooks = names(service.webhooks, `services.${key}.webhooks`);
    const previous = draft.services[key];
    // Reuse only the same reviewed version; renamed aliases and changed versions need a fresh exact identity.
    const pin = previous?.version === serviceVersion ? previous : await resolve(key, serviceVersion, previous?.service_id);
    return [key, { ...pin, version: serviceVersion, operations, webhooks }] as const;
  }));
  // The shared picker represents one selection per service and cannot preserve two differently scoped aliases.
  if (new Set(entries.map(/** Provider identity must remain unique in this form. */ ([, pin]) => pin.service_id)).size !== entries.length) throw new Error("Use one alias per service in this editor.");
  const pins = Object.fromEntries(entries);
  const next: UnifiedDraft = { ...draft, name, source, description: text(config.description ?? "", "config.description"), configSettings: { ...config }, services: pins,
    serviceSettings: Object.fromEntries(entries.map(/** Preserve auth and routing settings under their resolved provider identity. */ ([key, pin]) => [key, { service_id: pin.service_id, config: services[key] as Record<string, unknown> }])),
    webhookAttachment: text(config.webhook_attachment ?? "", "config.webhook_attachment") };
  return { draft: next, name, version, bucket };
}
