import { useState, useEffect } from "react";
import { useParams, Link, useSearchParams, type MetaFunction } from "@remix-run/react";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m: any) => m.id === "root").flatMap((m: any) => m.meta ?? []);
  return [
    ...parentMeta.filter((m: any) => !('title' in m)),
    { title: "MCP Diagnostics - Fused" },
  ];
};
import { ArrowLeft, Activity, ServerCrash, Clock, Users, Wrench } from "lucide-react";
import { api } from "~/lib/api";

export default function McpAnalyticsDashboard() {
  const { id } = useParams();
  const [data, setData] = useState<any>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [searchParams, setSearchParams] = useSearchParams();
  const urlTab = searchParams.get("tab");
  const activeTab = urlTab === "sessions" ? "sessions" : "analytics";

  const handleTabChange = (newTab: "analytics" | "sessions") => {
    setSearchParams(prev => {
      prev.set("tab", newTab);
      return prev;
    }, { replace: true });
  };

  const fetchAnalytics = () => {
    if (!id) return;
    setLoading(true);
    const queryStr = `
      query($id: String!) {
        mcpAnalytics(artifactId: $id) {
          total_requests
          failed_requests
          average_latency
          active_agents
          tool_usage {
            tool_name
            count
            failed
            average_latency
          }
          service_usage {
            service_name
            count
            failed
            average_latency
          }
          recent_sessions {
            id
            session_id
            started_at
            ended_at
          }
        }
      }
    `;
    // mcpAnalytics lives on the Engine's own MCP GraphQL schema now
    // (internal/engine/api/mcp_graphql.go), not the Registry-proxied
    // api.graphql this page used to call.
    api.mcpGraphql<{ mcpAnalytics: any }>(queryStr, { id })
      .then(res => {
        setData(res.mcpAnalytics);
      })
      .catch(e => setError(e.message))
      .finally(() => setLoading(false));
  };

  useEffect(() => {
    fetchAnalytics();
    const interval = setInterval(fetchAnalytics, 60000);
    return () => clearInterval(interval);
  }, [id]);

  if (loading && !data) return <div className="text-center py-12 text-slate-500">Loading analytics...</div>;
  if (error) return <div className="text-center py-12 text-red-500">Error: {error}</div>;
  if (!data) return null;

  const toolUsage = data.tool_usage ?? [];
  const serviceUsage = (data.service_usage ?? []).filter((svc: any) => {
    const name = typeof svc?.service_name === "string" ? svc.service_name.trim() : "";
    return name.length > 0;
  });

  return (
    <div className="space-y-6 animate-in fade-in slide-in-from-bottom-4 duration-500 max-w-5xl mx-auto">
      <div className="flex items-center gap-4">
        <Link to="/integrations/mcp" className="p-2 bg-white border border-slate-200 text-slate-600 rounded-lg hover:bg-slate-50 hover:text-indigo-600 transition-colors shadow-sm">
          <ArrowLeft className="w-4 h-4" />
        </Link>
        <div>
          <h2 className="text-xl font-bold text-slate-900 tracking-tight">MCP Diagnostics</h2>
          <p className="text-sm text-slate-500 mt-1">Real-time usage metrics and session data for this sandbox.</p>
        </div>
      </div>

      <div className="flex bg-slate-100 p-1 rounded-lg w-fit mb-6">
        <button
          data-track="view_mcp_analytics_tab"
          type="button"
          onClick={() => handleTabChange("analytics")}
          className={`px-4 py-1.5 text-sm font-medium rounded-md transition-all ${
            activeTab === "analytics"
              ? "bg-white text-slate-900 shadow-sm"
              : "text-slate-500 hover:text-slate-700 hover:bg-slate-200/50"
          } cursor-pointer`}
        >
          Analytics
        </button>
        <button
          data-track="view_mcp_sessions_tab"
          type="button"
          onClick={() => handleTabChange("sessions")}
          className={`px-4 py-1.5 text-sm font-medium rounded-md transition-all flex items-center gap-2 ${
            activeTab === "sessions"
              ? "bg-white text-slate-900 shadow-sm"
              : "text-slate-500 hover:text-slate-700 hover:bg-slate-200/50"
          } cursor-pointer`}
        >
          Sessions
          {data.recent_sessions?.length > 0 && (
            <span className={`px-2 py-0.5 rounded-full text-xs transition-colors ${
              activeTab === "sessions"
                ? "bg-slate-100 text-slate-600"
                : "bg-slate-200/50 text-slate-600"
            }`}>
              {data.recent_sessions.length}
            </span>
          )}
        </button>
      </div>

      {activeTab === "analytics" ? (
        <>
          <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
            <div className="bg-white p-5 rounded-xl border border-slate-200 shadow-sm flex flex-col relative overflow-hidden">
              <div className="absolute top-0 right-0 p-4 opacity-5">
            <Activity className="w-16 h-16 text-indigo-600" />
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

      <div className="bg-white rounded-xl border border-slate-200 shadow-sm overflow-hidden">
        <div className="px-5 py-4 border-b border-slate-100 flex items-center gap-2">
          <Wrench className="w-4 h-4 text-indigo-500" />
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
                  {toolUsage.map((tool: any) => {
                    const successRate = tool.count > 0 
                      ? (((tool.count - tool.failed) / tool.count) * 100).toFixed(1)
                      : 0;
                    return (
                      <tr key={tool.tool_name} className="hover:bg-slate-50 transition-colors">
                        <td className="px-6 py-4 font-mono text-indigo-600">{tool.tool_name}</td>
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

      <div className="bg-white rounded-xl border border-slate-200 shadow-sm overflow-hidden mt-6">
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
                  {serviceUsage.map((svc: any) => {
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
        </>
      ) : (
        <div className="bg-white rounded-xl border border-slate-200 shadow-sm overflow-hidden">
          <div className="px-5 py-4 border-b border-slate-100 flex items-center gap-2">
            <Clock className="w-4 h-4 text-blue-500" />
            <h3 className="font-semibold text-slate-900">Recent Agentic Sessions</h3>
          </div>
          <div className="p-0">
            {(!data.recent_sessions || data.recent_sessions.length === 0) ? (
              <div className="text-center py-12 text-slate-500 text-sm">
                No sessions recorded yet.
              </div>
            ) : (
              <div className="overflow-x-auto">
                <table className="w-full text-sm text-left">
                  <thead className="bg-slate-50/50 text-slate-500 text-xs uppercase tracking-wider">
                    <tr>
                      <th className="px-6 py-3 font-medium">Session ID</th>
                      <th className="px-6 py-3 font-medium">Started At</th>
                      <th className="px-6 py-3 font-medium">Ended At</th>
                      <th className="px-6 py-3 font-medium text-right">Status</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-slate-100 text-slate-700">
                    {data.recent_sessions.map((sess: any) => {
                      const isLive = !sess.ended_at;
                      return (
                        <tr key={sess.id} className="hover:bg-slate-50 transition-colors">
                          <td className="px-6 py-4 font-mono text-slate-600">{sess.session_id}</td>
                          <td className="px-6 py-4">{new Date(sess.started_at).toLocaleString()}</td>
                          <td className="px-6 py-4 text-slate-500">
                            {sess.ended_at ? new Date(sess.ended_at).toLocaleString() : "-"}
                          </td>
                          <td className="px-6 py-4 text-right">
                            {isLive ? (
                              <span className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-full text-xs font-medium bg-emerald-50 text-emerald-600 border border-emerald-100">
                                <span className="w-1.5 h-1.5 rounded-full bg-emerald-500 animate-pulse"></span>
                                Live
                              </span>
                            ) : (
                              <span className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-full text-xs font-medium bg-slate-100 text-slate-600 border border-slate-200">
                                Disconnected
                              </span>
                            )}
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
      )}
    </div>
  );
}
