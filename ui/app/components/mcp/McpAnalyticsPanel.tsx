import { Activity, Clock, ServerCrash, Users, Wrench } from "lucide-react";

// Shared with the per-server /integrations/mcp/:id/analytics route and the
// Observability page's MCP tab -- both fetch the exact same
// `mcpAnalytics(artifactId)` query from the Engine's MCP GraphQL schema
// (internal/engine/api/mcp_graphql.go) and just hand the result here.
export interface McpAnalyticsData {
  total_requests: number;
  failed_requests: number;
  average_latency: number;
  active_agents: number;
  tool_usage?: Array<{ tool_name: string; count: number; failed: number; average_latency: number }>;
  service_usage?: Array<{ service_name: string; count: number; failed: number; average_latency: number }>;
  recent_sessions?: Array<{ id: string; session_id: string; started_at: string; ended_at?: string }>;
}

export function McpAnalyticsStats({ data }: { data: McpAnalyticsData }) {
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
        <span className="text-3xl font-bold text-slate-900">{Math.round(data.average_latency)}<span className="text-xl text-slate-400 ml-1">ms</span></span>
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

export function McpToolUsageTable({ toolUsage }: { toolUsage: NonNullable<McpAnalyticsData["tool_usage"]> }) {
  return (
    <div className="bg-white rounded-xl border border-slate-200 shadow-sm overflow-hidden">
      <div className="px-5 py-4 border-b border-slate-100 flex items-center gap-2">
        <Wrench className="w-4 h-4 text-slate-500" />
        <h3 className="font-semibold text-slate-900">Tool Usage Breakdown</h3>
      </div>
      <div className="p-0">
        {toolUsage.length === 0 ? (
          <div className="text-center py-12 text-slate-500 text-sm">
            No tools have been called yet.
          </div>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm text-left">
              <thead className="bg-slate-50/50 text-slate-500 text-xs uppercase tracking-wider">
                <tr>
                  <th className="px-6 py-3 font-medium">Tool Name</th>
                  <th className="px-6 py-3 font-medium text-right">Calls</th>
                  <th className="px-6 py-3 font-medium text-right">Failed</th>
                  <th className="px-6 py-3 font-medium text-right">Avg Latency</th>
                  <th className="px-6 py-3 font-medium text-right">Success Rate</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-100 text-slate-700">
                {toolUsage.map((tool) => {
                  const successRate = tool.count > 0
                    ? (((tool.count - tool.failed) / tool.count) * 100).toFixed(1)
                    : 0;
                  return (
                    <tr key={tool.tool_name} className="hover:bg-slate-50 transition-colors">
                      <td className="px-6 py-4 font-mono text-slate-700">{tool.tool_name}</td>
                      <td className="px-6 py-4 text-right font-medium">{tool.count.toLocaleString()}</td>
                      <td className="px-6 py-4 text-right">
                        <span className={tool.failed > 0 ? "text-red-500 font-semibold" : "text-slate-400"}>
                          {tool.failed.toLocaleString()}
                        </span>
                      </td>
                      <td className="px-6 py-4 text-right font-medium text-slate-600">
                        {Math.round(tool.average_latency || 0)}ms
                      </td>
                      <td className="px-6 py-4 text-right">
                        <div className="flex items-center justify-end gap-2">
                          <span className={tool.failed > 0 ? "text-amber-500" : "text-emerald-500"}>{successRate}%</span>
                          <div className="w-16 h-1.5 bg-slate-100 rounded-full overflow-hidden">
                            <div
                              className={`h-full ${tool.failed > 0 ? 'bg-amber-400' : 'bg-emerald-400'}`}
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
        )}
      </div>
    </div>
  );
}

export function McpServiceUsageTable({ serviceUsage }: { serviceUsage: NonNullable<McpAnalyticsData["service_usage"]> }) {
  return (
    <div className="bg-white rounded-xl border border-slate-200 shadow-sm overflow-hidden">
      <div className="px-5 py-4 border-b border-slate-100 flex items-center gap-2">
        <Activity className="w-4 h-4 text-emerald-500" />
        <h3 className="font-semibold text-slate-900">Service Usage Breakdown</h3>
      </div>
      <div className="p-0">
        {serviceUsage.length === 0 ? (
          <div className="text-center py-12 text-slate-500 text-sm">
            No services have been hit yet.
          </div>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm text-left">
              <thead className="bg-slate-50/50 text-slate-500 text-xs uppercase tracking-wider">
                <tr>
                  <th className="px-6 py-3 font-medium">Service Name</th>
                  <th className="px-6 py-3 font-medium text-right">Calls</th>
                  <th className="px-6 py-3 font-medium text-right">Failed</th>
                  <th className="px-6 py-3 font-medium text-right">Avg Latency</th>
                  <th className="px-6 py-3 font-medium text-right">Success Rate</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-100 text-slate-700">
                {serviceUsage.map((svc) => {
                  const successRate = svc.count > 0
                    ? (((svc.count - svc.failed) / svc.count) * 100).toFixed(1)
                    : 0;
                  return (
                    <tr key={svc.service_name} className="hover:bg-slate-50 transition-colors">
                      <td className="px-6 py-4 font-mono text-emerald-600">{svc.service_name}</td>
                      <td className="px-6 py-4 text-right font-medium">{svc.count.toLocaleString()}</td>
                      <td className="px-6 py-4 text-right">
                        <span className={svc.failed > 0 ? "text-red-500 font-semibold" : "text-slate-400"}>
                          {svc.failed.toLocaleString()}
                        </span>
                      </td>
                      <td className="px-6 py-4 text-right font-medium text-slate-600">
                        {Math.round(svc.average_latency || 0)}ms
                      </td>
                      <td className="px-6 py-4 text-right">
                        <div className="flex items-center justify-end gap-2">
                          <span className={svc.failed > 0 ? "text-amber-500" : "text-emerald-500"}>{successRate}%</span>
                          <div className="w-16 h-1.5 bg-slate-100 rounded-full overflow-hidden">
                            <div
                              className={`h-full ${svc.failed > 0 ? 'bg-amber-400' : 'bg-emerald-400'}`}
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
        )}
      </div>
    </div>
  );
}

/** Full analytics body (stats + tool/service usage tables), no header or
 * sessions tab -- callers compose those around it as needed. */
export function McpAnalyticsPanel({ data }: { data: McpAnalyticsData }) {
  const toolUsage = data.tool_usage ?? [];
  const serviceUsage = (data.service_usage ?? []).filter((svc) => {
    const name = typeof svc?.service_name === "string" ? svc.service_name.trim() : "";
    return name.length > 0;
  });

  return (
    <>
      <McpAnalyticsStats data={data} />
      <McpToolUsageTable toolUsage={toolUsage} />
      <div className="mt-6">
        <McpServiceUsageTable serviceUsage={serviceUsage} />
      </div>
    </>
  );
}
