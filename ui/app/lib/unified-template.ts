import { decodeUnifiedRelease, type UnifiedTemplate } from "./unified-app-contract.ts";
import type { UnifiedAppSource } from "./unified-app-edit.ts";

export interface TemplateChecks { blockers: string[]; publicBlockers: string[] }
export interface TemplateDependency {
  service: { id: string; canonical_ref?: string | null; is_public: boolean } | null;
  serviceVersions: { id: string; name: string; status?: string; is_public: boolean }[];
  serviceOperations: { name: string }[];
}

/** Builds a portable allowlisted manifest without copying deployment credentials or runtime settings. */
export function templateFromApp(saved: UnifiedAppSource): UnifiedTemplate {
  const config = saved.config;
  // Compiled bundles cannot be reconstructed into reviewable TypeScript.
  if (config.kind !== "unified_app" || !config.source?.includes("buildUnifiedApp")) throw new Error("This app version has no saved TypeScript source. Choose a version with source.");
  // These capabilities have no representation in the current template schema; omitting them would break the installed app.
  if (hasUnsupportedDependencies(config)) {
    throw new Error("Templates do not yet support webhook triggers or calls to other Unified Apps. Choose an app without those dependencies.");
  }
  const services = Object.fromEntries(Object.entries(config.services ?? {}).map(([key, service]) => {
    const pin = saved.service_pins.find((item) => item.key === key);
    // Exact identities must survive extraction; never substitute the latest provider version.
    if (!pin) throw new Error(`Saved service identity is unavailable for ${key}.`);
    return [key, { service_id: pin.service_id, service_version_id: pin.service_version_id, version: service.version, operations: [...service.operations] }];
  }));
  // Non-Latin names still get an editable valid release slug.
  const slug = config.name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "").slice(0, 100).replace(/-$/, "") || "unified-app";
  return parseTemplate(JSON.stringify({ schema_version: 1, slug, version: config.version, name: config.name, description: config.description ?? "", category: "", requirements: [], services, source: config.source }));
}

/** Validates the generated publication envelope before source review and saving. */
export function parseTemplate(raw: string): UnifiedTemplate {
  // Bound the generated manifest before parsing and mirror Registry's publication limit.
  if (new TextEncoder().encode(raw).length > 256 * 1024) throw new Error("Template exceeds the 256 KiB limit.");
  const value = JSON.parse(raw);
  // Deployment fields must be rejected, never silently carried into an export or publication.
  if (!value || typeof value !== "object" || Array.isArray(value) || Object.keys(value).some((key) => !["schema_version", "slug", "version", "name", "description", "category", "requirements", "services", "source"].includes(key))) throw new Error("Use a portable template without credentials, bucket bindings, or deployment settings.");
  const template = decodeUnifiedRelease({ id: "", publisher: "", hash: "", public: false, is_owner: true, template: raw }).template;
  validateTemplateMetadata(template);
  validateTemplatePins(template);
  return template;
}

/** Detects runtime dependencies that cannot be represented by the current portable schema. */
function hasUnsupportedDependencies(config: UnifiedAppSource["config"]): boolean {
  return Boolean(config.webhook_attachment || Object.keys(config.unified_apps ?? {}).length || Object.values(config.services ?? {}).some((service) => service.webhooks?.length || service.webhooks_select_all));
}

/** Validates user-facing metadata independently of exact dependency admission. */
function validateTemplateMetadata(template: UnifiedTemplate): void {
  // Registry coordinates must remain usable after extraction or metadata editing.
  if (!boundedText(template.name, 200, true) || !boundedText(template.version, 128, true) || !boundedText(template.slug, 100, true) || !/^[a-z0-9]+(?:-[a-z0-9]+)*$/.test(template.slug)) throw new Error("Enter a name, release version, and a slug using lowercase letters, numbers, and hyphens.");
  // Descriptions and setup requirements are bounded plain text, never nested config.
  if (!boundedText(template.description, 16000) || !boundedText(template.category, 100) || template.requirements.length > 32 || template.requirements.some((item) => typeof item !== "string")) throw new Error("Check the description, category, and requirements (up to 32 lines).");
}

/** Accepts only bounded strings; required labels must contain non-whitespace characters. */
function boundedText(value: unknown, maximum: number, required = false): value is string {
  return typeof value === "string" && value.length <= maximum && (!required || Boolean(value.trim()));
}

/** Rejects nested runtime settings and ambiguous provider scopes before any publication request. */
function validateTemplatePins(template: UnifiedTemplate): void {
  const pins = Object.values(template.services);
  // The publication schema admits only finite, unique provider dependencies.
  if (pins.length > 16 || new Set(pins.map((pin) => pin.service_id)).size !== pins.length) throw new Error("Templates support up to 16 unique services.");
  for (const [key, pin] of Object.entries(template.services)) {
    // Service config is strictly allowlisted so private routing cannot escape through nested fields.
    if (Object.keys(pin).some((field) => !["service_id", "service_version_id", "version", "operations"].includes(field)) || pin.operations.length > 16 || pin.operations.some((op) => !boundedText(op, 256, true))) throw new Error(`Use exact service pins and 1–16 operations for ${key}, without credential settings.`);
  }
}

/** Explains dependency failures before publication while leaving Registry authoritative at commit time. */
export function checkTemplateDependency(key: string, pin: UnifiedTemplate["services"][string], data: TemplateDependency): TemplateChecks {
  const blockers: string[] = [], publicBlockers: string[] = [];
  const version = data.serviceVersions.find((item) => item.id === pin.service_version_id && item.name === pin.version);
  // Missing immutable identities cannot be repaired by silently upgrading the dependency.
  if (!data.service || data.service.id !== pin.service_id || !version) return { blockers: [`${key} ${pin.version}: the pinned service version is unavailable.`], publicBlockers };
  // Renaming a source alias would change executable code, so require an explicit source correction.
  if (data.service.canonical_ref !== key) blockers.push(`${key}: templates require the canonical service name ${data.service.canonical_ref ?? "from the service catalogue"}. Update the app's service references before publishing.`);
  // Deprecated versions and removed operations are rejected for private and public publication alike.
  if (version.status === "deprecated") blockers.push(`${key} ${pin.version}: this version is deprecated.`);
  const missing = pin.operations.filter((op) => !data.serviceOperations.some((item) => item.name === op));
  if (missing.length) blockers.push(`${key} ${pin.version}: unavailable operations: ${missing.join(", ")}.`);
  // Private dependencies remain usable for private templates but cannot be disclosed publicly.
  if (!data.service.is_public) publicBlockers.push(`${key}: the service is private.`);
  if (!version.is_public) publicBlockers.push(`${key} ${pin.version}: the service version is private.`);
  return { blockers, publicBlockers };
}

/** Keeps the create-and-return flow on a fixed local route, never a caller-supplied redirect. */
export function templatePublishPath(appID?: string): string {
  // A completed deployment resumes publication with its exact immutable version selected.
  return appID ? `/integrations/unified-apps/templates?publish=1&app=${encodeURIComponent(appID)}` : "/integrations/unified-apps/templates?publish=1";
}
