import { hasResourcePermission, type CurrentActorAccess } from "./current-actor-permissions.ts";
import type { ActivatedService } from "./api";
import type { ServiceAuthOption } from "./service-auth";

export type BucketConnectInput = {
  bucketId: string;
  serviceId: string;
  endUserRef: string;
  auth: ServiceAuthOption;
  authRef: string;
  scopes: string;
};

/** Offers only declared schemes that Engine can use for connected-user consent. */
export function bucketConnectOptions(service?: ActivatedService): ServiceAuthOption[] {
  // Missing service metadata must not invent a provider authentication contract.
  return (service?.auth_options ?? []).filter((option) => option.supports_connected_users);
}

/** Returns consent to the exact bucket, without carrying unrelated query state or credentials. */
export function bucketConnectReturnURL(origin: string, bucketId: string): string {
  const url = new URL("/integrations/buckets", origin);
  url.searchParams.set("bucket", bucketId);
  url.searchParams.set("tab", "connected-users");
  return url.href;
}

/** Matches CLI selectors and omissions; Engine remains authoritative for scopes, references, and permission checks. */
export function bucketConnectVariables(input: BucketConnectInput, origin: string) {
  // An empty user reference cannot identify the bucket-owned grant to create or reconnect.
  if (!input.endUserRef.trim()) throw new Error("Enter a user reference for this account.");
  // Only an exact selected scheme may start consent; never substitute another service or bucket.
  if (!input.bucketId || !input.serviceId || !input.auth.key_prefix) throw new Error("Choose a service and authentication scheme.");
  const scopes = input.scopes.trim().split(/\s+/).filter(Boolean);
  return {
    bucketId: input.bucketId,
    serviceId: input.serviceId,
    endUserRef: input.endUserRef.trim(),
    authType: input.auth.credential_type,
    authName: input.auth.key_prefix,
    // Omission uses the same Engine defaults as CLI connect, including the declared scope catalogue.
    authRef: input.authRef.trim() || undefined,
    scopes: scopes.length ? scopes : undefined,
    returnUrl: bucketConnectReturnURL(origin, input.bucketId),
  };
}

/** Rejects non-web destinations before navigating to Engine's provider or resource-input handoff. */
export function bucketAuthorizeURL(value: string): string {
  const url = new URL(value);
  // OAuth consent and Engine's hosted input page both require ordinary web navigation.
  if (url.protocol !== "https:" && url.protocol !== "http:") throw new Error("Engine returned an invalid connection URL.");
  return url.href;
}

/** Auto-selects only a sole declared scheme; multiple registrations require an explicit choice. */
export function selectedConnectOption(options: ServiceAuthOption[], id: string): ServiceAuthOption | undefined {
  // A removed explicit scheme must not silently resolve to a different registration.
  if (id) return options.find((option) => option.id === id);
  // A sole declared scheme is the only safe implicit selection.
  return options.length === 1 ? options[0] : undefined;
}

/** Mirrors the three independent Engine grants without treating bucket administration as permission to use a service. */
export function canConnectBucketService(access: CurrentActorAccess | null, bucketId: string, serviceId?: string): boolean {
  // A missing workspace service cannot safely identify the resource to authorize.
  if (!serviceId) return false;
  return hasResourcePermission(access, "connection.manage", "BUCKET", bucketId)
    && hasResourcePermission(access, "bucket.use", "BUCKET", bucketId)
    && hasResourcePermission(access, "service.consume", "SERVICE", serviceId);
}
