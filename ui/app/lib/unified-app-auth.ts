import type { AuthConfig } from "./api";

/** Translates imported provider scheme spellings into Engine's existing config vocabulary. */
export function unifiedAuthType(auth: AuthConfig): string {
  const type = auth.type.toLowerCase().replaceAll("-", "_");
  const aliases: Record<string, string> = { apikey: "api_key", oauth2: "oauth", oauth2_authorization_code: "oauth", openidconnect: "oidc", open_id_connect: "oidc", mutualtls: "mtls", mutual_tls: "mtls" };
  // HTTP schemes carry the useful auth family in scheme rather than the generic OpenAPI type.
  return type === "http" ? auth.scheme?.toLowerCase() ?? "" : aliases[type] ?? type;
}

/** Uses both type and name so two bearer/OAuth schemes cannot collapse into the same choice. */
export function unifiedAuthOptions(auths: AuthConfig[]) {
  const labels: Record<string,string> = { bearer: "Bearer token", basic: "Basic authentication", api_key: "API key", oauth: "OAuth 2.0", oidc: "OpenID Connect", mtls: "Mutual TLS", oauth1: "OAuth 1.0" };
  return auths.flatMap((auth) => {
    const type = unifiedAuthType(auth), name = auth.name?.trim();
    // Unnamed schemes cannot produce an exact credential selector; the Engine remains the validation authority.
    if (!name || !type) return [];
    return [{ type, name, key: JSON.stringify([type, name]), label: `${labels[type] ?? type} · ${name}${auth.deprecated ? " (deprecated)" : ""}` }];
  });
}
