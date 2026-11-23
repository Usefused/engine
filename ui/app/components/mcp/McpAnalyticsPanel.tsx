import { Activity, Clock, ServerCrash, Users, Wrench } from "lucide-react";
import type { McpSession } from "~/lib/mcp-sessions";
import { formatDuration } from "~/lib/format";

// The per-server activity route owns data loading; this component renders its
// overview without coupling presentation to GraphQL or route state.
export interface McpAnalyticsData {
  total_requests: number;
  failed_requests: number;
  average_latency: number;
  active_agents: number;
  tool_usage?: Array<{ tool_name: string; count: number; failed: number; average_latency: number }>;
  service_usage?: Array<{ service_name: string; count: number; failed: number; average_latency: number }>;
  recent_sessions?: McpSession[];
  token_activity?: Array<{
    id: string;
    name: string;
    binding_mode: "dynamic" | "fixed";
    status: "active" | "expired" | "revoked";
    issued_by_subject_id?: string;
    execution_count: number;
    session_count: number;
    created_at: string;
    last_used_at?: string;
    terminated_at?: string;
    termination_reason?: string;
  }>;
}

interface McpUsageCardEntry {
  key: string;
  name: string;
  count: number;
  failed: number;
  averageLatency: number;
  accent: "slate" | "emerald";
}

// usageSuccessRate keeps empty rows deterministic across mobile cards and
// desktop tables.
function usageSuccessRate(count: number, failed: number): string {
  if (count === 0) return "0.0";
  return (((count - failed) / count) * 100).toFixed(1);
}

// McpUsageCards exposes the complete usage summary as stacked cards when a
// five-column table would exceed the viewport.
function McpUsageCards({ entries }: { entries: McpUsageCardEntry[] }) {
  return (
    <div className="divide-y divide-slate-100 md:hidden">
      {entries.map((entry) => {
        const successRate = usageSuccessRate(entry.count, entry.failed);
        return (
          <div key={entry.key} className="p-4">
            <div className={`break-all font-mono text-sm font-medium ${entry.accent === "emerald" ? "text-emerald-700" : "text-slate-800"}`}>{entry.name}</div>
            <dl className="mt-3 grid grid-cols-2 gap-x-4 gap-y-3 text-sm">
              <div><dt className="text-xs text-slate-500">Calls</dt><dd className="mt-0.5 font-medium tabular-nums text-slate-800">{entry.count.toLocaleString()}</dd></div>
              <div><dt className="text-xs text-slate-500">Failed</dt><dd className={`mt-0.5 tabular-nums ${entry.failed > 0 ? "font-semibold text-red-600" : "text-slate-400"}`}>{entry.failed.toLocaleString()}</dd></div>
              <div><dt className="text-xs text-slate-500">Average latency</dt><dd className="mt-0.5 tabular-nums text-slate-700">{formatDuration(entry.averageLatency || 0)}</dd></div>
              <div><dt className="text-xs text-slate-500">Success rate</dt><dd className={`mt-0.5 tabular-nums ${entry.failed > 0 ? "text-amber-600" : "text-emerald-600"}`}>{successRate}%</dd></div>
            </dl>
          </div>
        );
      })}
    </div>
  );
}

// McpAnalyticsStats renders the high-level MCP measures in a responsive grid.
function McpAnalyticsStats({ data }: { data: McpAnalyticsData }) {
  return (
    <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
      <div className="bg-white p-5 rounded-xl border border-slate-200 shadow-sm flex flex-col relative overflow-hidden">
        <div className="absolute top-0 right-0 p-4 opacity-5">
          <Activity className="w-16 h-16 text-slate-600" />
        </div>
        <span className="text-slate-500 text-sm font-medium mb-1">Total Requests</span>
        <span className="text-3xl font-bold text-slate-900">{data.total_requests.toLocaleString()}</span>
      </div>
      <div className="bg-white p-5 rounded-xl border border-slate-200 shadow-sm flex flex-col relative overflow-hidden">
        <div className="absolute top-0 right-0 p-4 opacity-5">
          <ServerCrash className="w-16 h-16 text-red-600" />
        </div>
        <span className="text-slate-500 text-sm font-medium mb-1">Failed Requests</span>
        <span className="text-3xl font-bold text-slate-900">{data.failed_requests.toLocaleString()}</span>
        <div className="mt-1 flex items-center text-xs">
          <span className={data.failed_requests > 0 ? "text-red-500 font-semibold" : "text-emerald-500 font-semibold"}>
            {data.total_requests > 0 ? ((data.failed_requests / data.total_requests) * 100).toFixed(1) : 0}% failure rate
          </span>
        </div>
      </div>
      <div className="bg-white p-5 rounded-xl border border-slate-200 shadow-sm flex flex-col relative overflow-hidden">
        <div className="absolute top-0 right-0 p-4 opacity-5">
          <Clock className="w-16 h-16 text-blue-600" />
        </div>
        <span className="text-slate-500 text-sm font-medium mb-1">Avg Latency</span>
        <span className="text-3xl font-bold tabular-nums text-slate-900">{formatDuration(data.average_latency || 0)}</span>
      </div>
      <div className="bg-white p-5 rounded-xl border border-slate-200 shadow-sm flex flex-col relative overflow-hidden">
        <div className="absolute top-0 right-0 p-4 opacity-5">
          <Users className="w-16 h-16 text-emerald-600" />
        </div>
        <span className="text-slate-500 text-sm font-medium mb-1">Active Agents</span>
        <span className="text-3xl font-bold text-slate-900">{data.active_agents.toLocaleString()}</span>
        <div className="mt-1 flex items-center text-xs">
          <span className="text-emerald-500 font-semibold flex items-center gap-1">
            <span className="w-1.5 h-1.5 rounded-full bg-emerald-500 animate-pulse"></span>
            Live Connections
          </span>
        </div>
      </div>
    </div>
  );
}

interface McpUsageRow {
  name: string;
  count: number;
  failed: number;
  averageLatency: number;
}

// McpUsageTable renders tool or service usage breakdown as stacked cards on
// mobile and a dense metrics table when horizontal space is available.
function McpUsageTable({
  title,
  icon,
  emptyMessage,
  nameColumnLabel,
  accent,
  rows,
}: {
  title: string;
  icon: React.ReactNode;
  emptyMessage: string;
  nameColumnLabel: string;
  accent: "slate" | "emerald";
  rows: McpUsageRow[];
}) {
  const mobileEntries = rows.map((row) => ({
    key: row.name,
    name: row.name,
    count: row.count,
    failed: row.failed,
    averageLatency: row.averageLatency,
    accent,
  }));
  // Service names use emerald highlighting while tool names stay neutral slate.
  const nameClass = accent === "emerald" ? "text-emerald-600" : "text-slate-700";
  return (
    <div className="bg-white rounded-xl border border-slate-200 shadow-sm overflow-hidden">
      <div className="px-5 py-4 border-b border-slate-100 flex items-center gap-2">
        {icon}
        <h3 className="font-semibold text-slate-900">{title}</h3>
      </div>
      <div className="p-0">
        {rows.length === 0 ? (
          <div className="text-center py-12 text-slate-500 text-sm">
            {emptyMessage}
          </div>
        ) : (
          <>
            <McpUsageCards entries={mobileEntries} />
            <div className="hidden overflow-x-auto md:block">
              <table className="w-full text-sm text-left">
                <thead className="bg-slate-50/50 text-slate-500 text-xs uppercase tracking-wider">
                  <tr>
                    <th className="px-6 py-3 font-medium">{nameColumnLabel}</th>
                    <th className="px-6 py-3 font-medium text-right">Calls</th>
                    <th className="px-6 py-3 font-medium text-right">Failed</th>
                    <th className="px-6 py-3 font-medium text-right">Avg Latency</th>
                    <th className="px-6 py-3 font-medium text-right">Success Rate</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-slate-100 text-slate-700">
                  {rows.map((row) => {
                    const successRate = usageSuccessRate(row.count, row.failed);
                    return (
                      <tr key={row.name} className="hover:bg-slate-50 transition-colors">
                        <td className={`px-6 py-4 font-mono ${nameClass}`}>{row.name}</td>
                        <td className="px-6 py-4 text-right font-medium">{row.count.toLocaleString()}</td>
                        <td className="px-6 py-4 text-right">
                          <span className={row.failed > 0 ? "text-red-500 font-semibold" : "text-slate-400"}>
                            {row.failed.toLocaleString()}
                          </span>
                        </td>
                        <td className="px-6 py-4 text-right font-medium tabular-nums text-slate-600">
                          {formatDuration(row.averageLatency || 0)}
                        </td>
                        <td className="px-6 py-4 text-right">
                          <div className="flex items-center justify-end gap-2">
                            <span className={row.failed > 0 ? "text-amber-500" : "text-emerald-500"}>{successRate}%</span>
                            <div className="w-16 h-1.5 bg-slate-100 rounded-full overflow-hidden">
                              <div
                                className={`h-full ${row.failed > 0 ? "bg-amber-400" : "bg-emerald-400"}`}
                                style={{ width: `${successRate}%` }}
                              />
                            </div>
                          </div>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          </>
        )}
      </div>
    </div>
  );
}

/** Full analytics body (stats + tool/service usage tables), no header or
 * sessions tab -- callers compose those around it as needed. */
export function McpAnalyticsPanel({ data }: { data: McpAnalyticsData }) {
  const toolRows: McpUsageRow[] = (data.tool_usage ?? []).map((tool) => ({
    name: tool.tool_name,
    count: tool.count,
    failed: tool.failed,
    averageLatency: tool.average_latency,
  }));
  const serviceRows: McpUsageRow[] = (data.service_usage ?? [])
    .filter((svc) => {
      const name = typeof svc?.service_name === "string" ? svc.service_name.trim() : "";
      return name.length > 0;
    })
    .map((svc) => ({
      name: svc.service_name,
      count: svc.count,
      failed: svc.failed,
      averageLatency: svc.average_latency,
    }));

  return (
    <>
      <McpAnalyticsStats data={data} />
      <McpUsageTable
        title="Tool Usage Breakdown"
        icon={<Wrench className="w-4 h-4 text-slate-500" />}
        emptyMessage="No tools have been called yet."
        nameColumnLabel="Tool Name"
        accent="slate"
        rows={toolRows}
      />
      <div className="mt-6">
        <McpUsageTable
          title="Service Usage Breakdown"
          icon={<Activity className="w-4 h-4 text-emerald-500" />}
          emptyMessage="No services have been hit yet."
          nameColumnLabel="Service Name"
          accent="emerald"
          rows={serviceRows}
        />
      </div>
    </>
  );
}
