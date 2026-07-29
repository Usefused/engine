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

export function readBuckets(limit = 20, offset = 0): Promise<BucketPageState> {
  return readBucketSummaries(limit, offset).catch((err) => {
    if (!isUnknownGraphQLField(err, "bucketSummaries")) throw err;
    return readBucketsWithoutSummaries();
  });
}

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
          items { id name is_default secret_count value_count created_at updated_at }
        }
      }`,
      { limit, offset }
    )
    .then(({ bucketSummaryPage }) => ({
      bucketSummaries: bucketSummaryPage.items,
      total: bucketSummaryPage.total,
    }))
    .catch((err) => {
      if (!isUnknownGraphQLField(err, "bucketSummaryPage")) throw err;
      return api
        .mcpGraphql<BucketPageState>(
          `query {
          bucketSummaries { id name is_default secret_count value_count connected_user_count created_at updated_at }
        }`
        )
        .then((state) => ({ ...state, total: state.bucketSummaries.length }));
    });
}

function readBucketsWithoutSummaries(): Promise<BucketPageState> {
  return api
    .mcpGraphql<{ buckets: Bucket[] }>(
      `query {
        buckets { id name is_default created_at updated_at }
      }`
    )
    .then(({ buckets }) => ({
      // Older Engine processes may still be live while the UI has already
      // shipped the aggregate field. Keep the page usable with a GraphQL-only
      // read, then real counts appear once that process is restarted.
      bucketSummaries: buckets.map((bucket) => ({
        ...bucket,
        secret_count: 0,
        value_count: 0,
        connected_user_count: 0,
      })),
      total: buckets.length,
    }));
}

export function readBucketContents(
  bucketId: string,
  pages: BucketContentPages
): Promise<BucketContentState> {
  return readBucketContentPages(bucketId, pages).catch((err) => {
    if (!isUnknownGraphQLField(err, "bucketValuePage")) throw err;
    return readBucketEntries(bucketId);
  });
}

export function readBucketSummary(bucketId: string): Promise<BucketSummary> {
  return api
    .mcpGraphql<{ bucketSummary: BucketSummary }>(
      `query($bucketId: String!) {
        bucketSummary(bucket_id: $bucketId) { id name is_default secret_count value_count created_at updated_at }
      }`,
      { bucketId }
    )
    .then(({ bucketSummary }) => bucketSummary);
}

function readBucketContentPages(
  bucketId: string,
  pages: BucketContentPages
): Promise<BucketContentState> {
  return api
    .mcpGraphql<{
      bucketValuePage: GraphQLPage<BucketValue>;
      secretMetaPage: GraphQLPage<SecretMeta>;
      authConnectionPage: GraphQLPage<AuthConnection>;
      bucketSDKPage: GraphQLPage<BucketSDKSummary>;
      connectionServicePage: GraphQLPage<BucketServiceSummary>;
      bucketServicePage: GraphQLPage<BucketServiceSummary>;
      connectSummary: BucketConnectSummary | null;
    }>(
      `query(
        $bucketId: String!,
        $secretLimit: Int!, $secretOffset: Int!,
        $valueLimit: Int!, $valueOffset: Int!,
        $connectionLimit: Int!, $connectionOffset: Int!, $connectionServiceId: String,
        $connectionServiceSearch: String,
        $sdkLimit: Int!, $sdkOffset: Int!,
        $serviceLimit: Int!, $serviceOffset: Int!, $serviceSearch: String
      ) {
        bucketValuePage(bucket_id: $bucketId, limit: $valueLimit, offset: $valueOffset) {
          total
          items { id bucket_id service_id key_name location value created_at updated_at }
        }
        secretMetaPage(bucket_id: $bucketId, limit: $secretLimit, offset: $secretOffset) {
          total
          items { id bucket_id service_id key_name key_names credential_type last_used_at expires_at created_at updated_at }
        }
        authConnectionPage(bucket_id: $bucketId, service_id: $connectionServiceId, limit: $connectionLimit, offset: $connectionOffset) {
          total
          items { id bucket_id service_id end_user_ref auth_type token_type scopes scope_source issuer subject expires_at refresh_token_expires_at last_used_at refresh_state last_failure_code last_failure_at last_failure_trace_id created_at updated_at }
        }
        bucketSDKPage(bucket_id: $bucketId, limit: $sdkLimit, offset: $sdkOffset) {
          total
          items { id name kind active created_at }
        }
        connectionServicePage: bucketServicePage(bucket_id: $bucketId, search: $connectionServiceSearch, limit: 20, offset: 0) {
          items { service_id service_name secret_count value_count connect_config_count connected_user_count }
        }
        bucketServicePage(bucket_id: $bucketId, search: $serviceSearch, limit: $serviceLimit, offset: $serviceOffset) {
          total
          items { service_id service_name secret_count value_count connect_config_count connected_user_count }
        }
        connectSummary: bucketConnectSummary(bucket_id: $bucketId) { bucket_id connect_config_count connected_user_count }
      }`,
      {
        bucketId,
        secretLimit: pages.secrets.limit,
        secretOffset: pages.secrets.offset,
        valueLimit: pages.values.limit,
        valueOffset: pages.values.offset,
        connectionLimit: pages.connections.limit,
        connectionOffset: pages.connections.offset,
        connectionServiceId: pages.connections.serviceId || "",
        connectionServiceSearch: pages.connectionServices.search || "",
        sdkLimit: pages.sdks.limit,
        sdkOffset: pages.sdks.offset,
        serviceLimit: pages.services.limit,
        serviceOffset: pages.services.offset,
        serviceSearch: pages.services.search || "",
      }
    )
    .then((state) => ({
      bucketValues: state.bucketValuePage.items,
      bucketValueTotal: state.bucketValuePage.total,
      secretMetas: state.secretMetaPage.items,
      secretMetaTotal: state.secretMetaPage.total,
      authConnections: state.authConnectionPage.items,
      authConnectionTotal: state.authConnectionPage.total,
      bucketSDKs: state.bucketSDKPage.items,
      bucketSDKTotal: state.bucketSDKPage.total,
      bucketServices: state.bucketServicePage.items,
      bucketServiceTotal: state.bucketServicePage.total,
      connectionServices: state.connectionServicePage.items,
      connectSummary: state.connectSummary,
    }));
}

function readBucketEntries(bucketId: string): Promise<BucketContentState> {
  return api.mcpGraphql<BucketContentState>(
    `query($bucketId: String!) {
      bucketValues(bucket_id: $bucketId) { id bucket_id service_id key_name location value created_at updated_at }
      secretMetas(bucket_id: $bucketId) { id bucket_id service_id key_name credential_type last_used_at expires_at created_at updated_at }
      authConnections(bucket_id: $bucketId) { id bucket_id service_id end_user_ref auth_type token_type scopes scope_source issuer subject expires_at refresh_token_expires_at last_used_at refresh_state last_failure_code last_failure_at last_failure_trace_id created_at updated_at }
    }`,
    { bucketId }
  );
}

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

export function readBucketsForSDK(artifactId: string): Promise<SDKBucketState> {
  return api.mcpGraphql<SDKBucketState>(
    `query($artifactId: String!) {
      buckets { id name is_default created_at updated_at }
      sdkBuckets(artifact_id: $artifactId) { id name is_default created_at updated_at }
    }`,
    { artifactId }
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

function isUnknownGraphQLField(err: unknown, fieldName: string): boolean {
  return (
    err instanceof Error &&
    err.message.includes(`Cannot query field "${fieldName}"`)
  );
}
