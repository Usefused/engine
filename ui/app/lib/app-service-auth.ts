export interface AppAuthConfig { type: string; name: string; ref?: string }
export type AppAuthEdits = Record<string, { service_id: string; auth?: AppAuthConfig }>;

/** Applies explicit auth edits after consumer composition while preserving all unrelated service settings. */
export function applyAppAuthEdits(config: Record<string, unknown>, edits: AppAuthEdits, identities: Record<string, string>): Record<string, unknown> {
  const services = Object.fromEntries(Object.entries((config.services ?? {}) as Record<string, Record<string, unknown>>).map(([key, service]) => {
    const edit = edits[key];
    // Removed/replaced providers cannot inherit auth choices made for an earlier identity.
    if (!edit || edit.service_id !== identities[key]) return [key, service];
    const next = { ...service };
    // An explicit empty selection restores the provider contract's ordering.
    if (edit.auth) next.auth = edit.auth;
    else delete next.auth;
    return [key, next];
  }));
  return { ...config, services };
}
