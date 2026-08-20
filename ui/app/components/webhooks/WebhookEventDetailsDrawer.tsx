import { useEffect, type ReactNode } from "react";
import { X } from "lucide-react";
import type { WebhookEventEntry } from "~/lib/api";

// WebhookEventDetailsDrawer exposes only the reduced webhook receipt fields
// already returned by Engine and never renders the inbound payload or secrets.
export function WebhookEventDetailsDrawer({ event, onClose }: { event: WebhookEventEntry; onClose: () => void }) {
  useEffect(() => {
    // Escape mirrors the overlay and header close actions for keyboard users.
    const closeOnEscape = (keyboardEvent: KeyboardEvent) => {
      if (keyboardEvent.key === "Escape") onClose();
    };
    window.addEventListener("keydown", closeOnEscape);
    return () => window.removeEventListener("keydown", closeOnEscape);
  }, [onClose]);

  return <>
    <button type="button" aria-label="Close webhook receipt details" className="fixed inset-0 z-40 cursor-default bg-slate-900/20" onClick={onClose} />
    <aside role="dialog" aria-modal="true" aria-labelledby="webhook-receipt-title" className="fixed inset-y-0 right-0 z-50 flex w-full flex-col overflow-y-auto overflow-x-hidden border-l border-slate-200 bg-white shadow-2xl md:w-[calc(100vw-4rem)] md:max-w-[940px] xl:max-w-[1080px]">
      <div className="sticky top-0 z-10 flex items-center justify-between gap-4 border-b border-slate-100 bg-white/90 px-5 py-4 backdrop-blur sm:px-6">
        <div className="min-w-0"><h2 id="webhook-receipt-title" className="text-lg font-semibold text-slate-900">Webhook receipt details</h2><p className="mt-0.5 truncate text-xs text-slate-500">{event.event_name || "Unnamed event"}</p></div>
        <button type="button" onClick={onClose} aria-label="Close webhook receipt details" className="rounded-full p-2 text-slate-400 transition-colors hover:bg-slate-100 hover:text-slate-600"><X className="h-5 w-5" /></button>
      </div>
      <div className="space-y-6 p-5 sm:p-6">
        <section className="rounded-lg border border-slate-200 bg-white p-5">
          <div className="grid gap-6 sm:grid-cols-2 lg:grid-cols-3">
            <WebhookDetail label="Event" value={event.event_name} />
            <WebhookDetail label="Verification" value={event.verification_status} />
            <WebhookDetail label="Delivery" value={event.delivery_status || event.status} />
            <WebhookDetail label="Environment" value={event.environment} />
            <WebhookDetail label="Delivery time" value={event.latency_ms > 0 ? `${event.latency_ms} ms` : ""} />
            <WebhookDetail label="Payload size" value={formatBytes(event.payload_size)} />
            <WebhookDetail label="Retries" value={event.retry_count.toLocaleString()} />
            <WebhookDetail label="Received" value={formatReceived(event.created_at)} />
          </div>
        </section>
        <section className="rounded-lg border border-slate-200 bg-slate-50 p-5"><h3 className="text-xs font-semibold uppercase tracking-wide text-slate-500">Correlation</h3><div className="mt-4 grid gap-5 sm:grid-cols-2"><WebhookDetail label="Message ID" value={event.msg_id} mono /><WebhookDetail label="Receipt ID" value={event.id} mono /><WebhookDetail label="SDK record ID" value={event.sdk_record_id} mono /></div></section>
      </div>
    </aside>
  </>;
}

// WebhookDetail applies one missing-value treatment across the reduced receipt projection.
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
