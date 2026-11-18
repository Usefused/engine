import type { UnifiedDraft } from "./unified-app-contract.ts";
import type { WebhookCatalogueItem } from "./webhook-discovery-contract.ts";

export interface WebhookFocusService {
  serviceId: string;
  serviceName?: string;
}

export interface WebhookAttachmentCoverage {
  attachment: string;
  serviceIds: Set<string>;
  serviceNames: string[];
  services: WebhookFocusService[];
}

/** Builds a deduplicated per-service coverage map for one exact webhook attachment label. */
export function buildWebhookAttachmentCoverage(
  attachment: string,
  items: Array<Pick<WebhookCatalogueItem, "label" | "service_id" | "service_name">>,
): WebhookAttachmentCoverage {
  const trimmed = attachment.trim();
  const serviceIds = new Set<string>();
  const serviceNames: string[] = [];
  const services: WebhookFocusService[] = [];
  for (const item of items) {
    // Substring search can return sibling labels; only exact bundle labels cover this attachment.
    if (item.label !== trimmed || !item.service_id || serviceIds.has(item.service_id)) continue;
    serviceIds.add(item.service_id);
    // Prefer the human service name when available, falling back to the raw identity only if missing.
    const label = item.service_name?.trim() || item.service_id;
    serviceNames.push(label);
    services.push({ serviceId: item.service_id, serviceName: item.service_name?.trim() || undefined });
  }
  return { attachment: trimmed, serviceIds, serviceNames, services };
}

/** Checks whether a service pin requests inbound webhook events explicitly or via select-all. */
function serviceSelectsWebhookEvents(draft: UnifiedDraft, key: string, pin: UnifiedDraft["services"][string]): boolean {
  const rawService = draft.serviceSettings?.[key]?.config;
  // Both explicit event arrays and select-all flags require registration under the attached webhook.
  return Boolean(pin.webhooks?.length) || rawService?.webhooks_select_all === true;
}

/** Identifies event-selecting services that are verifiably absent from the attached webhook registration. */
export function uncoveredWebhookEventServices(
  draft: UnifiedDraft,
  coverage: WebhookAttachmentCoverage | null,
  canReadService: (serviceId: string) => boolean,
): string[] {
  const attachment = draft.webhookAttachment?.trim() ?? "";
  // Stale or absent coverage snapshots must never block or misdiagnose an unrelated attachment.
  if (!attachment || !coverage || coverage.attachment !== attachment) return [];
  return Object.entries(draft.services)
    .filter(([key, pin]) =>
      // Operation-only services and unreadable services are skipped so Engine remains authoritative for hidden grants.
      serviceSelectsWebhookEvents(draft, key, pin) && canReadService(pin.service_id) && !coverage.serviceIds.has(pin.service_id),
    )
    .map(([key]) => key);
}

/** Explains how to resolve a mismatch between selected event services and the attached webhook bundle. */
export function webhookCoverageMismatchMessage(
  attachment: string,
  uncoveredServices: string[],
  coveredServiceNames: string[],
): string {
  // No message is needed once every event-selecting service is covered by the attachment.
  if (!attachment.trim() || uncoveredServices.length === 0) return "";
  const missing = uncoveredServices.join(", ");
  // Naming the services this registration actually covers guides users directly to the right event tab or webhook.
  if (coveredServiceNames.length > 0) {
    const covered = coveredServiceNames.join(", ");
    return `Webhook "${attachment.trim()}" registers ${covered}, not ${missing}. Select events under ${covered}, or choose or create a webhook registration for ${missing}.`;
  }
  return `Webhook "${attachment.trim()}" does not register ${missing}. Choose an existing webhook or create a registration for ${missing}.`;
}

/** Explains inside a service's Events tab when the currently attached webhook does not register that service. */
export function serviceWebhookCoverageNotice(
  serviceId: string,
  serviceName: string,
  coverage: WebhookAttachmentCoverage | null,
  canReadService: boolean,
): string {
  // Covered services, unreadable services, and drafts without an attachment need no inline mismatch notice.
  if (!coverage || !coverage.attachment || !canReadService || coverage.serviceIds.has(serviceId)) return "";
  // Mentioning the attached webhook's registered service(s) prevents confusing one provider's webhook for another's.
  if (coverage.serviceNames.length > 0) {
    return `Attached webhook "${coverage.attachment}" registers ${coverage.serviceNames.join(", ")}, not ${serviceName}. Choose or create a webhook registration for ${serviceName} to deploy these events.`;
  }
  return `Attached webhook "${coverage.attachment}" does not register ${serviceName}. Choose or create a webhook registration for ${serviceName} to deploy these events.`;
}

/** Combines an explicitly chosen webhook service with all services covered by the attached registration without duplicates. */
export function mergeWebhookFocusServices(
  explicit: WebhookFocusService | null | undefined,
  coverage: WebhookAttachmentCoverage | null,
): WebhookFocusService[] {
  const merged: WebhookFocusService[] = [];
  const seen = new Set<string>();
  // Keep the explicitly clicked registration's service first so its Events tab surfaces immediately.
  if (explicit?.serviceId) {
    seen.add(explicit.serviceId);
    merged.push(explicit);
  }
  // Include all other services registered under the same attachment bundle for multi-service registrations.
  if (coverage) {
    for (const service of coverage.services) {
      if (!service.serviceId || seen.has(service.serviceId)) continue;
      seen.add(service.serviceId);
      merged.push(service);
    }
  }
  return merged;
}
