import type { AppCredentialReadiness, AppMissingCredential } from "./app-builder-contract";

/** Uses Engine's resolved auth requirements rather than guessing readiness from a bucket's total secret count. */
export function missingAppCredentials(readiness?: AppCredentialReadiness | null): AppMissingCredential[] {
  return readiness?.missing_credentials ?? [];
}

/** Opens the exact resolved bucket without losing the app draft in the current tab. */
export function appCredentialBucketURL(credential: AppMissingCredential): string {
  return `/integrations/buckets?${new URLSearchParams({ bucket: credential.bucket_id, tab: "secrets" })}`;
}

/** Distinguishes identical scheme names in different services and per-service bucket overrides. */
export function appCredentialRequirementKey(credential: AppMissingCredential): string {
  return JSON.stringify([credential.bucket_id, credential.service_id, credential.auth_type, credential.auth_name]);
}
