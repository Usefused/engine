import { useFusedAgent } from "~/components/agent/FusedAgentContext";
import { useEffect, type ReactNode } from "react";
import { X } from "lucide-react";
import type { WebhookEventEntry } from "~/lib/api";
import { WebhookMessageDiagnostics } from "./WebhookMessageDiagnostics";
import { OperationDisclosure } from "~/components/OperationDetailsParts";

// WebhookEventDetailsDrawer keeps diagnostic identifiers collapsed while showing receipt outcomes and the private message inspector.
export function WebhookEventDetailsDrawer({ event, onClose }: { event: WebhookEventEntry; onClose: () => void }) {
  const agent = useFusedAgent();
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
    {/* An open assistant remains an accessible sibling of this workspace-scoped drawer. */}
    <aside data-fused-detail-sidebar role="dialog" aria-modal={!agent?.isOpen} aria-labelledby="webhook-receipt-title" className="fixed inset-y-0 right-0 z-50 flex w-full flex-col overflow-y-auto overflow-x-hidden border-l border-slate-200 bg-white shadow-2xl md:w-[calc(100%-4rem)] md:max-w-[940px] xl:max-w-[1080px]">
      <div className="sticky top-0 z-10 flex items-center justify-between gap-4 border-b border-slate-100 bg-white/90 px-3 py-4 backdrop-blur sm:px-6">
        <div className="min-w-0"><h2 id="webhook-receipt-title" className="text-lg font-semibold text-slate-900">Webhook receipt details</h2><p className="mt-0.5 truncate text-xs text-slate-500">{event.event_name || "Unnamed event"}</p></div>
        <button type="button" onClick={onClose} aria-label="Close webhook receipt details" className="rounded-full p-2 text-slate-400 transition-colors hover:bg-slate-100 hover:text-slate-600"><X className="h-5 w-5" /></button>
      </div>
      {/* Compact phone gutters prevent the drawer and its cards from consuming reading width twice. */}
      <div className="space-y-6 px-3 py-5 sm:p-6">
        <section className="rounded-lg border border-slate-200 bg-white px-3 py-5 sm:px-5">
          <div className="grid gap-6 sm:grid-cols-2 lg:grid-cols-3">
            <WebhookDetail label="Event" value={event.event_name} />
            <WebhookDetail label="Execution status" value={event.execution_status} />
            <WebhookDetail label="Verification" value={event.verification_status} />
            <WebhookDetail label="Delivery" value={event.delivery_status || event.status} />
            <WebhookDetail label="Environment" value={event.environment} />
            <WebhookDetail label="Delivery time" value={event.latency_ms > 0 ? `${event.latency_ms} ms` : ""} />
            <WebhookDetail label="Payload size" value={formatBytes(event.payload_size)} />
            <WebhookDetail label="Retries" value={event.retry_count.toLocaleString()} />
            <WebhookDetail label="Attempts" value={event.attempt_count?.toLocaleString()} />
            <WebhookDetail label="Received" value={formatReceived(event.created_at)} />
            <WebhookDetail label="Completed" value={formatReceived(event.ended_at || "")} />
            <WebhookDetail label="Recorded" value={formatReceived(event.recorded_at || "")} />
          </div>
        </section>
        {/* Verification and delivery failures stay inspectable through the shared receipt drawer. */}
        {event.error_reason && <section className="rounded-lg border border-red-200 bg-red-50 px-3 py-5 sm:px-5"><h3 className="text-xs font-semibold uppercase tracking-wide text-red-800">Failure reason</h3><p className="mt-3 whitespace-pre-wrap break-words text-sm text-red-800">{event.error_reason}</p><div className="mt-4 grid gap-4 sm:grid-cols-2"><WebhookDetail label="Failure category" value={event.failure_category} /><WebhookDetail label="Failure code" value={event.failure_code} mono /></div></section>}
        {/* Troubleshooting identities stay available in the shared accordion; a different receipt starts collapsed. */}
        <OperationDisclosure key={event.id} label="Correlation"><div className="grid gap-5 px-3 py-4 sm:grid-cols-2 sm:px-4"><WebhookDetail label="Message ID" value={event.msg_id} mono /><WebhookDetail label="Receipt ID" value={event.id} mono /><WebhookDetail label="Registration ID" value={event.registration_id} mono /><WebhookDetail label="Service ID" value={event.service_id} mono /><WebhookDetail label="Service version ID" value={event.service_version_id} mono /><WebhookDetail label="App ID" value={event.sdk_record_id} mono /><WebhookDetail label="Trace ID" value={event.trace_id} mono /><WebhookDetail label="Span ID" value={event.span_id} mono /></div></OperationDisclosure>
        <WebhookMessageDiagnostics event={event} />
      </div>
    </aside>
  </>;
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
