export interface UnifiedServicePin {
  service_id: string;
  service_version_id: string;
  version: string;
  operations: string[];
  webhooks?: string[];
}
export interface UnifiedDraft {
  name: string;
  description: string;
  source: string;
  services: Record<string, UnifiedServicePin>;
  webhookAttachment?: string;
}
export interface UnifiedTemplate extends UnifiedDraft {
  schema_version: 1;
  slug: string;
  version: string;
  category: string;
  requirements: string[];
}
export interface UnifiedRelease {
  id: string;
  publisher: string;
  hash: string;
  public: boolean;
  is_owner: boolean;
  template: UnifiedTemplate;
}

/** Distinguishes revisable intent from transient drafting failures that can reuse the same pins. */
export class UnifiedSourceClarificationError extends Error {
  /** Retains the model's bounded clarification without treating it as a transport retry. */
  constructor(message: string) { super(message); this.name = "UnifiedSourceClarificationError"; }
}

/** Rejects ambiguous or malformed model output before it enters a reviewable app config. */
export function decodeUnifiedSource(raw: string): string {
  // Mirror CLI byte bounds so browser and terminal admit the same source envelope.
  if (new TextEncoder().encode(raw).length > 128 * 1024) throw new Error("Unified App draft exceeds the size limit.");
  const draft = JSON.parse(raw);
  // Clarification must preserve the goal for revision, never produce an empty shell.
  if (typeof draft.clarification === "string" && draft.clarification.trim()) throw new UnifiedSourceClarificationError(draft.clarification);
  // Engine remains the compiler authority; this guard rejects incomplete model responses early.
  if (typeof draft.source !== "string" || new TextEncoder().encode(draft.source).length > 64 * 1024 || !draft.source.includes("buildUnifiedApp")) throw new Error("The draft must contain bounded buildUnifiedApp TypeScript source.");
  return draft.source;
}

/** Includes reviewed source and event scope in the desired state consumed by Unified App planning. */
export function unifiedConfig(draft: UnifiedDraft, name: string, version: string, bucket: string): Record<string, unknown> {
  // User-reviewed identity is required before compilation or service activation.
  if (!name.trim() || !version.trim() || !bucket.trim()) throw new Error("Name, version, and a credential bucket are required.");
  const services = Object.fromEntries(Object.entries(draft.services).map(([key, pin]) => [key, { version: pin.version, operations: pin.operations, ...(pin.webhooks?.length ? { webhooks: pin.webhooks } : {}) }]));
  const hasEvents = Object.values(draft.services).some((pin) => pin.webhooks?.length);
  // A selected event without an applied registration would never reach the hosted worker.
  if (hasEvents && !draft.webhookAttachment?.trim()) throw new Error("Choose a webhook registration for the selected events.");
  return { apiVersion: "fused/v1", kind: "unified_app", name: name.trim(), description: draft.description, version: version.trim(), bucket: bucket.trim(), ...(hasEvents ? { webhook_attachment: draft.webhookAttachment!.trim() } : {}), services, source: draft.source };
}

/** Decodes published source templates for review before installation. */
export function decodeUnifiedRelease(release: Omit<UnifiedRelease, "template"> & { template: string }): UnifiedRelease {
  const template = JSON.parse(release.template) as UnifiedTemplate;
  // Catalogue content must have exact dependencies and the supported source schema before installation.
  if (template.schema_version !== 1 || !template.services || !Object.keys(template.services).length || !template.name || !template.slug || !template.version) throw new Error("Unsupported Unified App template.");
  decodeUnifiedSource(JSON.stringify({ source: template.source }));
  // Requirements are optional authoring metadata, but presentation always consumes a list.
  template.requirements = template.requirements ?? [];
  // Reject malformed dependency pins before activation receives a partial template.
  if (!Array.isArray(template.requirements) || Object.values(template.services).some(invalidServicePin)) throw new Error("Invalid Unified App template dependencies.");
  return { ...release, template };
}

/** Each portable service dependency requires exact identity and an explicit operation allowlist. */
function invalidServicePin(pin: UnifiedServicePin): boolean { return !pin?.service_id || !pin.service_version_id || !pin.version || !Array.isArray(pin.operations) || !pin.operations.length; }
