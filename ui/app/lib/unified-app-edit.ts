import { unifiedDraftHasEvents, unifiedServiceSettings, type UnifiedDraft } from "./unified-app-contract.ts";

export interface UnifiedAppSource {
  app_id: string;
  owner_team: string;
  config: Record<string, unknown> & {
    kind: string; name: string; version: string; bucket: string; source?: string; description?: string;
    webhook_attachment?: string;
    services: Record<string, Record<string, unknown> & { version: string; operations: string[]; webhooks?: string[] }>;
  };
  service_pins: Array<{ key: string; authored_key?: string; service_id: string; service_version_id: string }>;
}

/** Hydrates exact saved pins and an optional explicitly chosen registration without changing the saved version. */
export function unifiedEditDraft(saved: UnifiedAppSource, webhookAttachment?: string): UnifiedDraft {
  const config = saved.config;
  // Precompiled-only deployments have no recoverable TypeScript; never pretend an empty source is their implementation.
  if (config.kind !== "unified_app" || typeof config.source !== "string" || !config.source.trim()) throw new Error("This version has no saved TypeScript source to edit. Create a new app from your original source.");
  // The shared picker owns one selection per provider; collapsing multiple aliases would broaden their separate operation scopes.
  if (new Set(saved.service_pins.map((pin) => pin.service_id)).size !== saved.service_pins.length) throw new Error("This app uses multiple aliases for one service. Edit its source with fused-cli to preserve each alias's operation scope.");
  const services = Object.fromEntries(Object.entries(config.services).map(([key, service]) => {
    const pin = saved.service_pins.find((item) => item.key === key);
    // A missing immutable pin must not silently bind this draft to a different provider or version.
    if (!pin?.service_id || !pin.service_version_id || !service.version || !Array.isArray(service.operations)) throw new Error(`Saved service identity is unavailable for ${key}.`);
    // Restore the compiled authored alias when Engine normalized the saved config key to a display name.
    return [pin.authored_key ?? key, { ...pin, version: service.version, operations: [...service.operations], webhooks: [...(service.webhooks ?? [])] }];
  }));
  // Preserve the complete per-service config, including an explicit auth preference, for form and YAML editing.
  const serviceSettings = Object.fromEntries(Object.entries(config.services).map(([key, service]) => {
    const pin = saved.service_pins.find((item) => item.key === key)!;
    // Key settings by the restored authored alias so credential routing stays attached across edits.
    return [pin.authored_key ?? key, { service_id: pin.service_id, config: { ...service } }];
  }));
  // A chosen registration changes only the draft; ordinary edits retain their saved ingress binding.
  return { serviceSettings, configSettings: { ...config }, name: config.name, description: config.description ?? "", source: config.source, services, webhookAttachment: webhookAttachment ?? config.webhook_attachment ?? "" };
}

/** Suggests a new patch version for ordinary semantic versions while leaving custom labels to the author. */
export function nextUnifiedVersion(version: string): string {
  const match = /^(\d+)\.(\d+)\.(\d+)(?:[-+].*)?$/.exec(version);
  // Non-semantic labels have no safe implicit successor.
  if (!match) return "";
  return `${match[1]}.${match[2]}.${BigInt(match[3]) + 1n}`;
}

/** Keeps authored service aliases stable when the shared picker emits canonical provider-qualified names. */
export function unifiedEditSelections(
  saved: UnifiedAppSource,
  selected: UnifiedDraft["services"],
  current?: UnifiedDraft["services"],
): UnifiedDraft["services"] {
  return Object.fromEntries(Object.entries(selected).flatMap(([key, pin]) => {
    // Preserve an alias the author explicitly renamed in Code before falling back to the saved baseline's alias.
    const active = current ? Object.entries(current).filter(([, entry]) => entry.service_id === pin.service_id).map(([alias]) => alias) : [];
    if (active.length) return active.map((alias) => [alias, { ...pin }]);
    const originals = saved.service_pins.filter((item) => item.service_id === pin.service_id);
    // Newly selected providers use the picker's canonical reference; retained providers keep their authored names.
    if (!originals.length) return [[key, pin]];
    // Prefer the bundle's authored alias over Engine's normalized display-name key.
    return originals.map((original) => [original.authored_key ?? original.key, { ...pin }]);
  }));
}

/** Preserves family identity and unedited settings while compiling changed source into a new immutable version. */
export function unifiedEditConfig(saved: UnifiedAppSource, draft: UnifiedDraft, version: string): Record<string, unknown> {
  // An existing version can never be overwritten, including through a direct call around the form guard.
  if (!version.trim() || version.trim() === saved.config.version) throw new Error("Choose a new version before compiling your changes.");
  const services = Object.fromEntries(Object.entries(draft.services).map(([key, pin]) => {
    // Match either the restored authored alias or the saved config key for this provider identity.
    const original = saved.service_pins.find((item) => (item.authored_key ?? item.key) === key && item.service_id === pin.service_id);
    // Retain auth, routing, and bucket overrides only when the selected provider identity is unchanged.
    const settings = draft.serviceSettings?.[key] ? unifiedServiceSettings(draft, key) : original ? saved.config.services[original.key] : {};
    return [key, { ...settings, version: pin.version, operations: pin.operations, webhooks: pin.webhooks ?? [] }];
  }));
  const config: Record<string, unknown> = { ...(draft.configSettings ?? saved.config), version: version.trim(), source: draft.source, description: draft.description, services };
  const hasEvents = unifiedDraftHasEvents(draft);
  // A successor cannot silently keep an attachment after its final event selection is removed.
  if (hasEvents) {
    // A changed event scope still needs an explicit registered bundle before planning.
    if (!draft.webhookAttachment?.trim()) throw new Error("Choose a webhook registration for the selected events.");
    config.webhook_attachment = draft.webhookAttachment.trim();
  } else delete config.webhook_attachment;
  // A prior bundle or local source path cannot compete with the source being compiled in this browser.
  delete config.bundle_digest;
  delete config.source_path;
  return config;
}
