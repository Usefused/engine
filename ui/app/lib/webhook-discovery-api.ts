import { api, type ActivatedService } from "./api";
import { readAllBoundedPages } from "./bounded-pages";
import { webhookConfiguration, webhookServiceTag, type WebhookDraft, type WebhookListing, type WebhookRegistration } from "./webhook-discovery-contract";
import type { AppPlanResponse } from "./app-builder-contract";

export const WEBHOOK_REGISTRATIONS_QUERY = `query WebhookRegistrations($serviceId: String!) {
  workspaceWebhooks(service_id: $serviceId) { label slug callback_url delivery_mode signature signing_secret { bucket_id key_name } created_at }
}`;

/** Reads authorized services completely while keeping accidental request loops bounded. */
export async function webhookServices(): Promise<ActivatedService[]> {
  return readAllBoundedPages(async (limit, offset) => {
    const page = await api.workspace.getServicesPage(limit, offset);
    return { items: page.data, total: page.total };
  }, 100, 100);
}

/** Reuses the CLI's service registration query with bounded fan-out and explicit partial failures. */
export async function webhookListings(services: ActivatedService[]): Promise<{ items: WebhookListing[]; failed: string[] }> {
  const items: WebhookListing[] = [], failed: string[] = [];
  for (let offset = 0; offset < services.length; offset += 5) {
    const batch = services.slice(offset, offset + 5);
    const results = await Promise.allSettled(batch.map(async (service) => {
      const data = await api.mcpGraphql<{ workspaceWebhooks: WebhookRegistration[] }>(WEBHOOK_REGISTRATIONS_QUERY, { serviceId: service.service_id });
      // Keep Registry identity alongside the friendly name so tags and search refer to the same service.
      return data.workspaceWebhooks.map((registration) => ({ ...registration, service_id: service.service_id, service_name: service.service_name, service_ref: webhookServiceTag(service) }));
    }));
    results.forEach((result, index) => {
      // One inaccessible service must not hide readable URLs or masquerade as an empty catalogue.
      if (result.status === "fulfilled") items.push(...result.value);
      else failed.push(batch[index].service_name);
    });
  }
  return { items, failed };
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
