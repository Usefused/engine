import { Link } from "@remix-run/react";
import { RefreshCw, BarChart2 , ChevronLeft, ChevronRight } from "lucide-react";
import type { ServiceGenerationResult } from "~/lib/api";

function Badge({ label, color }: { label: string; color: string }) {
  return (
    <span className={`inline-flex items-center px-2 py-0.5 rounded text-xs font-medium ${color}`}>
      {label}
    </span>
  );
}

interface AnalyticsTabProps {
  res: ServiceGenerationResult;
  webhookEvents: any[];
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
  webhookAnalytics: any;
  loadWebhookData: () => void;
  dependentSDKs: any[];
  dependentMCPs: any[];
}

export default function AnalyticsTab({
  res,
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
  dependentSDKs,
  dependentMCPs,
}: AnalyticsTabProps) {
  return (
    <div className="space-y-6">
      <div className="bg-white rounded-xl border border-slate-200 overflow-hidden">
        <div className="px-5 py-3 border-b border-slate-100 flex flex-col sm:flex-row sm:items-center sm:justify-between gap-3 bg-slate-50">
          <h2 className="text-sm font-semibold text-slate-800">
            Webhook Delivery Logs
          </h2>
          <div className="flex flex-wrap items-center gap-3">
            <select
              value={webhookFilterEvent}
              onChange={(e) => setWebhookFilterEvent(e.target.value)}
              className="text-sm border border-slate-200 rounded px-2 py-1 bg-white text-slate-700 outline-none flex-1 sm:flex-initial"
            >
              <option value="">All Events</option>
              {Array.from(new Set(res.service.webhooks?.map((w: any) => w.name) || [])).map(name => (
                <option key={name as string} value={name as string}>{name as string}</option>
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

        {/* Analytics Stats */}
        {webhookAnalytics && (
          <div className="grid grid-cols-2 sm:grid-cols-4 gap-px bg-slate-100 border-b border-slate-200">
            <div className="bg-white p-4">
              <div className="text-[10px] uppercase font-bold text-slate-400 tracking-wider mb-1">Ingested</div>
              <div className="text-2xl font-light text-slate-800">{webhookAnalytics.total_ingested}</div>
            </div>
            <div className="bg-white p-4">
              <div className="text-[10px] uppercase font-bold text-slate-400 tracking-wider mb-1">Delivered</div>
              <div className="text-2xl font-light text-blue-600">{webhookAnalytics.total_delivered}</div>
            </div>
            <div className="bg-white p-4">
              <div className="text-[10px] uppercase font-bold text-slate-400 tracking-wider mb-1">Rejected</div>
              <div className="text-2xl font-light text-red-600">{webhookAnalytics.total_rejected}</div>
            </div>
            <div className="bg-white p-4">
              <div className="text-[10px] uppercase font-bold text-slate-400 tracking-wider mb-1">Failed</div>
              <div className="text-2xl font-light text-orange-600">{webhookAnalytics.total_failed}</div>
            </div>
          </div>
        )}

        <div className="overflow-x-auto">
          <table className="w-full text-left text-sm text-slate-600">
            <thead className="bg-slate-50 border-b border-slate-200 text-xs uppercase font-semibold text-slate-500">
              <tr>
                <th className="px-5 py-3 font-medium">Message ID</th>
                <th className="px-5 py-3 font-medium">Event Type</th>
                <th className="px-5 py-3 font-medium">Verification</th>
                <th className="px-5 py-3 font-medium">Delivery</th>
                <th className="px-5 py-3 font-medium">Latency</th>
                <th className="px-5 py-3 font-medium">Retries</th>
                <th className="px-5 py-3 font-medium">Credits</th>
                <th className="px-5 py-3 font-medium">Size</th>
                <th className="px-5 py-3 font-medium text-right">Time</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-slate-100">
              {webhookEvents.length > 0 ? (
                webhookEvents.map((ev, index) => (
                  <tr key={ev.id || ev.msg_id || index} className="hover:bg-slate-50 transition-colors">
                    <td className="px-5 py-3">
                      <code className="text-[10px] text-slate-400 truncate max-w-[80px] block" title={ev.msg_id}>{ev.msg_id}</code>
                      {ev.sdk_record_id && (
                        <code className="text-[9px] text-blue-400 mt-0.5 truncate max-w-[80px] block" title={`SDK Record: ${ev.sdk_record_id}`}>SDK: {ev.sdk_record_id.substring(0, 8)}...</code>
                      )}
                    </td>
                    <td className="px-5 py-3 font-medium text-slate-800">
                      {ev.event_name}
                    </td>
                    <td className="px-5 py-3">
                      {ev.verification_status ? (
                        <Badge
                          label={ev.verification_status}
                          color={ev.verification_status === "passed" ? "bg-green-100 text-green-700" : "bg-slate-100 text-slate-700"}
                        />
                      ) : <span className="text-slate-300">-</span>}
                    </td>
                    <td className="px-5 py-3">
                      <Badge
                        label={ev.delivery_status || ev.status}
                        color={
                          (ev.delivery_status || ev.status) === "acked" ? "bg-green-100 text-green-700" :
                          (ev.delivery_status || ev.status) === "delivered" ? "bg-blue-100 text-blue-700" :
                          (ev.delivery_status || ev.status) === "failed" || (ev.delivery_status || ev.status) === "nacked" || (ev.delivery_status || ev.status) === "rejected" ? "bg-red-100 text-red-700" :
                          "bg-slate-100 text-slate-700"
                        }
                      />
                      {ev.error_reason && (
                        <div className="text-[10px] text-red-500 mt-1 max-w-[150px] truncate" title={ev.error_reason}>
                          {ev.error_reason}
                        </div>
                      )}
                    </td>
                    <td className="px-5 py-3 text-slate-500">
                      {ev.latency_ms > 0 ? `${ev.latency_ms} ms` : <span className="text-slate-300">-</span>}
                    </td>
                    <td className="px-5 py-3 text-slate-500">
                      {ev.retry_count}
                    </td>
                    <td className="px-5 py-3 text-slate-500">
                      {ev.credits_consumed > 0 ? ev.credits_consumed.toFixed(2) : <span className="text-slate-300">-</span>}
                    </td>
                    <td className="px-5 py-3 text-slate-500">
                      {(ev.payload_size / 1024).toFixed(1)} kb
                    </td>
                    <td className="px-5 py-3 text-right text-xs text-slate-400 whitespace-nowrap">
                      {new Date(ev.created_at).toLocaleTimeString([], { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' })}
                    </td>
                  </tr>
                ))
              ) : (
                <tr>
                  <td colSpan={9} className="px-5 py-8 text-center text-slate-500">
                    No webhook events received yet.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>

        {/* Pagination Controls */}
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

      <div className="grid grid-cols-1 md:grid-cols-2 gap-6">
        <div className="bg-white rounded-xl border border-slate-200 overflow-hidden">
          <div className="px-5 py-3 border-b border-slate-100 bg-slate-50">
            <h2 className="text-sm font-semibold text-slate-800">
              SDKs Using This Service
            </h2>
          </div>
          <div className="divide-y divide-slate-100 max-h-[400px] overflow-y-auto">
            {dependentSDKs.length > 0 ? (
              dependentSDKs.map((sdk) => (
                <div key={sdk.id} className="p-4 hover:bg-slate-50 transition-colors">
                  <div className="flex items-center justify-between mb-1">
                    <Link to={`/integrations/sdks?search=${sdk.id}`} className="font-medium text-sm text-blue-600 hover:underline">
                      {sdk.name}
                    </Link>
                    <span className="text-xs text-slate-400">v{sdk.version}</span>
                  </div>
                  <div className="flex items-center justify-between mt-2">
                    <span className="text-xs text-slate-500">v{sdk.version}</span>
                    <span className="text-[10px] text-slate-400">{new Date(sdk.created_at).toLocaleDateString()}</span>
                  </div>
                </div>
              ))
            ) : (
              <div className="p-8 text-center text-slate-500 text-sm">
                No SDKs found using this service.
              </div>
            )}
          </div>
        </div>

        <div className="bg-white rounded-xl border border-slate-200 overflow-hidden">
          <div className="px-5 py-3 border-b border-slate-100 bg-slate-50">
            <h2 className="text-sm font-semibold text-slate-800">
              MCP Servers Using This Service
            </h2>
          </div>
          <div className="divide-y divide-slate-100 max-h-[400px] overflow-y-auto">
            {dependentMCPs.length > 0 ? (
              dependentMCPs.map((mcp) => (
                <div key={mcp.id} className="p-4 hover:bg-slate-50 transition-colors">
                  <div className="flex items-center justify-between mb-1">
                    <Link to={`/integrations/mcp/${mcp.id}/analytics`} className="font-medium text-sm text-blue-600 hover:underline flex items-center gap-1">
                      {mcp.name}
                      <BarChart2 className="w-3 h-3 text-slate-400" />
                    </Link>
                    <span className="text-xs text-slate-400">v{mcp.version}</span>
                  </div>
                  <div className="flex items-center justify-between mt-2">
                    <span className="text-xs text-slate-500">v{mcp.version}</span>
                    <span className="text-[10px] text-slate-400">{new Date(mcp.created_at).toLocaleDateString()}</span>
                  </div>
                </div>
              ))
            ) : (
              <div className="p-8 text-center text-slate-500 text-sm">
                No MCP Servers found using this service.
              </div>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
