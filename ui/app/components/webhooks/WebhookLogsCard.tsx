import { Select } from "../forms/Select.ts";
import { useState } from "react";
import { RefreshCw, ChevronLeft, ChevronRight } from "lucide-react";
import { WebhookEventDetailsDrawer } from "~/components/webhooks/WebhookEventDetailsDrawer";
import { type WebhookEventEntry, type WebhookAnalyticsSummary } from "~/lib/api";

// Switch columns based on the available panel width, including when the workspace sidebar is open.
const receiptColumns = "@2xl:grid-cols-[minmax(0,2fr)_minmax(0,1.25fr)_minmax(0,1fr)_minmax(0,1fr)_1rem] @2xl:gap-x-4";

interface ReceiptService {
  id: string;
  name: string;
}

/** Matches the displayed service to the receipt's recorded identity. */
function receiptServiceName(event: WebhookEventEntry, service: ReceiptService) {
  // Older or unmatched receipts can still show their recorded ID without borrowing another service's name.
  return event.service_id === service.id && service.name ? service.name : event.service_id;
}

/** Colors delivery outcomes consistently across the receipt list. */
function deliveryColor(status: string): string {
  // Acknowledgement, successful delivery, and failure have different operator meanings.
  if (status === "acked") return "bg-emerald-100 text-emerald-700";
  if (status === "delivered") return "bg-blue-100 text-blue-700";
  if (["failed", "nacked", "rejected"].includes(status)) return "bg-red-100 text-red-700";
  return "bg-slate-100 text-slate-700";
}

/** Keeps the full receipt row keyboard accessible with a single inspection button. */
function WebhookReceiptRow({ event, service, onSelect }: { event: WebhookEventEntry; service: ReceiptService; onSelect: (id: string) => void }) {
  // Older receipts can carry delivery outcome only in the legacy status field.
  const status = event.delivery_status || event.status;
  const received = new Date(event.created_at).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
  return (
    <li className="min-w-0">
      <button type="button" aria-haspopup="dialog" onClick={() => onSelect(event.id)} className={`grid w-full min-w-0 cursor-pointer grid-cols-[minmax(0,1fr)_1rem] items-center gap-x-3 gap-y-1 px-4 py-3.5 text-left transition-colors hover:bg-slate-50 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-blue-500 ${receiptColumns}`}>
        <span className="col-start-1 row-start-1 min-w-0 text-sm font-semibold text-slate-900 [overflow-wrap:anywhere]">
          {/* Failed verification can leave an event without a recognized name. */}
          {event.event_name || "Unnamed event"}
        </span>
        <span className="col-start-1 row-start-2 min-w-0 text-sm text-slate-600 [overflow-wrap:anywhere] @2xl:col-start-2 @2xl:row-start-1">
          <span className="sr-only">Service </span>{receiptServiceName(event, service) || "Unknown service"}
        </span>
        <span className="col-start-1 row-start-3 flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 @2xl:col-start-3 @2xl:row-start-1">
          <span className={`rounded-md px-2 py-0.5 text-xs font-medium [overflow-wrap:anywhere] ${deliveryColor(status)}`}><span className="sr-only">Delivery </span>{status || "Not recorded"}</span>
          {/* Verification can be absent on historical receipts and remains available in the inspector. */}
          {event.verification_status && <span className="text-xs text-slate-500 [overflow-wrap:anywhere]"><span className="sr-only">Verification </span>{event.verification_status}</span>}
        </span>
        <time dateTime={event.created_at} className="col-start-1 row-start-4 text-xs leading-5 text-slate-500 @2xl:col-start-4 @2xl:row-start-1">
          <span className="sr-only">Received </span>{received}
        </time>
        <ChevronRight aria-hidden="true" className="col-start-2 row-span-4 row-start-1 h-4 w-4 text-slate-400 @2xl:col-start-5 @2xl:row-span-1" />
      </button>
    </li>
  );
}

export interface WebhookLogsCardProps {
  service: ReceiptService;
  webhookNameOptions: string[];
  webhookEvents: WebhookEventEntry[];
  webhookTotal: number;
  webhookPage: number;
  setWebhookPage: (page: number | ((prev: number) => number)) => void;
  webhookLimit: number;
  webhookFilterEvent: string;
  setWebhookFilterEvent: (event: string) => void;
  webhookStartDate: string;
  setWebhookStartDate: (date: string) => void;
  webhookEndDate: string;
  setWebhookEndDate: (date: string) => void;
  webhookAnalytics: WebhookAnalyticsSummary | null;
  loadWebhookData: () => void;
  showSummary?: boolean;
  emptyMessage?: string;
}

/** Shares width-aware receipt browsing and inspection across both webhook activity views. */
export function WebhookLogsCard({
  service,
  webhookNameOptions,
  webhookEvents,
  webhookTotal,
  webhookPage,
  setWebhookPage,
  webhookLimit,
  webhookFilterEvent,
  setWebhookFilterEvent,
  webhookStartDate,
  setWebhookStartDate,
  webhookEndDate,
  setWebhookEndDate,
  webhookAnalytics,
  loadWebhookData,
  showSummary = true,
  emptyMessage = "No incoming webhooks yet.",
}: WebhookLogsCardProps) {
  const [selectedEventID, setSelectedEventID] = useState("");
  const selectedEvent = webhookEvents.find((event) => event.id === selectedEventID) ?? null;
  return (
    <div className="@container min-w-0 max-w-full rounded-lg border border-slate-200 bg-white">
      <div className="flex min-w-0 flex-wrap items-center justify-between gap-3 rounded-t-lg border-b border-slate-100 bg-slate-50 px-4 py-3">
        <h2 className="text-sm font-semibold text-slate-800">
          Incoming webhook receipts
        </h2>
        <div className="flex min-w-0 max-w-full flex-wrap items-center gap-3">
          <Select
            aria-label="Filter webhook event type"
            value={webhookFilterEvent}
            onChange={(e) => setWebhookFilterEvent(e.target.value)}
            className="max-w-full min-w-0 rounded border border-slate-200 bg-white px-2 py-1 text-sm text-slate-700"
          >
            <option value="">All Events</option>
            {webhookNameOptions.map(name => (
              <option key={name} value={name}>{name}</option>
            ))}
            <option value="UNKNOWN">UNKNOWN</option>
          </Select>
          <div className="flex min-w-0 flex-wrap items-center gap-2">
            <input
              type="date"
              aria-label="Webhook start date"
              value={webhookStartDate}
              onChange={(e) => setWebhookStartDate(e.target.value)}
              className="text-sm border border-slate-200 rounded px-2 py-1 bg-white text-slate-700 outline-none flex-1 sm:flex-initial min-w-[110px]"
              placeholder="Start Date"
            />
            <span className="text-slate-400 text-sm">to</span>
            <input
              type="date"
              aria-label="Webhook end date"
              value={webhookEndDate}
              onChange={(e) => setWebhookEndDate(e.target.value)}
              className="text-sm border border-slate-200 rounded px-2 py-1 bg-white text-slate-700 outline-none flex-1 sm:flex-initial min-w-[110px]"
              placeholder="End Date"
            />
          </div>
          <button
            data-track="refresh_webhook_logs"
            onClick={() => loadWebhookData()}
            className="p-1.5 text-slate-400 hover:text-slate-600 hover:bg-slate-200 rounded transition-colors cursor-pointer shrink-0"
            title="Refresh Logs"
          >
            <RefreshCw className="w-4 h-4" />
          </button>
        </div>
      </div>

      {/* Dedicated analytics tabs own their metrics; service Activity retains its inline summary. */}
      {showSummary && webhookAnalytics && (
        <div className="grid grid-cols-2 gap-px border-b border-slate-200 bg-slate-100 sm:grid-cols-4">
          <div className="bg-white p-4">
            <div className="mb-1 text-xs font-medium text-slate-500">Received</div>
            <div className="text-xl font-semibold text-slate-950">{webhookAnalytics.total_ingested}</div>
          </div>
          <div className="bg-white p-4">
            <div className="mb-1 text-xs font-medium text-slate-500">Delivered</div>
            <div className="text-xl font-semibold text-slate-950">{webhookAnalytics.total_delivered}</div>
          </div>
          <div className="bg-white p-4">
            <div className="mb-1 text-xs font-medium text-slate-500">Rejected</div>
            <div className="text-xl font-semibold text-slate-950">{webhookAnalytics.total_rejected}</div>
          </div>
          <div className="bg-white p-4">
            <div className="mb-1 text-xs font-medium text-slate-500">Failed</div>
            <div className="text-xl font-semibold text-slate-950">{webhookAnalytics.total_failed}</div>
          </div>
        </div>
      )}

      {/* A single list switches from stacked metadata to columns within the available width. */}
      {webhookEvents.length > 0 ? (
        <>
          <div aria-hidden="true" className={`hidden border-b border-slate-200 bg-slate-50 px-4 py-2.5 text-xs font-medium text-slate-500 @2xl:grid ${receiptColumns}`}>
            <span>Event</span><span>Service</span><span>Delivery</span><span>Received</span>
          </div>
          <ul aria-label="Incoming webhook receipts" className="divide-y divide-slate-100">
            {webhookEvents.map((event) => <WebhookReceiptRow key={event.id} event={event} service={service} onSelect={setSelectedEventID} />)}
          </ul>
        </>
      ) : (
        <p className="px-5 py-10 text-center text-sm text-slate-500">{emptyMessage}</p>
      )}

      {/* Server totals preserve page controls even when an offset has become empty. */}
      {webhookTotal > 0 && (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-b-lg border-t border-slate-100 bg-white px-4 py-3">
          <p className="text-xs text-slate-500">
            {Math.min((webhookPage - 1) * webhookLimit + 1, webhookTotal)}-{Math.min(webhookTotal, webhookPage * webhookLimit)} of {webhookTotal}
          </p>
          <div className="flex items-center gap-1">
            <button
              type="button"
              data-track="paginate_previous"
              onClick={() => setWebhookPage(p => Math.max(1, p - 1))}
              disabled={webhookPage <= 1}
              className="rounded-md p-1.5 text-slate-500 hover:bg-slate-100 disabled:opacity-40 disabled:hover:bg-transparent"
              aria-label="Previous page"
              title="Previous"
            >
              <ChevronLeft className="w-4 h-4" />
            </button>
            <span className="text-xs text-slate-500 pl-2">Page</span>
            <Select
              className="bg-white border border-slate-200 rounded px-2 py-1 text-xs focus:outline-none focus:ring-2 focus:ring-blue-500 mx-1 cursor-pointer"
              value={webhookPage}
              onChange={(e) => setWebhookPage(parseInt(e.target.value, 10))}
            >
              {Array.from({ length: Math.ceil(webhookTotal / webhookLimit) }, (_, i) => i + 1).map(p => (
                <option key={p} value={p}>{p}</option>
              ))}
            </Select>
            <span className="text-xs font-medium text-slate-500 pr-2">
              of {Math.max(1, Math.ceil(webhookTotal / webhookLimit))}
            </span>
            <button
              type="button"
              data-track="paginate_next"
              onClick={() => setWebhookPage(p => p + 1)}
              disabled={webhookPage * webhookLimit >= webhookTotal}
              className="rounded-md p-1.5 text-slate-500 hover:bg-slate-100 disabled:opacity-40 disabled:hover:bg-transparent"
              aria-label="Next page"
              title="Next"
            >
              <ChevronRight className="w-4 h-4" />
            </button>
          </div>
        </div>
      )}
      {/* The selected receipt retains its recorded service in the inspector. */}
      {selectedEvent && <WebhookEventDetailsDrawer event={selectedEvent} serviceName={receiptServiceName(selectedEvent, service)} onClose={() => setSelectedEventID("")} />}
    </div>
  );
}
