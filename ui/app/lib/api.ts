import { getCSRFToken, purgeLegacyBrowserCredential } from "./session";
import { credentialedRequestInit, credentialedResponseLoginPath } from "./browser-request";
import type { ServiceAuthOption } from "./service-auth";
import type { RateLimitConfig } from "./rate-limit";
import type { RetryConfig } from "./retry-policy";
import { unwrapGraphQLResponse, type GraphQLResponse } from "./graphql-response";
import { mutableWorkspaceNotificationID } from "./workspace-notification";
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
    if (envUrl !== undefined && envUrl !== null) return sameOriginBaseURL(envUrl);
  }
  if (typeof process !== "undefined" && process.env.BACKEND_URL != null) {
    return process.env.BACKEND_URL;
  }
  // Embedded builds are same-origin. Local development can still provide an
  // explicit BACKEND_URL through env.js or the process environment.
  return "";
}

function sameOriginBaseURL(value: string): string {
  if (value === "" || value.startsWith("/")) return value;
  const configured = new URL(value, window.location.origin);
  if (configured.origin !== window.location.origin) {
    // Host-only session and CSRF cookies cannot safely support a split-origin
    // browser UI. Deployments should reverse-proxy the Engine on this origin.
    throw new Error("Fused browser authentication requires a same-origin BACKEND_URL");
  }
  return configured.href.replace(/\/$/, "");
}
/** @deprecated prefer getBaseURL() which reads window.ENV lazily */
export const BASE = getBaseURL();

// discoveryStreamURL resolves runtime environment injection lazily and safely encodes the opaque session ID.
export function discoveryStreamURL(sessionID: string): string {
  return `${getBaseURL()}/integrations/session/${encodeURIComponent(sessionID)}/stream`;
}

/** Sends one credentialed JSON request and normalizes HTTP authentication failures. */
async function req<T>(
  path: string,
  init: RequestInit = {}
): Promise<T> {
  const base = getBaseURL();
  const res = await fetch(`${base}${path}`, credentialedRequestInit(init, getCSRFToken()));

  const data = await res.json().catch(() => ({}));

  const errorPayload = normalizeAPIErrorPayload(data);
  handleAuthenticationFailure(res.status, errorPayload);

  if (!res.ok) throw new APIRequestError(res.status, errorPayload);
  return data as T;
}

// handleAuthenticationFailure normalizes JSON API auth failures through the same redirect boundary as streams.
function handleAuthenticationFailure(
  status: number,
  payload: APIErrorPayload
): void {
  if (!isAuthenticationFailure(status, payload)) return;
  purgeLegacyBrowserCredential();
  redirectCredentialedResponse(status);
}

// handleCredentialedResponse gives streaming and non-JSON requests the shared 401 redirect behavior.
export function handleCredentialedResponse(response: Response): void {
  if (response.status !== 401) return;
  purgeLegacyBrowserCredential();
  redirectCredentialedResponse(response.status);
}

// redirectCredentialedResponse performs the browser mutation only after the pure route guard admits it.
function redirectCredentialedResponse(status: number): void {
  if (typeof window === "undefined") return;
  const loginPath = credentialedResponseLoginPath(status, window.location.pathname, window.location.search);
  if (loginPath) window.location.href = loginPath;
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

export interface ConnectBrandingInput {
  display_name: string;
  logo_url: string;
  primary_color: string;
  support_url: string;
  privacy_url: string;
}

export interface ConnectBranding extends ConnectBrandingInput {
  created_at?: string;
  updated_at?: string;
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
  rate_limit?: RateLimitConfig;
  retry_config?: RetryConfig;
  default_headers?: Record<string, string>;
  is_public: boolean;
  watch_for_drift: boolean;
  is_owner: boolean;
  webhook_count?: number;
  webhooks?: WebhookObject[];
  endpoint_count?: number;
  event_extraction_path?: string;
  incoming_webhook_config?: {
    auth_type?: string;
    auth_location?: string;
    auth_key_name?: string;
    signature_header?: string;
  };
  connect_config?: {
    auth_type?: "oauth" | "oidc";
    resource_input?: {
      fields: Array<{
        name: string;
        type?: "text" | "select";
        label?: string;
        placeholder?: string;
        description?: string;
        required?: boolean;
        pattern?: string;
        options?: Array<{ value: string; label?: string }>;
      }>;
    };
  };
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
  rate_limit?: RateLimitConfig;
  retry_config?: RetryConfig;
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
  normalized_path?: string;
  stable_key?: string;
  provider_protocol?: string;
  operation_kind?: string;
  deprecated?: boolean;
  deprecation_date?: string;
  parameters: Parameter[];
  request_content?: RequestContentContract;
  // Webhook rows share the details sidebar but retain their distinct payload field.
  request_body?: Schema;
  responses?: Record<string, ResponseContract>;
  security_requirements?: unknown[];
  pagination?: unknown;
  graphql_query?: string;
  spec_hash: string;
  created_at: string;
  updated_at: string;
  status: "active" | "drifted" | "updating";
  isWebhook?: boolean;
  // UI-only bookkeeping flag set after lazily fetching full endpoint details
  // (description/schemas) for the details sidebar; never sent by the API.
  _detailsLoaded?: boolean;
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
  path_encoding?: string;
}

export interface DiscoveryOperationSelection {
  method: string;
  path: string;
}

export type DiscoveryState =
  | "resolve_source"
  | "fetch_spec"
  | "crawl_docs"
  | "discover_operations"
  | "awaiting_selection"
  | "extract_contract"
  | "enrich_contract"
  | "awaiting_review"
  | "plan_ready"
  | "error"
  | "cancelled";

export type DiscoveryEventType =
  | "state_changed"
  | "source_candidate"
  | "source_resolved"
  | "crawl_progress"
  | "operations_discovered"
  | "selection_required"
  | "extraction_progress"
  | "draft_ready"
  | "enrichment_proposed"
  | "review_required"
  | "plan_ready"
  | "failed"
  | "cancelled";

export interface DiscoveryDiagnostic {
  severity?: string;
  code: string;
  message: string;
}

export interface DiscoveredOperation extends DiscoveryOperationSelection {
  summary?: string;
  occurrences: number;
}

export interface DiscoveryContractReceipt {
  draft_id: string;
  draft_revision: number;
  review_hash: string;
}

export interface DiscoveryReviewListCounts {
  total: number;
  returned: number;
  omitted: number;
}

export interface DiscoveryReviewServer {
  url: string;
  description?: string;
  environment?: string;
  is_default?: boolean;
}

export interface DiscoveryReviewOAuthFlow {
  type: string;
  authorization_url?: string;
  device_authorization_url?: string;
  token_url?: string;
  refresh_url?: string;
  scopes?: string[];
  scope_counts: DiscoveryReviewListCounts;
}

export interface DiscoveryReviewAuthScheme {
  name: string;
  type: string;
  scheme?: string;
  location?: string;
  key_name?: string;
  open_id_connect_url?: string;
  pkce_required?: boolean;
  token_endpoint_auth_method?: string;
  refresh_token_required?: boolean;
  refresh_token_rotates?: boolean;
  oauth_flows?: DiscoveryReviewOAuthFlow[];
  oauth_flow_counts: DiscoveryReviewListCounts;
}

export interface DiscoveryReviewParameter {
  name: string;
  in: string;
  required: boolean;
  type?: string;
}

export interface DiscoveryReviewResponse {
  status: string;
  media_types?: string[];
  media_type_counts: DiscoveryReviewListCounts;
}

export interface DiscoveryReviewSecurityAlternative {
  anonymous: boolean;
  schemes?: Array<{
    name: string;
    scopes?: string[];
    scope_counts: DiscoveryReviewListCounts;
  }>;
  scheme_counts: DiscoveryReviewListCounts;
}

export interface DiscoveryReviewOperation {
  method: string;
  path: string;
  operation_id: string;
  summary?: string;
  parameters?: DiscoveryReviewParameter[];
  parameter_counts: DiscoveryReviewListCounts;
  request_media_types?: string[];
  request_media_type_counts: DiscoveryReviewListCounts;
  responses?: DiscoveryReviewResponse[];
  response_counts: DiscoveryReviewListCounts;
  security?: DiscoveryReviewSecurityAlternative[];
  security_alternative_counts: DiscoveryReviewListCounts;
}

export interface DiscoveryReviewSummary {
  schema_version: 1;
  session_id: string;
  draft_id: string;
  draft_revision: number;
  review_hash: string;
  info: { title: string; version: string };
  servers?: DiscoveryReviewServer[];
  server_counts: DiscoveryReviewListCounts;
  auth_schemes?: DiscoveryReviewAuthScheme[];
  auth_scheme_counts: DiscoveryReviewListCounts;
  operations?: DiscoveryReviewOperation[];
  operation_counts: DiscoveryReviewListCounts;
  diagnostic_count: number;
  evidence_count: number;
}

export interface DiscoveryPlanReceipt {
  plan_id: string;
  review_hash: string;
}

export interface DiscoveryEnrichmentProposal {
  id: string;
  extension: string;
  pointer: string;
  scope: string;
  value: unknown;
  dependencies: string[];
  rationale: string;
  evidence: Array<{
    source_id: string;
    content_hash: string;
    source_url: string;
    locator: string;
    fact: string;
  }>;
  confidence: string;
  requires_confirmation: boolean;
}

export interface DiscoveryPayload {
  effective_workers: number;
  max_pages: number;
  max_depth: number;
  max_selections: number;
  diagnostics?: DiscoveryDiagnostic[];
  operations?: DiscoveredOperation[];
  proposals?: DiscoveryEnrichmentProposal[];
  contract?: DiscoveryContractReceipt;
  plan?: DiscoveryPlanReceipt;
  failure_code?: string;
}

export interface DiscoverySnapshot {
  version: 1;
  session_id: string;
  revision: number;
  draft_revision?: number;
  state: DiscoveryState;
  payload?: DiscoveryPayload;
}

export interface DiscoveryEventEnvelope {
  version: 1;
  session_id: string;
  revision: number;
  state: DiscoveryState;
  type: DiscoveryEventType;
  payload?: DiscoveryPayload;
}

export interface DiscoveryStartRequest {
  name: string;
  slug: string;
  version?: string;
  source_url: string;
  source_mode: "auto" | "spec" | "docs";
  requested_workers: number;
  crawl: { max_pages: number; max_depth: number };
}

export type DiscoveryActionRequest =
  | {
      version: 1;
      session_id: string;
      expected_revision: number;
      action: "select_operations";
      payload: { operations: DiscoveryOperationSelection[] };
    }
  | {
      version: 1;
      session_id: string;
      expected_revision: number;
      draft_revision: number;
      action: "accept_enrichment" | "reject_enrichment";
      payload: { proposal_ids: string[] };
    }
  | {
      version: 1;
      session_id: string;
      expected_revision: number;
      draft_revision: number;
      action: "update_overlay";
      payload: { overlay: Record<string, unknown> };
    }
  | {
      version: 1;
      session_id: string;
      expected_revision: number;
      draft_revision: number;
      action: "request_plan";
    }
  | {
      version: 1;
      session_id: string;
      expected_revision: number;
      draft_revision?: number;
      action: "cancel";
    };

export interface WebhookIdentifier {
  name: string;
  method: string;
}

export interface SpecificationImportPlan {
  plan_id: string;
  source_hash: string;
  review_hash: string;
  service_id?: string;
  slug?: string;
  name: string;
  is_new_service: boolean;
  target_version: string;
  action: "create_service" | "update_version" | "create_version";
  target_type?: string;
  destination_version?: string;
  webhook_draft?: { source_content: string };
  expected_target?: SpecificationImportTarget;
  usage?: { workspaces: unknown[] };
  diagnostics?: unknown;
  diff: {
    added: number;
    changed: number;
    removed: number;
    changed_names?: string[];
    removed_names?: string[];
    settings_changed?: boolean;
  };
}

export interface SpecificationImportTarget {
  service_id: string;
  service_version_id: string;
  revision: number;
}

export interface ServiceWebhookEditorSource extends SpecificationImportTarget {
  source_content: string;
}

export interface SpecificationImportStatus {
  status: string;
  operation_id: string;
  phase: string;
  commit_state: string;
  service_id?: string;
  service_version_id?: string;
  version?: string;
  revision?: number;
  code?: string;
  recovery?: string;
  guidance?: string;
}

export interface SpecificationImportApplyResult {
  status: "applied";
  plan_id: string;
  service_id: string;
  service_version_id: string;
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

export interface SchemaContract {
  raw?: Schema;
  projection?: Schema;
}

export interface RequestContentContract {
  default_media_type?: string;
  representations?: Array<{
    media_type?: string;
    serialization?: string;
    schema?: SchemaContract;
    item_schema?: SchemaContract;
  }>;
}

export interface ResponseContract {
  description?: string;
  representations?: Array<{
    media_type?: string;
    schema?: SchemaContract;
    item_schema?: SchemaContract;
    sse?: unknown;
  }>;
}

export interface AuthConfig {
  name?: string;
  type: string;
  scheme?: string;
  basic_password_mode?: string;
  location?: string;
  key_name?: string;
  open_id_connect_url?: string;
  oauth2_metadata_url?: string;
  deprecated?: boolean;
  oauth2_flows?: Partial<Record<OAuth2FlowName, OAuth2FlowContract>>;
  pkce_required?: boolean;
  scopes_delimiter?: string;
  token_endpoint_auth_method?: 'client_secret_basic' | 'client_secret_post';
  token_request_media_type?: string;
  extra_auth_params?: Record<string, string>;
  extra_token_params?: Record<string, string>;
  refresh_token_rotates?: boolean;
  strategy?: Record<string, unknown>;
  policy_provenance?: Record<string, unknown>;
}

export type OAuth2FlowName =
  | "implicit"
  | "password"
  | "clientCredentials"
  | "authorizationCode"
  | "deviceAuthorization";

export interface OAuth2FlowContract {
  authorization_url?: string;
  device_authorization_url?: string;
  token_url?: string;
  refresh_url?: string;
  scopes: Record<string, string>;
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

export interface PaginatedResponse<T> {
  data: T[];
  total: number;
  page: number;
  limit: number;
  total_pages: number;
}

// ─── API methods ──────────────────────────────────────────────────────────────

// ActivatedService mirrors Engine GraphQL workspaceServices. The singular
// version tuple is the latest enabled version used for service-level display;
// enabled_versions is the authoritative set of exact runnable versions.
export interface ActivatedService {
  id: string;
  service_id: string;
  service_version_id: string;
  /** Latest non-deprecated enabled version, not the service's only version. */
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
  environment: string;
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

// UnifiedExecutionStep describes orchestration outcomes without exposing mapped or provider data.
export interface UnifiedExecutionStep {
  target: string;
  phase: "forward" | "rollback";
  status: "success" | "error" | "skipped";
  error_code?: string;
}

export interface EngineExecutionEventEntry {
  id: string;
  execution_kind?: "physical" | "unified";
  parent_execution_id?: string;
  unified_target?: string;
  execution_phase?: "forward" | "rollback";
  unified_steps?: UnifiedExecutionStep[];
  trace_id?: string;
  span_id?: string;
  app_family_id?: string;
  app_id?: string;
  app_version?: string;
  app_kind?: "sdk" | "mcp" | "webhook";
  transport: "sdk" | "mcp" | "rest" | "webhook";
  provider_protocol?: "rest" | "graphql";
  direction: "inbound" | "outbound";
  service_id: string;
  service_version_id: string;
  service_name?: string;
  service_slug?: string;
  service_version?: string;
  operation_id?: string;
  webhook_id?: string;
  operation: string;
  event_name?: string;
  http_method?: string;
  request_path?: string;
  environment: string;
  environment_source?: string;
  provider_host?: string;
  provider_http_status?: number;
  provider_status_class?: string;
  status: "success" | "failed";
  failure_reason?: string;
  failure_category?: string;
  failure_code?: string;
  latency_ms: number;
  provider_latency_ms?: number;
  attempt_count: number;
  auth_scheme_names?: string[];
  auth_scheme_types?: string[];
  auth_scheme_count: number;
  auth_selection_outcome?: string;
  pagination_type?: string;
  pagination_page_count: number;
  pagination_item_count: number;
  pagination_byte_count: number;
  pagination_stop_reason?: string;
  rate_limit_decision?: string;
  rate_limit_policy_count: number;
  rate_limit_scope_kinds?: string[];
  rate_limit_units?: string[];
  // Totals stay strings because GraphQL Int cannot safely represent int64
  // counters without narrowing them to 32 bits.
  rate_limit_unit_totals?: string[];
  rate_limit_retry_outcome?: string;
  rate_limit_header_outcome?: string;
  request_bytes: number;
  response_bytes: number;
  verification_status?: string;
  delivery_status?: string;
  idempotency_replayed: boolean;
  started_at: string;
  ended_at: string;
  timings: Array<{ name: string; duration_ms: number }>;
}

const engineExecutionEventSelection = `
  id
  execution_kind
  parent_execution_id
  unified_target
  execution_phase
  unified_steps { target phase status error_code }
  trace_id
  span_id
  app_family_id
  app_id
  app_version
  app_kind
  transport
  provider_protocol
  direction
  service_id
  service_version_id
  service_name
  service_slug
  service_version
  operation_id
  webhook_id
  operation
  event_name
  http_method
  request_path
  environment
  environment_source
  provider_host
  provider_http_status
  provider_status_class
  status
  failure_reason
  failure_category
  failure_code
  latency_ms
  provider_latency_ms
  attempt_count
  auth_scheme_names
  auth_scheme_types
  auth_scheme_count
  auth_selection_outcome
  pagination_type
  pagination_page_count
  pagination_item_count
  pagination_byte_count
  pagination_stop_reason
  rate_limit_decision
  rate_limit_policy_count
  rate_limit_scope_kinds
  rate_limit_units
  rate_limit_unit_totals
  rate_limit_retry_outcome
  rate_limit_header_outcome
  request_bytes
  response_bytes
  verification_status
  delivery_status
  idempotency_replayed
  started_at
  ended_at
  timings { name duration_ms }
`;

export interface EngineExecutionAnalyticsSummary {
  total_calls: number;
  successful_calls: number;
  failed_calls: number;
  average_latency_ms: number;
  median_latency_ms: number;
  p95_latency_ms: number;
}

export interface EngineExecutionBreakdown {
  key: string;
  label: string;
  total_calls: number;
  failed_calls: number;
  inbound_calls: number;
  p95_latency_ms: number;
}

export interface AppExecutionAnalytics extends EngineExecutionAnalyticsSummary {
  by_service: EngineExecutionBreakdown[];
}

export interface WorkspaceExecutionAnalytics extends EngineExecutionAnalyticsSummary {
  inbound_calls: number;
  by_service: EngineExecutionBreakdown[];
  most_used_sdk?: EngineExecutionBreakdown | null;
  most_used_service?: EngineExecutionBreakdown | null;
  most_failed_service?: EngineExecutionBreakdown | null;
  most_used_bucket?: EngineExecutionBreakdown | null;
}

export interface PublicServiceInsights {
  source: "fused_engines_aggregate";
  generated_at: string;
  data_through?: string;
  partial_data: boolean;
  total_calls: number;
  successful_calls: number;
  failed_calls: number;
  p50_latency_ms: number;
  p95_latency_ms: number;
  time_series: PublicServiceInsightPoint[];
  top_operations: PublicServiceInsightPoint[];
  version_breakdown: PublicServiceInsightPoint[];
  transport_breakdown: PublicServiceInsightPoint[];
}

export interface PublicServiceInsightPoint {
  key: string;
  label: string;
  total_calls: number;
  failed_calls: number;
  p50_latency_ms: number;
  p95_latency_ms: number;
}

export interface ServiceConsumerEntry {
  id: string;
  name: string;
  version?: string;
  kind: "sdk" | "mcp";
  active: boolean;
  service_version_id: string;
  select_all: boolean;
  operation_count: number;
  webhook_count: number;
  created_at: string;
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
  auth_name?: string;
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
}

export interface WorkspaceConnectionProfile {
  service_id: string;
  service_version_id: string;
  auth_type: "oauth" | "oidc";
  registry_profile_id?: string;
  profile_revision: number;
  profile_hash: string;
  provenance: "workspace" | "provider" | "fused";
  source?: "workspace" | "provider" | "fused";
  has_workspace_override?: boolean;
  is_public: boolean;
  profile: Record<string, unknown>;
  bindings: WorkspaceConnectionBinding[];
}

const workspaceConnectionProfileSelection = `
  service_id
  service_version_id
  auth_type
  registry_profile_id
  profile_revision
  profile_hash
  provenance
  source
  has_workspace_override
  is_public
  profile
  bindings {
    id
    source_kind
    literal_value
    source_path
    target_location
    target_name
    operation_ids
    mode
    provenance
    source_profile_revision
  }
`;

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
  auth: {
    session: () => req<{ authenticated: boolean; subject_kind?: string }>("/auth/session"),
    startManaged: () => req<{
      transaction_id: string;
      poll_token: string;
      verification_url: string;
      expires_at: string;
    }>("/auth/managed/start", { method: "POST", body: "{}" }),
    pollManaged: (transactionId: string, pollToken: string, signal?: AbortSignal) =>
      req<{ status: "pending" | "authenticated"; expires_at?: string }>("/auth/managed/poll", {
        method: "POST",
        signal,
        body: JSON.stringify({ transaction_id: transactionId, poll_token: pollToken }),
      }),
    // Any active Engine API credential can establish a short-lived browser
    // session for the same subject; the source credential is never persisted
    // by the browser.
    exchangeAPIKey: (apiKey: string) =>
      req<{ status: "authenticated" }>("/auth/api-key/exchange", {
        method: "POST",
        body: JSON.stringify({ license_key: apiKey }),
      }),
    approveCLI: (transactionId: string, browserToken: string) =>
      req<void>("/auth/cli/approve", {
        method: "POST",
        body: JSON.stringify({ transaction_id: transactionId, token: browserToken }),
      }),
    logout: () => req<{ logout_url?: string }>("/auth/logout", { method: "POST", body: "{}" }),
  },

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

  connectBranding: {
    // get loads the Engine-owned appearance used by hosted connection pages.
    get: () => req<ConnectBranding>("/workspace/connect-branding"),
    // update sends only editable branding fields and lets Engine authorization enforce workspace.update.
    update: (input: ConnectBrandingInput) =>
      req<ConnectBranding>("/workspace/connect-branding", {
        method: "PUT",
        body: JSON.stringify(input),
      }),
  },

  credits: {
    getPricing: () => req<CreditsPricing>("/credits/pricing"),
  },

  // GraphQL freshness uses the shared credentialed transport without permitting a different method or body.
  graphql: <T>(query: string, variables?: Record<string, unknown>, options?: Pick<RequestInit, "headers" | "cache" | "signal">) =>
    req<GraphQLResponse<T>>(
      "/graphql",
      {
        ...options,
        method: "POST",
        body: JSON.stringify({ query, variables }),
      }
    ).then(unwrapGraphQLResponse),

  // mcpGraphql hits the Engine's own MCP GraphQL schema (list/deploy/kill/
  // reactivate/delete + analytics -- internal/engine/api/mcp_graphql.go),
  // a separate endpoint from api.graphql above, which is a pure Registry
  // forward-proxy with no MCP-aware resolvers of its own.
  mcpGraphql: <T>(query: string, variables?: Record<string, unknown>) =>
    req<GraphQLResponse<T>>("/engine/graphql", {
      method: "POST",
      body: JSON.stringify({ query, variables }),
    }).then(unwrapGraphQLResponse),

  appConfig: {
    // plan validates a versioned SDK or MCP config without changing active state.
    plan: <T>(kind: "sdk" | "mcp", input: {
      owner_team?: string;
      config_key: string;
      source_hash: string;
      config: Record<string, unknown>;
    }) => req<T>(`/${kind}-config/plan`, { method: "POST", body: JSON.stringify(input) }),
    // apply activates a previously validated immutable config plan.
    apply: <T>(kind: "sdk" | "mcp", input: { plan_id: string; source_hash: string }) =>
      req<T>(`/${kind}-config/apply`, { method: "POST", body: JSON.stringify(input) }),
  },

  integrations: {
    // Builder and uploaded-source reviews share Registry's authoritative parser and receipt.
    planImport: (input: {
      name: string;
      slug?: string;
      version?: string;
      source_url?: string;
      source_content?: string;
      target_type?: "endpoints" | "webhooks";
      destination_version?: string;
      expected_target?: SpecificationImportTarget;
      include_webhook_draft?: boolean;
    }) =>
      req<SpecificationImportPlan>("/integrations/import/plan", {
        method: "POST",
        body: JSON.stringify(input),
      }),

    // Only the reviewed source may cross the existing import mutation boundary.
    applyImport: (planId: string, reviewHash: string) =>
      req<SpecificationImportApplyResult>("/integrations/import/apply", {
        method: "POST",
        body: JSON.stringify({ plan_id: planId, review_hash: reviewHash }),
      }),

    // An interrupted apply is resolved from its durable ledger before offering a retry.
    importStatus: (operationID: string) =>
      req<SpecificationImportStatus>(`/integrations/import/operations/${encodeURIComponent(operationID)}`),

    // startDiscovery creates the authoritative revision-one session snapshot.
    startDiscovery: (input: DiscoveryStartRequest) =>
      req<DiscoverySnapshot>("/integrations/start", {
        method: "POST",
        body: JSON.stringify(input),
      }),

    // actOnDiscovery submits one strict, optimistic-concurrency-bound session decision.
    actOnDiscovery: (sessionId: string, input: DiscoveryActionRequest) =>
      req<DiscoverySnapshot>(`/integrations/session/${sessionId}/actions`, {
        method: "POST",
        body: JSON.stringify(input),
      }),

    // getActiveDiscoverySessions lists only resumable version-one snapshots.
    getActiveDiscoverySessions: () => req<DiscoverySnapshot[]>("/integrations/sessions/active"),

    // getDiscoverySession reloads the authoritative snapshot after reconnects and conflicts.
    getDiscoverySession: (sessionId: string) =>
      req<DiscoverySnapshot>(`/integrations/session/${sessionId}`),

    // getDiscoveryReviewSummary reads only the bounded structure bound to the exact public draft receipt.
    getDiscoveryReviewSummary: (sessionId: string, receipt: DiscoveryContractReceipt) => {
      const query = new URLSearchParams({
        draft_id: receipt.draft_id,
        draft_revision: String(receipt.draft_revision),
        review_hash: receipt.review_hash,
      });
      return req<DiscoveryReviewSummary>(`/integrations/session/${encodeURIComponent(sessionId)}/review-summary?${query.toString()}`);
    },

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
    download: async (id: string, name: string, version: string) => {
      const res = await fetch(`${BASE}/sdks/${id}/download`, {
        credentials: "include",
      });
      handleCredentialedResponse(res);
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
    deactivate: (appId: string) => req<void>(`/apps/${appId}/`, { method: "DELETE" }),
  },

  // The hosted Connect runtime API was removed with its obsolete browser
  // routes. Engine-owned app connection configuration is exposed separately.

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

    // getServices returns service membership plus every exact enabled version.
    getServices: () =>
      api
        .mcpGraphql<{ workspaceServices: ActivatedService[] }>(
          `query {
            workspaceServices {
              id
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

    // getServicesPage pages the same exact-version membership projection.
    getServicesPage: (limit: number, offset: number, names?: string[]) =>
      api
        .mcpGraphql<{ workspaceServicePage: { total: number; page: number; limit: number; data: ActivatedService[] } }>(
          `query WorkspaceServicePage($limit: Int, $offset: Int, $names: [String]) {
            workspaceServicePage(limit: $limit, offset: $offset, names: $names) {
              total page limit data {
                id
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
                environment
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

    // listEngineExecutionEvents pages service-wide execution receipts.
    listEngineExecutionEvents: (params: {
      serviceId: string;
      transport?: string;
      direction?: string;
      status?: string;
      limit?: number;
      offset?: number;
      startDate?: string;
      endDate?: string;
    }) =>
      api
        .mcpGraphql<{
          engineExecutionEvents: {
            items: EngineExecutionEventEntry[];
            total: number;
          };
        }>(
          `query EngineExecutionEvents($serviceId: String!, $transport: String, $direction: String, $status: String, $limit: Int, $offset: Int, $startDate: String, $endDate: String) {
            engineExecutionEvents(service_id: $serviceId, transport: $transport, direction: $direction, status: $status, limit: $limit, offset: $offset, start_date: $startDate, end_date: $endDate) {
              total
              items {
                ${engineExecutionEventSelection}
              }
            }
          }`,
          {
            serviceId: params.serviceId,
            transport: params.transport || null,
            direction: params.direction || null,
            status: params.status || null,
            limit: params.limit ?? null,
            offset: params.offset ?? null,
            startDate: params.startDate || null,
            endDate: params.endDate || null,
          }
        )
        .then(({ engineExecutionEvents }) => engineExecutionEvents),

    // listAppExecutionEvents scopes roots or a bounded child page to the same authorized app family.
    listAppExecutionEvents: (params: {
      appId: string;
      parentExecutionId?: string;
      includeAllVersions?: boolean;
      transport?: string;
      direction?: string;
      status?: string;
      limit?: number;
      offset?: number;
      startDate?: string;
      endDate?: string;
    }) =>
      api
        .mcpGraphql<{
          appExecutionEvents: {
            items: EngineExecutionEventEntry[];
            total: number;
          };
        }>(
          `query AppExecutionEvents($appId: String!, $parentExecutionId: String, $includeAllVersions: Boolean, $transport: String, $direction: String, $status: String, $limit: Int, $offset: Int, $startDate: String, $endDate: String) {
            appExecutionEvents(app_id: $appId, parent_execution_id: $parentExecutionId, include_all_versions: $includeAllVersions, transport: $transport, direction: $direction, status: $status, limit: $limit, offset: $offset, start_date: $startDate, end_date: $endDate) {
              total
              items {
                ${engineExecutionEventSelection}
              }
            }
          }`,
          {
            appId: params.appId,
            parentExecutionId: params.parentExecutionId ?? null,
            includeAllVersions: params.includeAllVersions ?? false,
            transport: params.transport || null,
            direction: params.direction || null,
            status: params.status || null,
            limit: params.limit ?? null,
            offset: params.offset ?? null,
            startDate: params.startDate || null,
            endDate: params.endDate || null,
          }
        )
        .then(({ appExecutionEvents }) => appExecutionEvents),

    // getAppExecutionAnalytics summarizes one app version or its whole family.
    getAppExecutionAnalytics: (params: {
      appId: string;
      includeAllVersions?: boolean;
      transport?: string;
      direction?: string;
      status?: string;
      startDate?: string;
      endDate?: string;
    }) =>
      api
        .mcpGraphql<{ appExecutionAnalytics: AppExecutionAnalytics }>(
          `query AppExecutionAnalytics($appId: String!, $includeAllVersions: Boolean, $transport: String, $direction: String, $status: String, $startDate: String, $endDate: String) {
            appExecutionAnalytics(app_id: $appId, include_all_versions: $includeAllVersions, transport: $transport, direction: $direction, status: $status, start_date: $startDate, end_date: $endDate) {
              total_calls
              successful_calls
              failed_calls
              average_latency_ms
              median_latency_ms
              p95_latency_ms
              by_service { key label total_calls failed_calls inbound_calls p95_latency_ms }
            }
          }`,
          {
            appId: params.appId,
            includeAllVersions: params.includeAllVersions ?? false,
            transport: params.transport || null,
            direction: params.direction || null,
            status: params.status || null,
            startDate: params.startDate || null,
            endDate: params.endDate || null,
          }
        )
        .then(({ appExecutionAnalytics }) => appExecutionAnalytics),

    // getEngineExecutionAnalytics summarizes service-wide execution receipts.
    getEngineExecutionAnalytics: (params: {
      serviceId: string;
      transport?: string;
      direction?: string;
      status?: string;
      startDate?: string;
      endDate?: string;
    }) =>
      api
        .mcpGraphql<{
          engineExecutionAnalytics: EngineExecutionAnalyticsSummary;
        }>(
          `query EngineExecutionAnalytics($serviceId: String!, $transport: String, $direction: String, $status: String, $startDate: String, $endDate: String) {
            engineExecutionAnalytics(service_id: $serviceId, transport: $transport, direction: $direction, status: $status, start_date: $startDate, end_date: $endDate) {
              total_calls
              successful_calls
              failed_calls
              average_latency_ms
              median_latency_ms
              p95_latency_ms
            }
          }`,
          {
            serviceId: params.serviceId,
            transport: params.transport || null,
            direction: params.direction || null,
            status: params.status || null,
            startDate: params.startDate || null,
            endDate: params.endDate || null,
          }
        )
        .then(({ engineExecutionAnalytics }) => engineExecutionAnalytics),

    // getWorkspaceExecutionAnalytics summarizes the actor's authorized workspace.
    getWorkspaceExecutionAnalytics: (
      params: { startDate?: string; endDate?: string } = {}
    ) =>
      api
        .mcpGraphql<{ workspaceExecutionAnalytics: WorkspaceExecutionAnalytics }>(
          `query WorkspaceExecutionAnalytics($startDate: String, $endDate: String) {
            workspaceExecutionAnalytics(start_date: $startDate, end_date: $endDate) {
              total_calls successful_calls failed_calls inbound_calls average_latency_ms median_latency_ms p95_latency_ms
              by_service { key label total_calls failed_calls inbound_calls p95_latency_ms }
              most_used_sdk { key label total_calls failed_calls inbound_calls p95_latency_ms }
              most_used_service { key label total_calls failed_calls inbound_calls p95_latency_ms }
              most_failed_service { key label total_calls failed_calls inbound_calls p95_latency_ms }
              most_used_bucket { key label total_calls failed_calls inbound_calls p95_latency_ms }
            }
          }`,
          { startDate: params.startDate || null, endDate: params.endDate || null }
        )
        .then(({ workspaceExecutionAnalytics }) => workspaceExecutionAnalytics),

    // getPublicServiceInsights forwards every current aggregate scope supported
    // by Engine so callers can choose exact-version or service-wide results.
    getPublicServiceInsights: (params: {
      serviceId: string;
      startDate: string;
      endDate: string;
      granularity?: "hour" | "day";
      serviceVersionId?: string;
      registryObjectKind?: "endpoint" | "webhook";
      registryObjectId?: string;
      transport?: "sdk" | "mcp" | "rest" | "webhook";
    }) =>
      api
        .mcpGraphql<{ publicServiceInsights: PublicServiceInsights }>(
          `query PublicServiceInsights($serviceId: String!, $startDate: String!, $endDate: String!, $granularity: String, $serviceVersionId: String, $registryObjectKind: String, $registryObjectId: String, $transport: String) {
            publicServiceInsights(service_id: $serviceId, start_date: $startDate, end_date: $endDate, granularity: $granularity, service_version_id: $serviceVersionId, registry_object_kind: $registryObjectKind, registry_object_id: $registryObjectId, transport: $transport) {
              source generated_at data_through partial_data total_calls successful_calls failed_calls p50_latency_ms p95_latency_ms
              time_series { key label total_calls failed_calls p50_latency_ms p95_latency_ms }
              top_operations { key label total_calls failed_calls p50_latency_ms p95_latency_ms }
              version_breakdown { key label total_calls failed_calls p50_latency_ms p95_latency_ms }
              transport_breakdown { key label total_calls failed_calls p50_latency_ms p95_latency_ms }
            }
          }`,
          {
            serviceId: params.serviceId,
            startDate: params.startDate,
            endDate: params.endDate,
            granularity: params.granularity || "day",
            serviceVersionId: params.serviceVersionId || null,
            registryObjectKind: params.registryObjectKind || null,
            registryObjectId: params.registryObjectId || null,
            transport: params.transport || null,
          }
        )
        .then(({ publicServiceInsights }) => publicServiceInsights),

    listServiceConsumers: (serviceId: string) =>
      api
        .mcpGraphql<{ serviceConsumers: ServiceConsumerEntry[] }>(
          `query ServiceConsumers($serviceId: String!) {
            serviceConsumers(service_id: $serviceId) {
              id
              name
              version
              kind
              active
              service_version_id
              select_all
              operation_count
              webhook_count
              created_at
            }
          }`,
          { serviceId }
        )
        .then(({ serviceConsumers }) => serviceConsumers),

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

    // getWorkspaceConnectionProfile reads the exact service-version/auth tuple.
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
              ${workspaceConnectionProfileSelection}
            }
          }`,
          input
        )
        .then(({ workspaceConnectionProfile }) => workspaceConnectionProfile),

    // setWorkspaceConnectionProfile validates and replaces one exact workspace
    // override without floating to another service version.
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
              ${workspaceConnectionProfileSelection}
            }
          }`,
          input
        )
        .then(({ setWorkspaceConnectionProfile }) => setWorkspaceConnectionProfile),

    // resetWorkspaceConnectionProfile removes only the tuple's workspace
    // override and leaves its Registry or Fused baseline intact.
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
              ${workspaceConnectionProfileSelection}
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
    // Activity Notifications tab (offset pagination, numbered
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

    // updateNotificationStatus changes only persisted Engine notifications -- "id"
    // must be a store.WorkspaceNotification's own id (source: "engine"),
    // never a "registry:"-prefixed live drift item's composite id; those
    // aren't notifications this mutation can act on (see the backend
    // resolver's own doc comment for why).
    updateNotificationStatus: (id: string, status: "acknowledged" | "dismissed") => {
      const rawId = mutableWorkspaceNotificationID(id);
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
