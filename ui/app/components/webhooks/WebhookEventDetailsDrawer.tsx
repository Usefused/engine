import { useFusedAgent } from "~/components/agent/FusedAgentContext";
import { useEffect, useLayoutEffect, useRef, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { X } from "lucide-react";
import type { WebhookConsumptionEntry, WebhookEventEntry } from "~/lib/api";
import { WebhookMessageDiagnostics } from "./WebhookMessageDiagnostics";
import { OperationDisclosure } from "~/components/OperationDetailsParts";
import { workspaceDialogHost } from "~/lib/workspace-dialog-host";
import { formatDuration } from "~/lib/format";

/** Shows the recorded service, consumption history, and receipt outcomes while keeping correlation and private messages behind disclosures. */
export function WebhookEventDetailsDrawer({ event, serviceName, onClose }: { event: WebhookEventEntry; serviceName?: string; onClose: () => void }) {
  const agent = useFusedAgent();
  const titleRef = useRef<HTMLHeadingElement>(null);
  const consumptions = event.consumptions ?? [];
  const deliveryStatus = event.delivery_status || event.status;

  // Restore focus to the invoking receipt row on dismissal without scrolling the underlying workspace.
  useLayoutEffect(() => {
    const trigger = document.activeElement;
    return () => {
      // A filtered or paged-out row cannot receive focus after dismissal.
      if (trigger instanceof HTMLElement && trigger.isConnected) trigger.focus({ preventScroll: true });
    };
  }, []);

  // Move keyboard focus into the open receipt heading without triggering document scroll jumps.
  useLayoutEffect(() => {
    titleRef.current?.focus({ preventScroll: true });
  }, [event.id]);

  useEffect(() => {
    // Escape mirrors the overlay and header close actions for keyboard users.
    const closeOnEscape = (keyboardEvent: KeyboardEvent) => {
      if (keyboardEvent.key === "Escape") onClose();
    };
    window.addEventListener("keydown", closeOnEscape);
    return () => window.removeEventListener("keydown", closeOnEscape);
  }, [onClose]);

  const drawer = <>
    <button type="button" aria-label="Close webhook receipt details" className="fixed inset-0 z-40 cursor-default bg-slate-900/20" onClick={onClose} />
    {/* An open assistant remains an accessible sibling of this workspace-scoped drawer. */}
    <aside data-fused-detail-sidebar role="dialog" aria-modal={!agent?.isOpen} aria-labelledby="webhook-receipt-title" className="receipt-drawer-enter fixed inset-y-0 right-0 z-50 flex h-dvh max-h-dvh w-full max-w-full flex-col overflow-hidden border-l border-slate-200 bg-white shadow-2xl md:w-[calc(100%-4rem)] md:max-w-[940px] xl:max-w-[1080px]">
      <div className="z-10 flex shrink-0 items-center justify-between gap-4 border-b border-slate-100 bg-white/90 px-4 py-4 backdrop-blur sm:px-6">
        <div className="min-w-0"><h2 ref={titleRef} tabIndex={-1} id="webhook-receipt-title" className="text-lg font-semibold text-slate-900 outline-none">Webhook receipt details</h2><p className="mt-0.5 truncate text-xs text-slate-500">{event.event_name || "Unnamed event"}</p></div>
        <button type="button" onClick={onClose} aria-label="Close webhook receipt details" className="rounded-full p-2 text-slate-400 transition-colors hover:bg-slate-100 hover:text-slate-600"><X className="h-5 w-5" /></button>
      </div>
      {/* Confine drawer scrolling inside the viewport-bounded shell so wheel gestures never shift the outer page. */}
      <div className="min-h-0 flex-1 space-y-6 overflow-y-auto overflow-x-hidden overscroll-contain px-3 py-5 [overflow-wrap:anywhere] sm:p-6">
        <WebhookSummaryCard event={event} serviceName={serviceName} deliveryStatus={deliveryStatus} consumptions={consumptions} />
        <WebhookConsumptionSection consumptions={consumptions} deliveryStatus={deliveryStatus} verificationStatus={event.verification_status} />
        {/* Verification and delivery failures stay inspectable through the shared receipt drawer. */}
        {event.error_reason && <section className="rounded-lg border border-red-200 bg-red-50 px-3 py-5 sm:px-5"><h3 className="text-xs font-semibold uppercase tracking-wide text-red-800">Failure reason</h3><p className="mt-3 whitespace-pre-wrap break-words text-sm text-red-800">{event.error_reason}</p><div className="mt-4 grid gap-4 sm:grid-cols-2"><WebhookDetail label="Failure category" value={event.failure_category} /><WebhookDetail label="Failure code" value={event.failure_code} mono /></div></section>}
        {/* Troubleshooting identities stay available in the shared accordion; a different receipt starts collapsed. */}
        <OperationDisclosure key={event.id} label="Correlation"><div className="grid gap-5 px-3 py-4 sm:grid-cols-2 sm:px-4"><WebhookDetail label="Message ID" value={event.msg_id} mono /><WebhookDetail label="Receipt ID" value={event.id} mono /><WebhookDetail label="Registration ID" value={event.registration_id} mono /><WebhookDetail label="Service ID" value={event.service_id} mono /><WebhookDetail label="Service version ID" value={event.service_version_id} mono /><WebhookDetail label="App ID" value={event.sdk_record_id} mono /><WebhookDetail label="Trace ID" value={event.trace_id} mono /><WebhookDetail label="Span ID" value={event.span_id} mono /></div></OperationDisclosure>
        <WebhookMessageDiagnostics event={event} />
      </div>
    </aside>
  </>;

  // Portal outside ancestor container-query and scroll wrappers while staying inside the workspace host.
  return typeof document !== "undefined" ? createPortal(drawer, workspaceDialogHost()) : drawer;
}

/** Projects canonical receipt delivery, timing, and consumption summary metadata into the top card. */
function WebhookSummaryCard({ event, serviceName, deliveryStatus, consumptions }: { event: WebhookEventEntry; serviceName?: string; deliveryStatus: string; consumptions: WebhookConsumptionEntry[] }) {
  return (
    <section className="rounded-lg border border-slate-200 bg-white px-3 py-5 sm:px-5">
      <div className="grid gap-6 sm:grid-cols-2 lg:grid-cols-3">
        <WebhookDetail label="Event" value={event.event_name} />
        <WebhookDetail label="Execution status" value={event.execution_status} />
        <WebhookDetail label="Verification" value={event.verification_status} />
        <WebhookDetail label="Delivery" value={deliveryStatus} />
        <WebhookDetail label="Service" value={serviceName} />
        <WebhookDetail label="Consumed by" value={formatConsumedBySummary(consumptions, deliveryStatus)} />
        <WebhookDetail label="Consumed at" value={formatConsumedAtSummary(consumptions, deliveryStatus, event.ended_at)} />
        {/* Convert recorded delivery latency into readable units while treating zero or missing values as unrecorded. */}
        <WebhookDetail label="Delivery time" value={event.latency_ms > 0 ? formatDuration(event.latency_ms) : ""} />
        <WebhookDetail label="Payload size" value={formatBytes(event.payload_size)} />
        <WebhookDetail label="Retries" value={event.retry_count.toLocaleString()} />
        <WebhookDetail label="Attempts" value={event.attempt_count?.toLocaleString()} />
        <WebhookDetail label="Received" value={formatReceived(event.created_at)} />
        <WebhookDetail label="Completed" value={formatReceived(event.ended_at || "")} />
        <WebhookDetail label="Recorded" value={formatReceived(event.recorded_at || "")} />
      </div>
    </section>
  );
}

/** Displays every consumer that received this webhook and its exact consumption timestamp. */
function WebhookConsumptionSection({ consumptions, deliveryStatus, verificationStatus }: { consumptions: WebhookConsumptionEntry[]; deliveryStatus: string; verificationStatus: string }) {
  return (
    <section aria-label="Webhook consumption" className="rounded-lg border border-slate-200 bg-white px-3 py-4 sm:px-5">
      <div className="flex items-center justify-between gap-3">
        <h3 className="text-sm font-semibold text-slate-800">Consumption</h3>
        {/* Show the total consumer count once at least one consumer has received the event. */}
        {consumptions.length > 0 && <span className="rounded-full bg-slate-100 px-2 py-0.5 text-xs font-medium text-slate-600">{consumptions.length}</span>}
      </div>
      {/* Distinguish active consumer receipts from unconsumed, rejected, and legacy pre-tracking deliveries. */}
      {consumptions.length > 0 ? (
        <ul className="mt-3 divide-y divide-slate-100">
          {consumptions.map((entry, index) => (
            <li key={`${entry.consumer_kind}:${entry.app_id || entry.app_family_id || entry.consumer_name || index}:${entry.receiver_name || ""}`} className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 py-3 first:pt-1 last:pb-0">
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-2">
                  <span className={`inline-flex rounded-md px-2 py-0.5 text-xs font-medium ${consumerKindBadge(entry.consumer_kind)}`}>{formatConsumerKind(entry.consumer_kind)}</span>
                  <span className="min-w-0 break-words text-sm font-medium text-slate-900">{consumerDisplayName(entry)}</span>
                  {/* Show immutable version when recorded so operators can verify which deployment handled the event. */}
                  {entry.app_version && <span className="rounded bg-slate-100 px-1.5 py-0.5 font-mono text-[11px] text-slate-600">v{entry.app_version}</span>}
                </div>
                {/* SDK subscriptions can distinguish multiple named receivers under the same app version. */}
                {entry.receiver_name && <p className="mt-1 text-xs text-slate-500">Receiver: <span className="font-mono">{entry.receiver_name}</span></p>}
              </div>
              <time dateTime={entry.consumed_at} className="shrink-0 text-xs text-slate-500">{formatReceived(entry.consumed_at) || "Time not recorded"}</time>
            </li>
          ))}
        </ul>
      ) : (
        <p className="mt-2 text-sm text-slate-500">{emptyConsumptionMessage(deliveryStatus, verificationStatus)}</p>
      )}
    </section>
  );
}

/** Maps canonical consumer transport identifiers to user-facing labels. */
function formatConsumerKind(kind: string): string {
  // Normalize backend enum values to the product's exact app terminology.
  switch (kind) {
    case "sdk": return "SDK";
    case "mcp": return "MCP";
    case "unified_app": return "Unified App";
    case "relay": return "Relay";
    default: return kind || "Consumer";
  }
}

/** Applies consistent badge styling per consumer transport so mixed consumption lists scan quickly. */
function consumerKindBadge(kind: string): string {
  // Separate SDK, MCP, Unified App, and relay transports visually without implying error severity.
  switch (kind) {
    case "sdk": return "bg-blue-50 text-blue-700";
    case "mcp": return "bg-violet-50 text-violet-700";
    case "unified_app": return "bg-emerald-50 text-emerald-700";
    case "relay": return "bg-amber-50 text-amber-700";
    default: return "bg-slate-100 text-slate-700";
  }
}

/** Picks the most readable recorded name for a consumer row while falling back cleanly to stable IDs. */
function consumerDisplayName(entry: WebhookConsumptionEntry): string {
  // Prefer the app family display name, then receiver name or recorded UUIDs if the app was removed.
  return entry.consumer_name || entry.receiver_name || entry.app_id || entry.app_family_id || formatConsumerKind(entry.consumer_kind);
}

/** Recognizes legacy delivery outcomes recorded before per-consumer tracking existed. */
function isDeliveredStatus(status: string): boolean {
  return status === "success" || status === "delivered" || status === "acked";
}

/** Summarizes who consumed the webhook for the top metadata grid. */
function formatConsumedBySummary(consumptions: WebhookConsumptionEntry[], deliveryStatus: string): string {
  // Empty consumption lists distinguish unconsumed events from historical deliveries lacking consumer rows.
  if (consumptions.length === 0) {
    return isDeliveredStatus(deliveryStatus) ? "Consumer not recorded" : "No one";
  }
  // A single consumer fits directly in the summary cell with its transport and display name.
  if (consumptions.length === 1) {
    const only = consumptions[0];
    const kind = formatConsumerKind(only.consumer_kind);
    return only.consumer_name ? `${kind} · ${only.consumer_name}` : kind;
  }
  const kinds = Array.from(new Set(consumptions.map((entry) => formatConsumerKind(entry.consumer_kind))));
  return `${consumptions.length} consumers (${kinds.join(", ")})`;
}

/** Summarizes when the webhook was consumed for the top metadata grid. */
function formatConsumedAtSummary(consumptions: WebhookConsumptionEntry[], deliveryStatus: string, endedAt?: string): string {
  // Report the latest recorded consumption timestamp when one or more consumers received the webhook.
  if (consumptions.length > 0) {
    return formatReceived(consumptions[consumptions.length - 1].consumed_at) || "Not recorded";
  }
  // Historical delivered receipts retain their completion clock even without per-consumer rows.
  if (isDeliveredStatus(deliveryStatus)) {
    return formatReceived(endedAt || "") || "Not recorded";
  }
  return "Not consumed";
}

/** Explains why a receipt has no consumer entries without conflating rejection, unconsumed ingress, and legacy history. */
function emptyConsumptionMessage(deliveryStatus: string, verificationStatus: string): string {
  // Rejected requests never enter the durable broker stream, so no consumer can receive them.
  if (verificationStatus === "rejected" || deliveryStatus === "rejected") {
    return "Not consumed — this webhook was rejected during verification.";
  }
  // Preserve accuracy for historical receipts acknowledged prior to per-consumer tracking.
  if (isDeliveredStatus(deliveryStatus)) {
    return "Consumed, but consumer details were not recorded for this historical receipt.";
  }
  return "No one has consumed this webhook yet.";
}

// WebhookDetail applies one missing-value treatment across the safe canonical metadata projection.
function WebhookDetail({ label, value, mono = false }: { label: string; value?: ReactNode; mono?: boolean }) {
  return <div className="min-w-0"><div className="text-xs font-medium text-slate-500">{label}</div><div className={`mt-1 break-words text-sm text-slate-900 ${mono ? "font-mono text-xs" : ""}`}>{value || "Not recorded"}</div></div>;
}

// formatBytes keeps payload metadata readable without exposing payload content.
function formatBytes(value: number) {
  if (!Number.isFinite(value) || value < 0) return "";
  if (value < 1024) return `${value} B`;
  return `${(value / 1024).toFixed(1)} KB`;
}

// formatReceived converts the canonical timestamp for the current browser locale.
function formatReceived(value: string) {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "" : date.toLocaleString();
}
