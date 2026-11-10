import { api, type WebhookAnalyticsSummary, type WebhookEventEntry } from "./api";
import type { WebhookListing } from "./webhook-discovery-contract";
import { webhookDateBound, type WebhookEventStatus } from "./webhook-detail";

export interface RegistrationAnalytics extends WebhookAnalyticsSummary {
  average_latency_ms: number;
  p95_latency_ms: number;
}
export interface RegistrationEventPage { items: WebhookEventEntry[]; total: number }
export interface WebhookActivityInput {
  serviceId: string;
  registrationId: string;
  eventName?: string;
  status?: WebhookEventStatus;
  startDate?: string;
  endDate?: string;
  limit?: number;
  offset?: number;
}

export const WEBHOOK_DETAIL_QUERY = `query WebhookRegistration($serviceId: String!, $slug: String!) {
  workspaceWebhook(service_id: $serviceId, slug: $slug) {
    id service_id service_name service_ref label slug callback_url delivery_mode signature created_at
    signing_secret { bucket_id key_name }
  }
}`;
export const WEBHOOK_ANALYTICS_QUERY = `query WebhookRegistrationAnalytics($serviceId: String!, $registrationId: String!, $startDate: String, $endDate: String) {
  webhookAnalytics(service_id: $serviceId, registration_id: $registrationId, start_date: $startDate, end_date: $endDate) {
    total_ingested total_delivered total_rejected total_failed average_latency_ms p95_latency_ms
  }
}`;
export const WEBHOOK_EVENTS_QUERY = `query WebhookRegistrationEvents($serviceId: String!, $registrationId: String!, $eventName: String, $status: String, $startDate: String, $endDate: String, $limit: Int, $offset: Int) {
  webhookEvents(service_id: $serviceId, registration_id: $registrationId, event_name: $eventName, status: $status, start_date: $startDate, end_date: $endDate, limit: $limit, offset: $offset) {
    total
    items { registration_id service_version_id trace_id span_id execution_status failure_category failure_code attempt_count ended_at recorded_at id account_id service_id msg_id event_name status delivery_status verification_status environment latency_ms retry_count credits_consumed sdk_record_id error_reason payload_size created_at }
  }
}`;

/** Every detail request is cancellable on navigation and bounded when the Engine is unavailable. */
function requestSignal(signal?: AbortSignal): AbortSignal {
  const deadline = AbortSignal.timeout(15_000);
  // A caller abort wins over the deadline without keeping a stale route request alive.
  return signal ? AbortSignal.any([signal, deadline]) : deadline;
}

/** Reads one exact route without searching registration labels or walking discovery pages. */
export async function webhookDetails(serviceId: string, slug: string, signal?: AbortSignal): Promise<WebhookListing | null> {
  const data = await api.mcpGraphql<{ workspaceWebhook: WebhookListing | null }>(WEBHOOK_DETAIL_QUERY, { serviceId, slug }, { signal: requestSignal(signal) });
  return data.workspaceWebhook;
}

/** Validates exact identity before any reporting request, so missing IDs cannot broaden the scope. */
function activityVariables(input: WebhookActivityInput) {
  // An absent exact registration is a programming/input error, never permission to show service-wide data.
  if (!input.registrationId || !input.serviceId) throw new Error("An exact webhook registration is required.");
  return { serviceId: input.serviceId, registrationId: input.registrationId, startDate: webhookDateBound(input.startDate || ""), endDate: webhookDateBound(input.endDate || "", true) };
}

/** Loads all outcome metrics for this registration and period independently of receipt pagination. */
export async function webhookRegistrationAnalytics(input: WebhookActivityInput, signal?: AbortSignal): Promise<RegistrationAnalytics> {
  const data = await api.mcpGraphql<{ webhookAnalytics: RegistrationAnalytics }>(WEBHOOK_ANALYTICS_QUERY, activityVariables(input), { signal: requestSignal(signal) });
  return data.webhookAnalytics;
}

/** Sends filters and page bounds to SQL rather than filtering a mixed-registration page in the browser. */
export async function webhookRegistrationEvents(input: WebhookActivityInput, signal?: AbortSignal): Promise<RegistrationEventPage> {
  const variables = { ...activityVariables(input), eventName: input.eventName || null, status: input.status || null, limit: input.limit ?? 20, offset: input.offset ?? 0 };
  const data = await api.mcpGraphql<{ webhookEvents: RegistrationEventPage }>(WEBHOOK_EVENTS_QUERY, variables, { signal: requestSignal(signal) });
  return data.webhookEvents;
}
