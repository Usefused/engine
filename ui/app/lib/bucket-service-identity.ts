import type { ActivatedService, SecretMeta } from "./api";

type ServiceIdentity = {
  service_id: string;
  service_name?: string | null;
  service_slug?: string | null;
};

/** Uses authorized Registry or workspace metadata, keeping unresolved IDs out of service titles. */
export function bucketServiceIdentity(service: ServiceIdentity, services: ActivatedService[] = []) {
  const workspace = services.find((item) => item.service_id === service.service_id);
  const name = service.service_name?.trim() || workspace?.service_name?.trim();
  const slug = service.service_slug?.trim() || workspace?.service_slug?.trim();
  // Missing metadata is explicit; retaining the full ID below still distinguishes orphaned records.
  return { name: name || slug || "Service unavailable", detail: slug || (name ? "" : `Service ID: ${service.service_id}`) };
}

/** Distinguishes credential families by service and auth scheme without touching secret values or storage keys. */
export function bucketSecretIdentity(secret: SecretMeta, services: ActivatedService[] = []) {
  const credential = credentialLabel(secret.credential_type);
  // Bucket-wide variables have no provider service or auth scheme to resolve.
  if (secret.credential_type === "bucket_secret") {
    return { name: credential, detail: secret.key_name };
  }
  const service = bucketServiceIdentity(secret, services);
  const workspace = services.find((item) => item.service_id === secret.service_id);
  const option = workspace?.auth_options?.find((item) => item.key_name === secret.key_name || item.key_prefix === secret.key_name);
  // Keep the stored scheme identifier when a friendly label alone could be ambiguous.
  const scheme = option?.label && option.label !== secret.key_name
    ? `${option.label} (${secret.key_name})`
    : secret.key_name;
  return { name: `${service.name} · ${credential}`, detail: [service.detail, `Auth scheme: ${scheme}`].filter(Boolean).join(" · ") };
}

/** Names application credential pairs distinctly from single tokens and bucket-wide variables. */
function credentialLabel(value: string): string {
  const type = value.toLowerCase().replaceAll("-", "_");
  const labels: Record<string, string> = {
    api_key: "API key", apikey: "API key", basic: "Basic credentials",
    mtls: "mTLS credentials", mutualtls: "mTLS credentials", mutual_tls: "mTLS credentials",
    oauth: "OAuth credentials", oauth2: "OAuth credentials",
    oidc: "OIDC credentials", openidconnect: "OIDC credentials",
    bearer: "Bearer token", bucket_secret: "Secret",
  };
  // Unknown auth types remain visibly service-scoped instead of pretending to be a generic variable.
  return labels[type] || "Service Auth";
}
