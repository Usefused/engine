import { RefreshCw, ChevronLeft, ChevronRight } from "lucide-react";
import { type WebhookEventEntry, type WebhookAnalyticsSummary } from "~/lib/api";

function Badge({ label, color }: { label: string; color: string }) {
  return (
    <span className={`inline-flex items-center px-2 py-0.5 rounded text-xs font-medium ${color}`}>
      {label}
    </span>
  );
}

function deliveryStatus(event: WebhookEventEntry): string {
  return event.delivery_status || event.status;
}

function mobileDeliveryColor(status: string): string {
  if (status === "failed" || status === "rejected") return "bg-red-100 text-red-700";
  return "bg-slate-100 text-slate-700";
}

function desktopDeliveryColor(status: string): string {
  if (status === "acked") return "bg-green-100 text-green-700";
  if (status === "delivered") return "bg-blue-100 text-blue-700";
  if (status === "failed" || status === "nacked" || status === "rejected") return "bg-red-100 text-red-700";
  return "bg-slate-100 text-slate-700";
}

function WebhookEventCard({ event }: { event: WebhookEventEntry }) {
  const status = deliveryStatus(event);
  return (
    <article className="space-y-3 p-4">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="break-words text-sm font-medium text-slate-950">{event.event_name}</div>
          <div className="mt-1 truncate font-mono text-[11px] text-slate-400" title={event.msg_id}>{event.msg_id}</div>
        </div>
        <Badge label={status} color={mobileDeliveryColor(status)} />
      </div>
      <dl className="grid grid-cols-2 gap-3 text-xs">
        <div><dt className="text-slate-400">Verification</dt><dd className="mt-0.5 text-slate-700">{event.verification_status || "Not recorded"}</dd></div>
        <div><dt className="text-slate-400">Environment</dt><dd className="mt-0.5 text-slate-700">{event.environment || "Not recorded"}</dd></div>
        <div><dt className="text-slate-400">Delivery time</dt><dd className="mt-0.5 text-slate-700">{event.latency_ms > 0 ? `${event.latency_ms} ms` : "Not recorded"}</dd></div>
        <div><dt className="text-slate-400">Payload</dt><dd className="mt-0.5 text-slate-700">{(event.payload_size / 1024).toFixed(1)} KB</dd></div>
        <div><dt className="text-slate-400">Retries</dt><dd className="mt-0.5 text-slate-700">{event.retry_count}</dd></div>
        <div><dt className="text-slate-400">Received</dt><dd className="mt-0.5 text-slate-700">{new Date(event.created_at).toLocaleString()}</dd></div>
      </dl>
      {event.error_reason ? <div className="text-xs text-red-700">{event.error_reason}</div> : null}
    </article>
  );
}

function SdkRecordCode({ sdkRecordId }: { sdkRecordId?: string }) {
  if (!sdkRecordId) return null;
  return (
    <code className="text-[9px] text-blue-400 mt-0.5 truncate max-w-[80px] block" title={`SDK Record: ${sdkRecordId}`}>SDK: {sdkRecordId.substring(0, 8)}...</code>
  );
}

function VerificationCell({ status }: { status?: string }) {
  if (!status) return <span className="text-slate-300">-</span>;
  return <Badge label={status} color={status === "passed" ? "bg-green-100 text-green-700" : "bg-slate-100 text-slate-700"} />;
}

function WebhookEventRow({ ev }: { ev: WebhookEventEntry }) {
  const status = deliveryStatus(ev);
  return (
    <tr className="hover:bg-slate-50 transition-colors">
      <td className="break-words px-4 py-3 font-medium text-slate-900">{ev.event_name}</td>
      <td className="px-4 py-3">
        <code className="text-[10px] text-slate-400 truncate max-w-[80px] block" title={ev.msg_id}>{ev.msg_id}</code>
        <SdkRecordCode sdkRecordId={ev.sdk_record_id} />
      </td>
      <td className="px-4 py-3">
        <VerificationCell status={ev.verification_status} />
      </td>
      <td className="px-4 py-3">
        <Badge label={status} color={desktopDeliveryColor(status)} />
        {ev.error_reason && (
          <div className="text-[10px] text-red-500 mt-1 max-w-[150px] truncate" title={ev.error_reason}>
            {ev.error_reason}
          </div>
        )}
      </td>
      <td className="px-4 py-3 text-slate-500">{ev.environment || "Not recorded"}</td>
      <td className="px-4 py-3 text-slate-500">
        {ev.latency_ms > 0 ? `${ev.latency_ms} ms` : <span className="text-slate-300">-</span>}
      </td>
      <td className="px-4 py-3 text-slate-500">
        {typeof ev.payload_size === "number" ? `${(ev.payload_size / 1024).toFixed(1)} KB` : <span className="text-slate-300">-</span>}
      </td>
      <td className="px-4 py-3 text-slate-500">
        {ev.retry_count}
      </td>
      <td className="whitespace-nowrap px-4 py-3 text-right text-xs text-slate-400">
        {new Date(ev.created_at).toLocaleString()}
      </td>
    </tr>
  );
}

export interface WebhookLogsCardProps {
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
}

// Shared between the per-service Webhook activity tab (components/AnalyticsTab.tsx)
// and the Observability page's Webhooks tab -- both feed it results from the
// same api.workspace.listWebhookEvents / getWebhookAnalytics calls.
export function WebhookLogsCard({
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
}: WebhookLogsCardProps) {
  return (
    <div className="overflow-hidden rounded-lg border border-slate-200 bg-white">
      <div className="flex flex-col gap-3 border-b border-slate-100 bg-slate-50 px-4 py-3 sm:flex-row sm:items-center sm:justify-between">
        <h2 className="text-sm font-semibold text-slate-800">
          Incoming webhook receipts
        </h2>
        <div className="flex flex-wrap items-center gap-3">
          <select
            value={webhookFilterEvent}
            onChange={(e) => setWebhookFilterEvent(e.target.value)}
            className="text-sm border border-slate-200 rounded px-2 py-1 bg-white text-slate-700 outline-none flex-1 sm:flex-initial"
          >
            <option value="">All Events</option>
            {webhookNameOptions.map(name => (
              <option key={name} value={name}>{name}</option>
            ))}
            <option value="UNKNOWN">UNKNOWN</option>
          </select>
          <div className="flex items-center gap-2 flex-wrap flex-1 sm:flex-initial">
            <input
              type="date"
              value={webhookStartDate}
              onChange={(e) => setWebhookStartDate(e.target.value)}
              className="text-sm border border-slate-200 rounded px-2 py-1 bg-white text-slate-700 outline-none flex-1 sm:flex-initial min-w-[110px]"
              placeholder="Start Date"
            />
            <span className="text-slate-400 text-sm">to</span>
            <input
              type="date"
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

      {webhookAnalytics && (
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

      <div className="divide-y divide-slate-100 md:hidden">
        {webhookEvents.length > 0 ? webhookEvents.map((event, index) => (
          <WebhookEventCard key={event.id || event.msg_id || index} event={event} />
        )) : (
          <div className="px-5 py-10 text-center text-sm text-slate-500">No incoming webhooks yet.</div>
        )}
      </div>

      <div className="hidden overflow-x-auto md:block">
        <table className="w-full min-w-[900px] table-fixed text-left text-sm text-slate-600">
          <thead className="border-b border-slate-200 bg-slate-50 text-xs font-medium text-slate-500">
            <tr>
              <th className="w-[17%] px-4 py-3">Event</th>
              <th className="w-[15%] px-4 py-3">Message ID</th>
              <th className="w-[12%] px-4 py-3">Verification</th>
              <th className="w-[11%] px-4 py-3">Delivery</th>
              <th className="w-[11%] px-4 py-3">Environment</th>
              <th className="w-[11%] px-4 py-3">Delivery time</th>
              <th className="w-[8%] px-4 py-3">Payload</th>
              <th className="w-[6%] px-4 py-3">Retries</th>
              <th className="w-[9%] px-4 py-3 text-right">Received</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-slate-100">
            {webhookEvents.length > 0 ? (
              webhookEvents.map((ev, index) => (
                <WebhookEventRow key={ev.id || ev.msg_id || index} ev={ev} />
              ))
            ) : (
              <tr>
                <td colSpan={9} className="px-5 py-10 text-center text-slate-500">
                  No incoming webhooks yet.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>

      {webhookTotal > 0 && (
        <div className="flex items-center justify-between gap-3 border-t border-slate-100 px-4 py-3 bg-white">
          <p className="text-xs text-slate-500">
            {webhookTotal === 0 ? 0 : (webhookPage - 1) * webhookLimit + 1}-{Math.min(webhookTotal, webhookPage * webhookLimit)} of {webhookTotal}
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
            <select
              className="bg-white border border-slate-200 rounded px-2 py-1 text-xs focus:outline-none focus:ring-2 focus:ring-blue-500 mx-1 cursor-pointer"
              value={webhookPage}
              onChange={(e) => setWebhookPage(parseInt(e.target.value, 10))}
            >
              {Array.from({ length: Math.ceil(webhookTotal / webhookLimit) }, (_, i) => i + 1).map(p => (
                <option key={p} value={p}>{p}</option>
              ))}
            </select>
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
    </div>
  );
}
