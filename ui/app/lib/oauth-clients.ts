import { api } from "./api";
import { OAUTH_CLIENT_OPERATIONS, OAUTH_REGISTRATION_KEY_OPERATIONS } from "./oauth-clients-contract";

export type OAuthClientType = "CONFIDENTIAL" | "PUBLIC";

/** One entry of the Engine's built-in OAuth scope catalog: a permission string plus its human label. */
export interface OAuthScope {
  value: string;
  label: string;
}

// Fetches the scope catalog from the server (oauthScopeCatalog query) instead
// of hardcoding a copy client-side, so the scope picker always reflects the
// server's current permission set.
export async function listOAuthScopeCatalog(): Promise<OAuthScope[]> {
  const data = await api.mcpGraphql<{ oauthScopeCatalog: OAuthScope[] }>(OAUTH_CLIENT_OPERATIONS.scopeCatalog);
  return data.oauthScopeCatalog;
}

export interface OAuthClient {
  id: string;
  name: string;
  client_id: string;
  client_type: OAuthClientType;
  redirect_uris: string[];
  allowed_scopes: string[];
  has_secret: boolean;
  created_at: string;
  revoked_at: string | null;
}

export interface OAuthClientCreatedPayload {
  client: OAuthClient;
  client_secret: string | null;
}

export async function listOAuthClients(): Promise<OAuthClient[]> {
  const data = await api.mcpGraphql<{ oauthClients: OAuthClient[] }>(OAUTH_CLIENT_OPERATIONS.list);
  return data.oauthClients;
}

export async function createOAuthClient(input: {
  name: string;
  client_type: OAuthClientType;
  redirect_uris: string[];
  allowed_scopes: string[];
}): Promise<OAuthClientCreatedPayload> {
  const data = await api.mcpGraphql<{ createOAuthClient: OAuthClientCreatedPayload }>(
    OAUTH_CLIENT_OPERATIONS.create,
    { input }
  );
  return data.createOAuthClient;
}

export async function revokeOAuthClient(id: string): Promise<boolean> {
  const data = await api.mcpGraphql<{ revokeOAuthClient: boolean }>(OAUTH_CLIENT_OPERATIONS.revoke, { id });
  return data.revokeOAuthClient;
}

export interface OAuthRegistrationKeyStatus {
  exists: boolean;
}

export async function getOAuthRegistrationKeyStatus(): Promise<OAuthRegistrationKeyStatus> {
  const data = await api.mcpGraphql<{ oauthRegistrationKey: OAuthRegistrationKeyStatus }>(
    OAUTH_REGISTRATION_KEY_OPERATIONS.status
  );
  return data.oauthRegistrationKey;
}

export async function createOAuthRegistrationKey(): Promise<string> {
  const data = await api.mcpGraphql<{ createOAuthRegistrationKey: { key: string } }>(
    OAUTH_REGISTRATION_KEY_OPERATIONS.create
  );
  return data.createOAuthRegistrationKey.key;
}

export async function revokeOAuthRegistrationKey(): Promise<boolean> {
  const data = await api.mcpGraphql<{ revokeOAuthRegistrationKey: boolean }>(
    OAUTH_REGISTRATION_KEY_OPERATIONS.revoke
  );
  return data.revokeOAuthRegistrationKey;
}
