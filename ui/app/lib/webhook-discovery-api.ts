import { api, type ActivatedService } from "./api";
import { readAllBoundedPages } from "./bounded-pages";
import { webhookConfiguration, type WebhookDraft, type WebhookListing } from "./webhook-discovery-contract";
import type { AppPlanResponse } from "./app-builder-contract";

export const WEBHOOK_REGISTRATIONS_QUERY = `query WorkspaceWebhookPage($limit: Int, $offset: Int, $serviceId: String, $search: String) {
  workspaceWebhookPage(limit: $limit, offset: $offset, service_id: $serviceId, search: $search) {
    total
    items { service_id service_name service_ref label slug callback_url delivery_mode signature signing_secret { bucket_id key_name } created_at }
  }
}`;

/** Reads authorized services completely while keeping accidental request loops bounded. */
export async function webhookServices(): Promise<ActivatedService[]> {
  return readAllBoundedPages(async (limit, offset) => {
    const page = await api.workspace.getServicesPage(limit, offset);
    return { items: page.data, total: page.total };
  }, 100, 100);
}

export interface WebhookPage { items: WebhookListing[]; total: number }

/** Fetches exactly one filtered registration page; neither catalogue discovery nor per-service requests are needed. */
export async function webhookListings(filters: { limit: number; offset: number; serviceId?: string; search?: string }, signal?: AbortSignal): Promise<WebhookPage> {
  const deadline = AbortSignal.timeout(15_000);
  // Navigation cancels the underlying fetch while the deadline also bounds an unresponsive Engine.
  const requestSignal = signal ? AbortSignal.any([signal, deadline]) : deadline;
  try {
    requestSignal.throwIfAborted();
    const data = await api.mcpGraphql<{ workspaceWebhookPage: WebhookPage }>(
      WEBHOOK_REGISTRATIONS_QUERY, filters, { signal: requestSignal }
    );
    return data.workspaceWebhookPage;
  } catch (error) {
    // Navigation aborts stay silent at the route; timeouts provide an actionable retry state.
    if (deadline.aborted) throw new Error("Loading webhook URLs timed out. Refresh to try again.");
    throw error;
  }
}

/** Creates only a review receipt; provider ingress changes solely after the user presses Create webhook. */
export async function planWebhook(draft: WebhookDraft, ownerTeam: string): Promise<AppPlanResponse> {
  const config = webhookConfiguration(draft);
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(JSON.stringify(config)));
  const source_hash = `sha256:${Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("")}`;
  const plan = await api.appConfig.plan<AppPlanResponse>("webhook", { config_key: `webhook:${config.name}`, config, source_hash, owner_team: ownerTeam });
  // This creation flow must never replace an existing bundle and prune its other registered services.
  if (plan.summary.create_webhook !== true) throw new Error("That webhook name already exists. Choose a different name to create a new receiving URL.");
  return plan;
}

/** Applies exactly the reviewed hash, without silently retrying a possibly committed provisioning request. */
export async function createWebhook(plan: AppPlanResponse): Promise<void> {
  await api.appConfig.apply("webhook", { plan_id: plan.plan_id, source_hash: plan.source_hash });
}
