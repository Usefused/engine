import {
  api,
  type ActivatedService,
  type AuthConnection,
  type Bucket,
  type BucketConnectSummary,
  type BucketSDKSummary,
  type BucketServiceSummary,
  type BucketSummary,
  type BucketValue,
  type GraphQLPage,
  type SecretMeta,
} from "~/lib/api";

export type BucketPageState = {
  bucketSummaries: BucketSummary[];
  total?: number;
};

export type BucketContentState = {
  bucketValues?: BucketValue[];
  bucketValueTotal?: number;
  secretMetas?: SecretMeta[];
  secretMetaTotal?: number;
  authConnections?: AuthConnection[];
  authConnectionTotal?: number;
  bucketSDKs?: BucketSDKSummary[];
  bucketSDKTotal?: number;
  bucketServices?: BucketServiceSummary[];
  bucketServiceTotal?: number;
  connectionServices?: BucketServiceSummary[];
  connectSummary?: BucketConnectSummary | null;
};

export type BucketContentPages = {
  secrets: BucketPageRequest;
  values: BucketPageRequest;
  connections: BucketPageRequest & { serviceId?: string };
  sdks: BucketPageRequest;
  services: BucketPageRequest & { search?: string };
  connectionServices: BucketPageRequest & { search?: string };
};

export type BucketContentAccess = {
  values: boolean;
  secrets: boolean;
  connections: boolean;
  apps: boolean;
  services: boolean;
};

export type BucketPageRequest = {
  limit: number;
  offset: number;
};

export type SDKBucketState = {
  buckets: Bucket[];
  sdkBuckets: Bucket[];
};

export type BucketEntryKind = "secret" | "value";
export type BucketDetailTab = "secrets" | "env" | "connected-users";

export type SecretFormPayload = {
  serviceId: string;
  keyName: string;
  credentialType: string;
  value: string;
  expiresAt?: string;
};

export type ValueFormPayload = {
  serviceId: string;
  keyName: string;
  location: string;
  value: string;
};

/** Loads one authorized page of credential-set summaries. */
export function readBuckets(limit = 20, offset = 0): Promise<BucketPageState> {
  return readBucketSummaries(limit, offset);
}

/** Loads the authorized page of aggregate credential-set summaries. */
function readBucketSummaries(
  limit: number,
  offset: number
): Promise<BucketPageState> {
  return api
    .mcpGraphql<{
      bucketSummaryPage: { items: BucketSummary[]; total: number };
    }>(
      `query($limit: Int!, $offset: Int!) {
        bucketSummaryPage(limit: $limit, offset: $offset) {
          total
          items { id name is_default secret_count value_count connected_user_count created_at updated_at }
        }
      }`,
      { limit, offset }
    )
    .then(({ bucketSummaryPage }) => ({
      bucketSummaries: bucketSummaryPage.items,
      total: bucketSummaryPage.total,
    }));
}

/** Loads only credential-set sections the current actor is authorized to read. */
export function readBucketContents(
  bucketId: string,
  pages: BucketContentPages,
  access: BucketContentAccess
): Promise<BucketContentState> {
  // These are a fixed set of independent page reads, not row-driven fan-out;
  // separating them prevents one protected section from invalidating all data.
  const reads: Promise<BucketContentState>[] = [];
  if (access.values) reads.push(readBucketValues(bucketId, pages.values));
  if (access.secrets) reads.push(readBucketSecrets(bucketId, pages.secrets));
  if (access.connections) {
    reads.push(readBucketConnections(bucketId, pages.connections));
    reads.push(readBucketConnectSummary(bucketId));
  }
  // Service visibility is independent from connection visibility, so the
  // connected-user filter must never make an otherwise permitted read fail.
  if (access.connections && access.services) {
    reads.push(readBucketConnectionServices(bucketId, pages.connectionServices));
  }
  if (access.apps) reads.push(readBucketApps(bucketId, pages.sdks));
  if (access.services) reads.push(readBucketServices(bucketId, pages));
  return Promise.all(reads).then((states) => Object.assign({}, ...states));
}

/** Loads one aggregate credential-set summary. */
export function readBucketSummary(bucketId: string): Promise<BucketSummary> {
  return api
    .mcpGraphql<{ bucketSummary: BucketSummary }>(
      `query($bucketId: String!) {
        bucketSummary(bucket_id: $bucketId) { id name is_default secret_count value_count connected_user_count created_at updated_at }
      }`,
      { bucketId }
    )
    .then(({ bucketSummary }) => bucketSummary);
}

/** Loads a page of plaintext bucket values when workspace-level value access exists. */
function readBucketValues(
  bucketId: string,
  page: BucketPageRequest
): Promise<BucketContentState> {
  return api
    .mcpGraphql<{ bucketValuePage: GraphQLPage<BucketValue> }>(
      `query($bucketId: String!, $limit: Int!, $offset: Int!) {
        bucketValuePage(bucket_id: $bucketId, limit: $limit, offset: $offset) {
          total
          items { id bucket_id service_id key_name location value created_at updated_at }
        }
      }`,
      { bucketId, limit: page.limit, offset: page.offset }
    )
    .then(({ bucketValuePage }) => ({
      bucketValues: bucketValuePage.items,
      bucketValueTotal: bucketValuePage.total,
    }));
}

/** Loads a page of secret metadata without requesting encrypted values. */
function readBucketSecrets(
  bucketId: string,
  page: BucketPageRequest
): Promise<BucketContentState> {
  return api
    .mcpGraphql<{ secretMetaPage: GraphQLPage<SecretMeta> }>(
      `query($bucketId: String!, $limit: Int!, $offset: Int!) {
        secretMetaPage(bucket_id: $bucketId, limit: $limit, offset: $offset) {
          total
          items { id bucket_id service_id key_name key_names credential_type last_used_at expires_at created_at updated_at }
        }
      }`,
      { bucketId, limit: page.limit, offset: page.offset }
    )
    .then(({ secretMetaPage }) => ({
      secretMetas: secretMetaPage.items,
      secretMetaTotal: secretMetaPage.total,
    }));
}

/** Loads one page of connected users under connection-read permission. */
function readBucketConnections(
  bucketId: string,
  page: BucketContentPages["connections"]
): Promise<BucketContentState> {
  return api
    .mcpGraphql<{ authConnectionPage: GraphQLPage<AuthConnection> }>(
      `query BucketConnections($bucketId: String!, $limit: Int!, $offset: Int!, $serviceId: String) {
        authConnectionPage(bucket_id: $bucketId, service_id: $serviceId, limit: $limit, offset: $offset) {
          total
          items { id bucket_id service_id end_user_ref auth_type token_type scopes scope_source issuer subject expires_at refresh_token_expires_at last_used_at refresh_state last_failure_code last_failure_at last_failure_trace_id created_at updated_at }
        }
      }`,
      {
        bucketId,
        limit: page.limit,
        offset: page.offset,
        serviceId: page.serviceId || "",
      }
    )
    .then(({ authConnectionPage }) => ({
      authConnections: authConnectionPage.items,
      authConnectionTotal: authConnectionPage.total,
    }));
}

/** Loads connected-user service filters only when service-read access exists. */
function readBucketConnectionServices(
  bucketId: string,
  page: BucketContentPages["connectionServices"]
): Promise<BucketContentState> {
  return api
    .mcpGraphql<{ connectionServicePage: GraphQLPage<BucketServiceSummary> }>(
      `query BucketConnectionServices($bucketId: String!, $search: String, $limit: Int!, $offset: Int!) {
        connectionServicePage: bucketServicePage(bucket_id: $bucketId, search: $search, limit: $limit, offset: $offset) {
          total
          items { service_id service_name secret_count value_count connect_config_count connected_user_count }
        }
      }`,
      {
        bucketId,
        search: page.search || "",
        limit: page.limit,
        offset: page.offset,
      }
    )
    .then(({ connectionServicePage }) => ({
      connectionServices: connectionServicePage.items,
    }));
}

/** Loads the bucket-level connection aggregate separately from service data. */
function readBucketConnectSummary(bucketId: string): Promise<BucketContentState> {
  return api
    .mcpGraphql<{ connectSummary: BucketConnectSummary | null }>(
      `query BucketConnectSummary($bucketId: String!) {
        connectSummary: bucketConnectSummary(bucket_id: $bucketId) {
          bucket_id connect_config_count connected_user_count
        }
      }`,
      { bucketId }
    )
    .then(({ connectSummary }) => ({ connectSummary }));
}

/** Loads SDKs associated with one credential set. */
function readBucketApps(bucketId: string, page: BucketPageRequest): Promise<BucketContentState> {
  return api
    .mcpGraphql<{ bucketSDKPage: GraphQLPage<BucketSDKSummary> }>(
      `query($bucketId: String!, $limit: Int!, $offset: Int!) {
        bucketSDKPage(bucket_id: $bucketId, limit: $limit, offset: $offset) {
          total
          items { id name kind active created_at }
        }
      }`,
      { bucketId, limit: page.limit, offset: page.offset }
    )
    .then(({ bucketSDKPage }) => ({
      bucketSDKs: bucketSDKPage.items,
      bucketSDKTotal: bucketSDKPage.total,
    }));
}

/** Loads services associated with one credential set. */
function readBucketServices(
  bucketId: string,
  pages: BucketContentPages
): Promise<BucketContentState> {
  return api
    .mcpGraphql<{ bucketServicePage: GraphQLPage<BucketServiceSummary> }>(
      `query($bucketId: String!, $limit: Int!, $offset: Int!, $search: String) {
        bucketServicePage(bucket_id: $bucketId, search: $search, limit: $limit, offset: $offset) {
          total
          items { service_id service_name secret_count value_count connect_config_count connected_user_count }
        }
      }`,
      {
        bucketId,
        limit: pages.services.limit,
        offset: pages.services.offset,
        search: pages.services.search || "",
      }
    )
    .then(({ bucketServicePage }) => ({
      bucketServices: bucketServicePage.items,
      bucketServiceTotal: bucketServicePage.total,
    }));
}

/** Loads workspace services for credential composers and labels. */
export function readWorkspaceServices(): Promise<ActivatedService[]> {
  return api
    .mcpGraphql<{ workspaceServices: ActivatedService[] }>(
      `query {
        workspaceServices {
          id
          workspace_id
          service_id
          service_name
          service_slug
          version
          service_version_id
          added_by
          created_at
          enabled_versions { id service_version_id version status created_at enabled_at }
          auth_options { id label auth_type credential_type key_name key_prefix required_fields supports_connected_users }
        }
      }`
    )
    .then(({ workspaceServices }) => workspaceServices);
}

/** Loads the visible credential sets and those already attached to an SDK. */
export function readBucketsForSDK(appFamilyId: string): Promise<SDKBucketState> {
  return api.mcpGraphql<SDKBucketState>(
    `query($appFamilyId: String!) {
      buckets: bucketSummaries { id name is_default created_at updated_at }
      sdkBuckets(app_family_id: $appFamilyId) { id name is_default created_at updated_at }
    }`,
    { appFamilyId }
  );
}

export function deleteBucketAuthConnection(
  bucketId: string,
  connectionId: string
) {
  return api.mcpGraphql<{ deleteAuthConnection: boolean }>(
    `mutation($bucketId: String!, $connectionId: String!) {
      deleteAuthConnection(bucket_id: $bucketId, connection_id: $connectionId)
    }`,
    { bucketId, connectionId }
  );
}

// Prefer the default bucket after deletes because it is the stable CLI/runtime
// fallback and cannot itself be removed.
export function preferredBucketID(
  buckets: Pick<Bucket, "id" | "is_default">[]
): string {
  return (
    buckets.find((bucket) => bucket.is_default)?.id || buckets[0]?.id || ""
  );
}

export function bucketSearchParams(
  current: URLSearchParams,
  bucketId: string
): URLSearchParams {
  const next = new URLSearchParams(current);
  if (bucketId) {
    next.set("bucket", bucketId);
    return next;
  }
  next.delete("bucket");
  next.delete("tab");
  return next;
}

export function bucketTabFromSearch(params: URLSearchParams): BucketDetailTab {
  const tab = params.get("tab");
  if (tab === "env" || tab === "connected-users") return tab;
  return "secrets";
}

export function bucketTabSearchParams(
  current: URLSearchParams,
  tab: BucketDetailTab
): URLSearchParams {
  const next = new URLSearchParams(current);
  next.set("tab", tab);
  return next;
}

export function formatExpiry(value?: string): string {
  if (!value) return "Never";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleDateString();
}

export function errorMessage(err: unknown, fallback: string): string {
  return err instanceof Error ? err.message : fallback;
}
