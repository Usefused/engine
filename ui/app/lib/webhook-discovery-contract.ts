export interface WebhookRegistration {
  label: string;
  slug: string;
  callback_url: string;
  delivery_mode: "direct" | "managed" | "unknown";
  signing_secret?: { bucket_id: string; key_name: string } | null;
  signature: "set" | "none";
  created_at: string;
}
export interface WebhookListing extends WebhookRegistration { service_id: string; service_name: string; service_ref?: string }
export interface WebhookDraft { name: string; service: string; secret: string; baseURL: string }

/** Uses the service's canonical identity without inventing a provider when Registry metadata is absent. */
export function webhookServiceTag(service: { service_name: string; service_slug?: string; registry_slug?: string; canonical_ref?: string | null; provider?: { handle: string } | null }): string {
  const reference = [service.canonical_ref, service.service_slug].find((value) => /^@[^/]+\/[^/]+$/.test(value || ""));
  // Cached canonical references remain useful even while the Registry is unavailable.
  if (reference) return reference;
  const slug = service.registry_slug || service.service_slug;
  // Only combine known provider metadata with a bare service slug.
  if (service.provider?.handle && slug && !slug.includes("/")) return `@${service.provider.handle}/${slug}`;
  // Older cached services may have only their display name; preserve that identity honestly.
  return service.service_name;
}

/** Builds the same named desired configuration accepted by the CLI webhook plan endpoint. */
export function webhookConfiguration(draft: WebhookDraft): Record<string, unknown> {
  const name = draft.name.trim(), service = draft.service.trim(), base = draft.baseURL.trim(), secret = draft.secret.trim();
  // Blank identities must not accidentally target an existing default registration.
  if (!name || !service) throw new Error("Enter a webhook name and choose a service.");
  let parsed: URL;
  try { parsed = new URL(base); } catch { throw new Error("Enter the public URL of your Engine."); }
  // Provider delivery requires an explicit safe base; do not infer it from the UI's development proxy.
  if ((parsed.protocol !== "https:" && !(parsed.protocol === "http:" && ["localhost", "127.0.0.1", "[::1]"].includes(parsed.hostname))) || parsed.username || parsed.password || parsed.search || parsed.hash) throw new Error("Use an HTTPS Engine URL without credentials, query parameters, or a fragment.");
  // Only references may enter desired configuration; never accept the provider's raw signing secret here.
  if (secret && !/^\$\{bucket\.[^{}]+\.secret\.[^{}]+\}$/.test(secret) && !/^\$\{bucket\.secret\.[^{}]+\}$/.test(secret)) throw new Error("Choose a secret reference from Credentials, such as ${bucket.default.secret.signing_key}.");
  return { apiVersion: "fused/v1", kind: "webhook", name, callback_base_url: base.replace(/\/$/, ""), services: { [service]: secret ? { secret } : {} } };
}

/** Only direct, fully qualified destinations are offered as provider-ready URLs. */
export function copyableWebhookURL(registration: WebhookRegistration): string | null {
  // Managed subscriptions pull from a broker and must never advertise a local ingress URL.
  if (registration.delivery_mode !== "direct" || !registration.callback_url) return null;
  try {
    const url = new URL(registration.callback_url);
    // Persisted URLs still cross a display boundary; reject credentials and non-HTTP schemes.
    if (!["https:", "http:"].includes(url.protocol) || url.username || url.password) return null;
    return registration.callback_url;
  } catch { return null; }
}

/** Search includes the visible service tag and registration identities without inspecting secrets. */
export function matchesWebhook(registration: WebhookListing, search: string): boolean {
  const value = search.trim().toLowerCase();
  // Older callers can omit the canonical reference without breaking display-name search.
  return [registration.label, registration.service_name, registration.service_ref || "", registration.callback_url, registration.slug].some((field) => field.toLowerCase().includes(value));
}
