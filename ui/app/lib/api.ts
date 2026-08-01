import { getApiKey, clearApiKey } from "./session";
import type { ServiceAuthOption } from "./service-auth";
import {
  APIRequestError,
  isAuthenticationFailure,
  normalizeAPIErrorPayload,
  type APIErrorPayload,
} from "./authorization-error";

// When served as an embedded SPA from the engine, BACKEND_URL is set to ""
// (explicitly empty) in root.tsx so all API calls are relative to the engine's
// own origin. BASE is kept for external callers that reference it directly,
// but req() calls getBaseURL() lazily on every request so window.ENV is read
// AFTER React renders and injects the <script>window.ENV = {...}</script> tag.
function getBaseURL(): string {
  if (typeof window !== "undefined") {
    // env.js is loaded by the document shell before Remix starts. Reading its
    // stable value avoids relying on a client-inserted script, which browsers
    // do not execute, and keeps embedded requests on the current Engine origin.
    const runtimeWindow = window as Window & {
      __FUSED_ENV?: { BACKEND_URL?: string };
      ENV?: { BACKEND_URL?: string };
    };
    const envUrl =
      runtimeWindow.__FUSED_ENV?.BACKEND_URL ??
      runtimeWindow.ENV?.BACKEND_URL;
    if (envUrl !== undefined && envUrl !== null) return envUrl; // "" → relative, "https://…" → absolute
  }
  if (typeof process !== "undefined" && process.env.BACKEND_URL != null) {
    return process.env.BACKEND_URL;
  }
  // Embedded builds are same-origin. Local development can still provide an
  // explicit BACKEND_URL through env.js or the process environment.
  return "";
}
/** @deprecated prefer getBaseURL() which reads window.ENV lazily */
export const BASE = getBaseURL();

async function req<T>(
  path: string,
  init: RequestInit = {},
  serverToken?: string
): Promise<T> {
  const base = getBaseURL();
  const envToken =
    typeof window !== "undefined" ? (window as any).ENV?.API_KEY : null;
  const key = serverToken || envToken || getApiKey() || "";
  const res = await fetch(`${base}${path}`, {
    ...init,
    headers: {
      "Content-Type": "application/json",
      "X-API-Key": key,
      ...(init.headers ?? {}),
    },
  });

  const data = await res.json().catch(() => ({}));

  const errorPayload = normalizeAPIErrorPayload(data);
  handleAuthenticationFailure(res.status, errorPayload);

  if (!res.ok) throw new APIRequestError(res.status, errorPayload);
  return data as T;
}

function handleAuthenticationFailure(
  status: number,
  payload: APIErrorPayload
): void {
  if (!isAuthenticationFailure(status, payload)) return;
  clearApiKey();
  if (shouldRedirectToLogin()) window.location.href = "/login";
}

function shouldRedirectToLogin(): boolean {
  if (typeof window === "undefined") return false;
  return (
    window.location.pathname !== "/login" && window.location.pathname !== "/"
  );
}

// ─── Types ────────────────────────────────────────────────────────────────────

export interface Account {
  id: string;
  name: string;
  // provider is the account's public, URL-safe identifier -- the segment
  // used in a service's public URL (/integrations/<provider>/<slug>).
  provider?: string;
  email?: string;
  credit_balance?: number;
  auto_topup_enabled?: boolean;
  auto_topup_threshold?: number;
  auto_topup_bundle_id?: string;
  created_at: string;
  updated_at: string;
}

export interface CreditBundle {
  id: string;
  name: string;
  credits: number;
  price_usd: number;
}

export interface CreditsPricing {
  bundles: CreditBundle[];
  actions: Record<string, number>;
}

// ─── Account Settings ─────────────────────────────────────────────────────────

export interface Service {
  current_service_version?: string;
  service_version_id?: string;
  id: string;
  name: string;
  slug?: string;
  // Provider identity is stable regardless of who is viewing the service;
  // is_owner independently controls management actions and local URL shape.
  provider?: { name: string; handle: string } | null;
  canonical_ref?: string | null;
  description: string;
  base_url: string;
  servers?: {
    url: string;
    description?: string;
    environment?: string;
    is_default?: boolean;
  }[];
  provider_servers?: {
    url: string;
    description?: string;
    environment?: string;
    is_default?: boolean;
  }[];
  source_id?: string;
  source_url?: string;
  import_method?: string;
  import_warnings?: ServiceImportWarning[];
  resources?: { id: string; name: string; endpointCount?: number }[];
  auth_configs: AuthConfig[];
  rate_limit?: {
    strategy: string;
    requests_per_second?: number;
    requests_per_minute?: number;
  };
  retry_config?: {
    strategy: string;
    max_retries?: number;
    backoff_ms?: number;
  };
  default_headers?: Record<string, string>;
  is_public: boolean;
  watch_for_drift: boolean;
  is_owner: boolean;
  webhook_count?: number;
  endpoint_count?: number;
  event_extraction_path?: string;
  incoming_webhook_config?: any;
  connect_config?: {
    auth_type?: "oauth" | "oidc";
    resource_input?: {
      fields: Array<{
        name: string;
        label?: string;
        required?: boolean;
        pattern?: string;
      }>;
    };
  };
  webhooks?: WebhookObject[];
  webhook_slug?: string;
  created_at: string;
  updated_at: string;
}

// serviceHref builds a service's detail-page path, provider-aware: a
// service with no `provider` (the caller's own -- see the field's doc
// comment above) keeps the existing single-segment /integrations/<slug>
// URL; a foreign public service (provider set, e.g. from a searchServices
// result) gets /integrations/<provider>/<slug> instead, since slugs are
// only unique within an account. Every place that links to a service detail
// page from a list/search result should build its href through this, not
// by hand, so the two URL shapes don't drift apart.
export function serviceHref(
  obj: Pick<Service, "id" | "slug" | "provider" | "is_owner">
): string {
  const idOrSlug = obj.slug || obj.id;
  if (obj.provider && obj.is_owner === false) {
    return `/integrations/${obj.provider.handle}/${idOrSlug}`;
  }
  return `/integrations/${idOrSlug}`;
}

export interface ServiceVersion {
  id: string;
  name: string;
  is_public: boolean;
  status?: "public" | "deprecated" | string;
  base_url?: string;
  auth_configs?: AuthConfig[];
  rate_limit?: {
    strategy: string;
    requests_per_second?: number;
    requests_per_minute?: number;
  };
  retry_config?: {
    strategy: string;
    max_retries?: number;
    backoff_ms?: number;
    retry_on?: number[];
  };
  event_extraction_path?: string;
  incoming_webhook_config?: {
    auth_type: string;
    auth_location?: string;
    auth_key_name?: string;
    signature_header?: string;
    verification_headers?: string[];
  };
  default_headers?: Record<string, string>;
  servers?: { url: string; description?: string }[];
  created_at: string;
  updated_at: string;
}

export interface ServiceImportWarning {
  id: string;
  endpoint_id?: string;
  method: string;
  path: string;
  operation_id?: string;
  reasons: string[];
  recommendation?: string;
  source?: string;
  created_at?: string;
}

export interface WebhookObject {
  id: string;
  service_id: string;
  name: string;
  description: string;
  method: string;
  request_body?: Schema;
}

export interface IntegrationObject {
  id: string;
  service_id: string;
  name: string;
  description: string;
  resource: string;
  resource_id?: string;
  version: string;
  method: string;
  path: string;
  deprecated?: boolean;
  deprecation_date?: string;
  parameters: Parameter[];
  request_body?: Schema;
  responses?: Record<string, Schema>;
  graphql_query?: string;
  spec_hash: string;
  created_at: string;
  updated_at: string;
  status: "active" | "drifted" | "updating";
  isWebhook?: boolean;
}

export interface ServiceGenerationResult {
  serviceVersions?: ServiceVersion[];
  service: Service;
  integrations: IntegrationObject[];
  source_url?: string;
  import_method?: string;
}

export interface Parameter {
  name: string;
  in: string;
  required: boolean;
  type: string;
  description: string;
}

export interface EndpointIdentifier {
  method: string;
  path: string;
  name?: string;
}

export interface WebhookIdentifier {
  name: string;
  method: string;
}

export interface PreviewOpenAPIResult {
  serviceName: string;
  integrations?: any[];
  webhooks?: any[];
}

export interface SpecificationImportPlan {
  plan_id: string;
  source_hash: string;
  service_id?: string;
  slug?: string;
  name: string;
  is_new_service: boolean;
  target_version: string;
  action: "create_service" | "update_version" | "create_version";
  diff: {
    added: number;
    changed: number;
    removed: number;
    changed_names?: string[];
    removed_names?: string[];
  };
}

export interface SpecificationImportApplyResult {
  status: "applied";
  plan_id: string;
  service_id: string;
  slug?: string;
  is_new_service: boolean;
  action: SpecificationImportPlan["action"];
  version: string;
  revision: number;
}

export interface Schema {
  type: string;
  properties?: Record<string, Schema>;
  required?: string[];
  example?: unknown;
  format?: string;
}

export interface AuthConfig {
  type: string;
  flow?: string;
  scheme?: string;
  location?: string;
  key_name?: string;
  token_url?: string;
  authorization_url?: string;
  open_id_connect_url?: string;
  scopes?: string[];
  pkce_required?: boolean;
  scopes_delimiter?: string;
  token_endpoint_auth?: string;
  extra_auth_params?: Record<string, string>;
  extra_token_params?: Record<string, string>;
  refresh_token_rotates?: boolean;
}

export interface DriftSnapshot {
  id: string;
  source_id: string;
  integration_object_id: string;
  previous_hash: string;
  current_hash: string;
  diff: DriftChange[];
  detected_at: string;
  status: "pending" | "applied" | "dismissed";
}

export interface DriftChange {
  field: string;
  old_value: unknown;
  new_value: unknown;
  severity: "breaking" | "non-breaking";
  description: string;
}

// WorkspaceNotification is one item from the Engine's workspaceNotifications
// GraphQL query (internal/engine/api/connect_graphql.go's
// workspaceNotificationGraphQLType) -- the merged inbox of Engine-local
// workspace_*/registry_* notifications (source: "engine") and live provider
// drift snapshots (source: "registry"). See the fused-notifications CLI
// skill and plans/plan-service-changelog.md's "## Phase 3"/"## Phase 4" for
// the full type/severity/dedupe semantics this mirrors.
export interface WorkspaceNotification {
  id: string;
  source: "engine" | "registry";
  type: string;
  severity: "breaking" | "non-breaking";
  status: "pending" | "acknowledged" | "dismissed";
  service_id?: string;
  version?: string;
  config_key?: string;
  message: string;
  integration_object_id?: string;
  webhook_object_id?: string;
  detected_at?: string;
  diff?: DriftChange[];
}

export interface WorkspaceNotificationInbox {
  items: WorkspaceNotification[];
  warnings: string[];
  // total_count/pending_count are always populated (paginated or not) --
  // see the backend's workspaceNotificationInbox doc comment. total_count
  // reflects whatever filter is active (all unresolved, or pending-only
  // when unreadOnly is passed), so it's the right number to drive a
  // numbered-page control off of.
  total_count: number;
  pending_count: number;
}

// NotificationServiceRef is the minimal shape notification rows need to
// render a real title and a working link instead of a raw service_id UUID
// -- resolved through the Registry's existing batched service lookup rather
// than adding a second backend query just for notification labels.
export interface NotificationServiceRef {
  id: string;
  name: string;
  slug?: string;
}

export interface AgentResponse {
  Status: string;
  Message?: string;
  Question?: string;
  Options?: string[];
  IntegrationID?: string;
  SessionID?: string;
  session_id?: string;
  service_id?: string;
}

export interface PaginatedResponse<T> {
  data: T[];
  total: number;
  page: number;
  limit: number;
  total_pages: number;
}

// ─── API methods ──────────────────────────────────────────────────────────────

// ActivatedService mirrors Engine GraphQL workspaceServices. New rows are
// pinned; empty version only represents historical activations created before
// Sprint 5 pinning.
export interface ActivatedService {
  id: string;
  workspace_id: string;
  service_id: string;
  service_version_id: string;
  /** Empty string only for historical unpinned rows. */
  version: string;
  /** Cached at add-time for offline resilience (Engine can list without Registry). */
  service_name: string;
  service_slug: string;
  enabled_versions?: Array<{
    id?: string;
    service_version_id?: string;
    version: string;
    status?: string;
    created_at?: string;
    enabled_at?: string;
  }>;
  auth_options?: ServiceAuthOption[];
  added_by: string;
  created_at: string;
}

export interface WebhookEventEntry {
  id: string;
  account_id: string;
  service_id: string;
  msg_id: string;
  event_name: string;
  status: string;
  delivery_status: string;
  verification_status: string;
  latency_ms: number;
  retry_count: number;
  credits_consumed: number;
  sdk_record_id?: string;
  error_reason: string;
  payload_size: number;
  created_at: string;
}

export interface WebhookAnalyticsSummary {
  total_ingested: number;
  total_delivered: number;
  total_rejected: number;
  total_failed: number;
}

export interface Bucket {
  id: string;
  workspace_id?: string;
  name: string;
  is_default: boolean;
  created_at?: string;
  updated_at?: string;
}

export interface BucketSummary extends Bucket {
  secret_count: number;
  value_count: number;
  connected_user_count: number;
}

export interface BucketConnectSummary {
  bucket_id: string;
  connect_config_count: number;
  connected_user_count: number;
}

export interface GraphQLPage<T> {
  items: T[];
  total: number;
}

export interface BucketSDKSummary {
  id: string;
  name: string;
  kind: string;
  active: boolean;
  created_at?: string;
}

export interface BucketServiceSummary {
  service_id: string;
  service_name: string;
  secret_count: number;
  value_count: number;
  connect_config_count: number;
  connected_user_count: number;
}

export interface AuthConnection {
  id: string;
  bucket_id: string;
  service_id: string;
  end_user_ref: string;
  auth_type: string;
  token_type: string;
  scopes: string[];
  scope_source: string;
  issuer?: string;
  subject?: string;
  expires_at?: string;
  refresh_token_expires_at?: string;
  last_used_at?: string;
  refresh_state: string;
  last_failure_code?: string;
  last_failure_at?: string;
  last_failure_trace_id?: string;
  created_at?: string;
  updated_at?: string;
}

export interface ConnectionResource {
  id: string;
  connection_id: string;
  service_id: string;
  provider_resource_id: string;
  resource_type: string;
  display_name: string;
  base_url: string;
  scopes: string[];
  is_default: boolean;
  created_at?: string;
  updated_at?: string;
}

export interface WorkspaceConnectionBinding {
  id: string;
  source_kind: "literal" | "connection_resource";
  literal_value?: string;
  source_path?: string;
  target_location: "base_url" | "header" | "query" | "path" | "body";
  target_name?: string;
  operation_ids: string[];
  mode: "default" | "force";
  provenance: "workspace" | "provider" | "fused";
  source_profile_revision?: number;
  locally_overridden?: boolean;
}

export interface WorkspaceConnectionProfile {
  service_id: string;
  service_version_id: string;
  auth_type: "oauth" | "oidc";
  profile_revision: number;
  profile_hash: string;
  provenance: "workspace" | "provider" | "fused";
  source?: "workspace" | "provider" | "fused";
  has_workspace_override?: boolean;
  profile: Record<string, unknown>;
  bindings: WorkspaceConnectionBinding[];
}

export interface SecretMeta {
  id: string;
  workspace_id?: string;
  bucket_id: string;
  service_id: string;
  key_name: string;
  key_names?: string[];
  credential_type: string;
  last_used_at?: string;
  expires_at?: string;
  created_at?: string;
  updated_at?: string;
}

export interface BucketValue {
  id: string;
  workspace_id?: string;
  bucket_id: string;
  service_id: string;
  key_name: string;
  location: string;
  value: string;
  created_at?: string;
  updated_at?: string;
}

export const api = {
  // Account
  getAccount: () => req<Account>("/account"),
  updateEmail: (email: string) =>
    req<{ status: string }>("/account", {
      method: "PUT",
      body: JSON.stringify({ email }),
    }),
  regenerateApiKey: () =>
    req<{ account_id: string; api_key: string; message: string }>(
      "/account/regenerate-key",
      { method: "POST" }
    ),
  updateAutoTopup: (enabled: boolean, threshold: number, bundleId: string) =>
    req<{ status: string }>("/account/auto-topup", {
      method: "PUT",
      body: JSON.stringify({ enabled, threshold, bundle_id: bundleId }),
    }),

  credits: {
    getPricing: () => req<CreditsPricing>("/credits/pricing"),
  },

  graphql: <T>(query: string, variables?: any, serverToken?: string) =>
    req<{ data: T; errors?: any[] }>(
      "/graphql",
      {
        method: "POST",
        body: JSON.stringify({ query, variables }),
      },
      serverToken
    ).then((res) => {
      if (res.errors && res.errors.length > 0)
        throw new Error(res.errors[0].message);
      return res.data;
    }),

  // mcpGraphql hits the Engine's own MCP GraphQL schema (list/deploy/kill/
  // reactivate/delete + analytics -- internal/engine/api/mcp_graphql.go),
  // a separate endpoint from api.graphql above, which is a pure Registry
  // forward-proxy with no MCP-aware resolvers of its own.
  mcpGraphql: <T>(query: string, variables?: any) =>
    req<{ data: T; errors?: any[] }>("/engine/graphql", {
      method: "POST",
      body: JSON.stringify({ query, variables }),
    }).then((res) => {
      if (res.errors && res.errors.length > 0)
        throw new Error(res.errors[0].message);
      return res.data;
    }),

  artifactConfig: {
    plan: <T>(kind: "sdk" | "mcp" | "webhook", input: {
      owner_team_id: string;
      config_key: string;
      source_hash: string;
      config: Record<string, unknown>;
    }) => req<T>(`/${kind}-config/plan`, { method: "POST", body: JSON.stringify(input) }),
    apply: <T>(kind: "sdk" | "mcp" | "webhook", input: { plan_id: string; source_hash: string }) =>
      req<T>(`/${kind}-config/apply`, { method: "POST", body: JSON.stringify(input) }),
  },

  integrations: {
    planImport: (input: {
      name: string;
      slug?: string;
      version?: string;
      source_url?: string;
      source_content?: string;
    }) =>
      req<SpecificationImportPlan>("/integrations/import/plan", {
        method: "POST",
        body: JSON.stringify(input),
      }),

    applyImport: (planId: string, sourceHash: string) =>
      req<SpecificationImportApplyResult>("/integrations/import/apply", {
        method: "POST",
        body: JSON.stringify({ plan_id: planId, source_hash: sourceHash }),
      }),

    start: (
      name: string,
      serviceSlug: string,
      version: string,
      sourceUrl: string,
      importMethod: string,
      sourceContent?: string,
      selectedEndpoints?: EndpointIdentifier[],
      targetContext?: string,
      graphqlUrl?: string
    ) =>
      req<AgentResponse>("/integrations/start", {
        method: "POST",
        body: JSON.stringify({
          name,
          service_slug: serviceSlug,
          version,
          source_url: sourceUrl,
          import_method: importMethod,
          source_content: sourceContent,
          selected_endpoints: selectedEndpoints,
          target_resource_name: targetContext,
          graphql_url: graphqlUrl,
        }),
      }),

    previewOpenAPI: (
      sourceUrl: string,
      sourceContent?: string,
      targetType?: string
    ) =>
      req<PreviewOpenAPIResult>("/integrations/preview_openapi", {
        method: "POST",
        body: JSON.stringify({
          source_url: sourceUrl,
          source_content: sourceContent,
          target_type: targetType,
        }),
      }),

    respond: (sessionId: string, answer: string, rewindTo?: string) =>
      req<AgentResponse>("/integrations/respond", {
        method: "POST",
        body: JSON.stringify({
          session_id: sessionId,
          answer,
          rewind_to: rewindTo,
        }),
      }),

    getActiveSessions: () => req<any[]>("/integrations/sessions/active"),

    recoverSession: (sessionId: string) =>
      req<void>(`/integrations/session/${sessionId}/recover`, {
        method: "POST",
      }),
    cancelSession: (sessionId: string) =>
      req<void>(`/integrations/session/${sessionId}/cancel`, {
        method: "POST",
      }),
    deleteSession: (sessionId: string) =>
      req<void>(`/integrations/session/${sessionId}`, { method: "DELETE" }),

    getSession: (sessionId: string) =>
      req<any>(`/integrations/session/${sessionId}`),

    delete: (id: string) =>
      req<void>(`/integrations/${id}`, { method: "DELETE" }),

    updateDriftWatch: (id: string, watch: boolean) =>
      req<{ status: string }>(`/integrations/${id}/drift-watch`, {
        method: "PUT",
        body: JSON.stringify({ watch_for_drift: watch }),
      }),
    // rotateWebhookSlug / generateAccountWebhook / updateAccountWebhookSecret
    // were removed: webhooks are Engine-owned now (engine_owned_webhooks_plan.md,
    // Task 7 dropped account_webhooks and these Registry routes entirely).
    // Equivalent read-only visibility lives on Engine GraphQL; secrets are set via the CLI
    // (`fused secret set`), which is write-only by design.

    createLead: (lead: {
      name: string;
      email: string;
      role: string;
      language: string;
      reason: string;
    }) =>
      req<{ status: string }>("/leads", {
        method: "POST",
        body: JSON.stringify(lead),
      }),

    updatePublic: (id: string, isPublic: boolean) =>
      req<void>(`/integrations/${id}/public`, {
        method: "PUT",
        body: JSON.stringify({ is_public: isPublic }),
      }),

    // updateVersionPublic is updatePublic's per-version sibling: it sets
    // is_public on just one version, independent of the service's own
    // visibility (owner only, enforced server-side).
    updateVersionPublic: (id: string, version: string, isPublic: boolean) =>
      req<void>(`/integrations/${id}/versions/${encodeURIComponent(version)}/public`, {
        method: "PUT",
        body: JSON.stringify({ is_public: isPublic }),
      }),

    clearImportWarnings: (id: string) =>
      req<void>(`/integrations/${id}/import-warnings`, { method: "DELETE" }),

    dismissDrift: (id: string, driftId: string) =>
      req(`/integrations/${id}/drift/${driftId}/dismiss`, {
        method: "POST",
        body: "{}",
      }),
  },

  sdks: {
    generateAsync: (
      name: string,
      description: string,
      version: string,
      targetType: string,
      targetLanguage: string,
      selections: {
        service_id: string;
        service_name?: string;
        service_slug?: string;
        endpoint_ids: string[];
        webhook_ids?: string[];
        service_version_id?: string;
      }[],
      skipSandbox?: boolean,
      upgradeFrom?: string
    ) =>
      req<{ job_id: string }>("/sdks/generate", {
        method: "POST",
        body: JSON.stringify({
          name,
          description,
          version,
          target_type: targetType,
          target_language: targetLanguage,
          selections,
          skip_sandbox: skipSandbox,
          upgrade_from: upgradeFrom,
        }),
      }),
    upgradeAsync: (id: string) =>
      req<{ job_id: string }>(`/sdks/${id}/upgrade`, {
        method: "PUT",
        body: "{}",
      }),
    download: async (id: string, name: string, version: string) => {
      const key = getApiKey() ?? "";
      const res = await fetch(`${BASE}/sdks/${id}/download`, {
        headers: { "X-API-Key": key },
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const blob = await res.blob();
      const url = window.URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `${name}-v${version}.zip`;
      document.body.appendChild(a);
      a.click();
      a.remove();
      window.URL.revokeObjectURL(url);
    },
    // delete: still the Registry-proxied path -- for plain (non-MCP) SDKs
    // only. MCP servers are deleted via the deleteMcpServer GraphQL mutation
    // (api.mcpGraphql) instead, since an Engine-native MCP scope has no
    // Registry sdks row to delete. kill/restart/regenerateToken were removed
    // here along with their Registry routes (MCP creation no longer
    // generates a per-SDK sandbox) -- see integrations/mcp.tsx for their
    // GraphQL replacements (kill/reactivate; there is no token-rotation
    // equivalent in the new model, so that action was dropped).
    delete: (id: string) => req<void>(`/sdks/${id}`, { method: "DELETE" }),
  },

  // api.auth (the hosted Connect runtime's setOAuthConfig/getConnections/
  // deleteConnection/getWebhookAttempts) was removed along with the
  // connect.$artifact_id.$service_id and connect.callback routes that were its
  // only callers, and the backend endpoints behind it.

  // S2: Workspace membership — Engine-local (not proxied to the Registry).
  // The Engine owns which services are in a workspace; the Registry owns the
  // service definitions themselves.
  workspace: {
    // addService can include serviceVersionId when the caller already has an
    // exact Registry version. That avoids asking Engine to infer a version.
    addService: (
      serviceId: string,
      serviceName: string,
      versionTag: string,
      serviceVersionId?: string
    ) =>
      req<{ status: string }>("/workspace/services", {
        method: "POST",
        body: JSON.stringify({
          service_id: serviceId,
          service_name: serviceName,
          version_tag: versionTag,
          service_version_id: serviceVersionId,
        }),
      }),

    removeService: (serviceId: string) =>
      req<void>(`/workspace/services/${serviceId}`, { method: "DELETE" }),

    getServices: () =>
      api
        .mcpGraphql<{ workspaceServices: ActivatedService[] }>(
          `query {
            workspaceServices {
              id
              workspace_id
              service_id
              service_version_id
              version
              service_name
              service_slug
              added_by
              created_at
              enabled_versions { id service_version_id version status created_at enabled_at }
              auth_options { id label auth_type credential_type key_name key_prefix required_fields supports_connected_users }
            }
          }`
        )
        .then(({ workspaceServices }) => workspaceServices),

    getServicesPage: (limit: number, offset: number, names?: string[]) =>
      api
        .mcpGraphql<{ workspaceServicePage: { total: number; page: number; limit: number; data: ActivatedService[] } }>(
          `query WorkspaceServicePage($limit: Int, $offset: Int, $names: [String]) {
            workspaceServicePage(limit: $limit, offset: $offset, names: $names) {
              total page limit data {
                id
                workspace_id
                service_id
                service_version_id
                version
                service_name
                service_slug
                added_by
                created_at
                enabled_versions { id service_version_id version status created_at enabled_at }
                auth_options { id label auth_type credential_type key_name key_prefix required_fields supports_connected_users }
              }
            }
          }`,
          { limit, offset, names }
        )
        .then(({ workspaceServicePage }) => workspaceServicePage),

    // Webhook delivery events/analytics are Engine-owned data
    // (fused_webhook_events) that never touches the Registry.
    listWebhookEvents: (params: {
      serviceId: string;
      eventName?: string;
      limit?: number;
      offset?: number;
      startDate?: string;
      endDate?: string;
    }) =>
      api
        .mcpGraphql<{
          webhookEvents: { items: WebhookEventEntry[]; total: number };
        }>(
          `query WebhookEvents($serviceId: String!, $eventName: String, $limit: Int, $offset: Int, $startDate: String, $endDate: String) {
            webhookEvents(service_id: $serviceId, event_name: $eventName, limit: $limit, offset: $offset, start_date: $startDate, end_date: $endDate) {
              total
              items {
                id
                account_id
                service_id
                msg_id
                event_name
                status
                delivery_status
                verification_status
                latency_ms
                retry_count
                credits_consumed
                sdk_record_id
                error_reason
                payload_size
                created_at
              }
            }
          }`,
          {
            serviceId: params.serviceId,
            eventName: params.eventName || null,
            limit: params.limit ?? null,
            offset: params.offset ?? null,
            startDate: params.startDate || null,
            endDate: params.endDate || null,
          }
        )
        .then(({ webhookEvents }) => webhookEvents),

    getWebhookAnalytics: (params: {
      serviceId: string;
      eventName?: string;
      startDate?: string;
      endDate?: string;
    }) =>
      api
        .mcpGraphql<{ webhookAnalytics: WebhookAnalyticsSummary }>(
          `query WebhookAnalytics($serviceId: String!, $eventName: String, $startDate: String, $endDate: String) {
            webhookAnalytics(service_id: $serviceId, event_name: $eventName, start_date: $startDate, end_date: $endDate) {
              total_ingested
              total_delivered
              total_rejected
              total_failed
            }
          }`,
          {
            serviceId: params.serviceId,
            eventName: params.eventName || null,
            startDate: params.startDate || null,
            endDate: params.endDate || null,
          }
        )
        .then(({ webhookAnalytics }) => webhookAnalytics),

    createBucket: (name: string) =>
      req<Bucket>("/workspace/buckets", {
        method: "POST",
        body: JSON.stringify({ name }),
      }),

    deleteBucket: (name: string) =>
      req<void>(`/workspace/buckets/${encodeURIComponent(name)}`, {
        method: "DELETE",
      }),

    upsertSecret: (payload: {
      bucketId: string;
      serviceId: string;
      keyName: string;
      credentialType: string;
      value: string;
      expiresAt?: string;
    }) =>
      req<void>("/workspace/secrets", {
        method: "PUT",
        body: JSON.stringify({
          bucket_id: payload.bucketId,
          service_id: payload.serviceId,
          key_name: payload.keyName,
          credential_type: payload.credentialType,
          value: payload.value,
          expires_at: payload.expiresAt || undefined,
        }),
      }),

    upsertSecrets: (payload: {
      bucketId: string;
      secrets: Array<{
        serviceId: string;
        keyName: string;
        credentialType: string;
        value: string;
        expiresAt?: string;
      }>;
    }) =>
      api
        .mcpGraphql<{ upsertSecrets: boolean }>(
          `mutation($bucketId: String!, $secrets: [SecretUpsertInput]!) {
          upsertSecrets(bucket_id: $bucketId, secrets: $secrets)
        }`,
          {
            bucketId: payload.bucketId,
            secrets: payload.secrets.map((secret) => ({
              service_id: secret.serviceId,
              key_name: secret.keyName,
              credential_type: secret.credentialType,
              value: secret.value,
              expires_at: secret.expiresAt || undefined,
            })),
          }
        )
        .then(() => undefined),

    deleteSecret: (bucketId: string, serviceId: string, keyName: string) => {
      const q = new URLSearchParams({
        bucket_id: bucketId,
        service_id: serviceId,
        key_name: keyName,
      });
      return req<void>(`/workspace/secrets?${q.toString()}`, {
        method: "DELETE",
      });
    },

    deleteSecrets: (bucketId: string, serviceId: string, keyNames: string[]) =>
      api
        .mcpGraphql<{ deleteSecrets: boolean }>(
          `mutation($bucketId: String!, $serviceId: String!, $keyNames: [String!]!) {
          deleteSecrets(bucket_id: $bucketId, service_id: $serviceId, key_names: $keyNames)
        }`,
          { bucketId, serviceId, keyNames }
        )
        .then(() => undefined),

    // Resource reads stay on Engine GraphQL so connection ownership is checked
    // in the same API boundary that owns connected credentials.
    listConnectionResources: (connectionId: string) =>
      api
        .mcpGraphql<{ connectionResources: ConnectionResource[] }>(
          `query($connectionId: String!) {
            connectionResources(connection_id: $connectionId) {
              id connection_id service_id provider_resource_id resource_type
              display_name base_url scopes is_default created_at updated_at
            }
          }`,
          { connectionId }
        )
        .then(({ connectionResources }) => connectionResources),

    // The server performs the default swap atomically; callers never need to
    // issue an unset followed by a set that could leave routing ambiguous.
    setDefaultConnectionResource: (connectionId: string, resourceId: string) =>
      api
        .mcpGraphql<{
          setDefaultConnectionResource: ConnectionResource;
        }>(
          `mutation($connectionId: String!, $resourceId: String!) {
            setDefaultConnectionResource(connection_id: $connectionId, resource_id: $resourceId) {
              id connection_id service_id provider_resource_id resource_type
              display_name base_url scopes is_default created_at updated_at
            }
          }`,
          { connectionId, resourceId }
        )
        .then(
          ({ setDefaultConnectionResource }) => setDefaultConnectionResource
        ),

    // Rediscovery is an explicit lifecycle action because provider membership
    // can change independently of the OAuth token or local bucket settings.
    rediscoverConnectionResources: (connectionId: string) =>
      api
        .mcpGraphql<{ rediscoverConnectionResources: ConnectionResource[] }>(
          `mutation($connectionId: String!) {
            rediscoverConnectionResources(connection_id: $connectionId) {
              id connection_id service_id provider_resource_id resource_type
              display_name base_url scopes is_default created_at updated_at
            }
          }`,
          { connectionId }
        )
        .then(
          ({ rediscoverConnectionResources }) => rediscoverConnectionResources
        ),

    getWorkspaceConnectionProfile: (input: {
      serviceId: string;
      serviceVersionId: string;
      authType: string;
    }) =>
      api
        .mcpGraphql<{
          workspaceConnectionProfile: WorkspaceConnectionProfile | null;
        }>(
          `query($serviceId: String!, $serviceVersionId: String!, $authType: String!) {
            workspaceConnectionProfile(
              service_id: $serviceId,
              service_version_id: $serviceVersionId,
              auth_type: $authType
            ) {
              service_id service_version_id auth_type
              profile_revision profile_hash provenance source has_workspace_override profile
              bindings {
                id source_kind source_path target_location target_name
                operation_ids mode provenance source_profile_revision
              }
            }
          }`,
          input
        )
        .then(({ workspaceConnectionProfile }) => workspaceConnectionProfile),

    setWorkspaceConnectionProfile: (input: {
      serviceId: string;
      serviceVersionId: string;
      version: string;
      authType: string;
      profile: Record<string, unknown>;
    }) =>
      api
        .mcpGraphql<{ setWorkspaceConnectionProfile: WorkspaceConnectionProfile }>(
          `mutation($serviceId: String!, $serviceVersionId: String!, $version: String!, $authType: String!, $profile: EngineJSON!) {
            setWorkspaceConnectionProfile(
              service_id: $serviceId,
              service_version_id: $serviceVersionId, version: $version,
              auth_type: $authType, profile: $profile
            ) {
              service_id service_version_id auth_type
              profile_revision profile_hash provenance source has_workspace_override profile
              bindings {
                id source_kind source_path target_location target_name
                operation_ids mode provenance source_profile_revision
              }
            }
          }`,
          input
        )
        .then(({ setWorkspaceConnectionProfile }) => setWorkspaceConnectionProfile),

    resetWorkspaceConnectionProfile: (input: {
      serviceId: string;
      serviceVersionId: string;
      authType: string;
    }) =>
      api
        .mcpGraphql<{ resetWorkspaceConnectionProfile: WorkspaceConnectionProfile | null }>(
          `mutation($serviceId: String!, $serviceVersionId: String!, $authType: String!) {
            resetWorkspaceConnectionProfile(
              service_id: $serviceId,
              service_version_id: $serviceVersionId, auth_type: $authType
            ) {
              service_id service_version_id auth_type
              profile_revision profile_hash provenance source has_workspace_override profile
              bindings {
                id source_kind source_path target_location target_name
                operation_ids mode provenance source_profile_revision
              }
            }
          }`,
          input
        )
        .then(({ resetWorkspaceConnectionProfile }) => resetWorkspaceConnectionProfile),

    // listNotifications reads the Engine's merged inbox (Engine-local
    // workspace_*/registry_* rows plus live provider drift) -- the same
    // query `fused-cli plan`/`apply` already prints from, now also readable
    // here for the bell panel / contextual page banners. See
    // plans/plan-service-changelog.md's "## Phase 4".
    listNotifications: () =>
      api
        .mcpGraphql<{ workspaceNotifications: WorkspaceNotificationInbox }>(
          `query {
            workspaceNotifications {
              items {
                id source type severity status service_id version config_key
                message integration_object_id webhook_object_id detected_at
                diff { field old_value new_value severity description }
              }
              warnings
              total_count
              pending_count
            }
          }`
        )
        .then(({ workspaceNotifications }) => workspaceNotifications),

    // listNotificationsPage is listNotifications' paginated sibling for the
    // full /integrations/notifications page (offset pagination, numbered
    // pages -- see plans/plan-service-changelog.md's Phase 4 pagination
    // follow-up). Unlike listNotifications, this never includes live
    // registry drift snapshots (backend's workspaceNotificationInbox skips
    // that enrichment once limit>0) -- drift snapshots aren't paginated,
    // Engine-local notifications are. page is 1-indexed.
    listNotificationsPage: (page: number, limit: number, unreadOnly: boolean, readOnly: boolean) =>
      api
        .mcpGraphql<{ workspaceNotifications: WorkspaceNotificationInbox }>(
          `query($page: Int!, $limit: Int!, $unreadOnly: Boolean!, $readOnly: Boolean!) {
            workspaceNotifications(page: $page, limit: $limit, unread_only: $unreadOnly, read_only: $readOnly) {
              items {
                id source type severity status service_id version config_key
                message integration_object_id webhook_object_id detected_at
                diff { field old_value new_value severity description }
              }
              warnings
              total_count
              pending_count
            }
          }`,
          { page, limit, unreadOnly, readOnly }
        )
        .then(({ workspaceNotifications }) => workspaceNotifications),

    // updateNotificationStatus is the one write path Phase 4 adds -- "id"
    // must be a store.WorkspaceNotification's own id (source: "engine"),
    // never a "registry:"-prefixed live drift item's composite id; those
    // aren't notifications this mutation can act on (see the backend
    // resolver's own doc comment for why).
    updateNotificationStatus: (id: string, status: "acknowledged" | "dismissed") => {
      const rawId = id.replace(/^(engine|registry):/, "");
      return api
        .mcpGraphql<{ updateWorkspaceNotificationStatus: WorkspaceNotification }>(
          `mutation($id: String!, $status: String!) {
            updateWorkspaceNotificationStatus(id: $id, status: $status) {
              id source type severity status service_id version config_key
              message integration_object_id webhook_object_id detected_at
              diff { field old_value new_value severity description }
            }
          }`,
          { id: rawId, status }
        )
        .then(({ updateWorkspaceNotificationStatus }) => updateWorkspaceNotificationStatus);
    },

    // listServiceRefsByIds resolves notification.service_id (a bare UUID)
    // into a real name + slug for display/linking, via the Registry's
    // existing servicesByIds batch lookup (api.graphql, not api.mcpGraphql
    // -- this is a pure Registry field, no Engine resolver involved).
    // Deduplicate ids before calling; an empty list short-circuits without
    // a network call.
    listServiceRefsByIds: (serviceIds: string[]) =>
      serviceIds.length === 0
        ? Promise.resolve([] as NotificationServiceRef[])
        : api
            .graphql<{ servicesByIds: NotificationServiceRef[] }>(
              `query($serviceIds: [String!]!) {
                servicesByIds(serviceIds: $serviceIds) {
                  id name slug
                }
              }`,
              { serviceIds }
            )
            .then(({ servicesByIds }) => servicesByIds),

    upsertBucketValue: (payload: {
      bucketId: string;
      serviceId: string;
      keyName: string;
      location: string;
      value: string;
    }) =>
      req<void>(
        `/workspace/buckets/${encodeURIComponent(payload.bucketId)}/values`,
        {
          method: "PUT",
          body: JSON.stringify({
            service_id: payload.serviceId,
            key_name: payload.keyName,
            location: payload.location,
            value: payload.value,
          }),
        }
      ),

    deleteBucketValue: (
      bucketId: string,
      serviceId: string,
      keyName: string
    ) => {
      const q = new URLSearchParams({
        bucket_id: bucketId,
        service_id: serviceId,
        key_name: keyName,
      });
      return req<void>(
        `/workspace/buckets/${encodeURIComponent(
          bucketId
        )}/values?${q.toString()}`,
        { method: "DELETE" }
      );
    },
  },

  health: () => req<{ status: string }>("/health"),
};
