import { useState, useEffect } from "react";
import { useParams, Link, useSearchParams, type MetaFunction } from "@remix-run/react";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m) => m.id === "root").flatMap((m) => m.meta ?? []);
  return [
    ...parentMeta.filter((m) => !('title' in m)),
    { title: "MCP server activity - Fused" },
  ];
};
import { ArrowLeft, Clock } from "lucide-react";
import { api } from "~/lib/api";
import { McpAnalyticsPanel, type McpAnalyticsData } from "~/components/mcp/McpAnalyticsPanel";

export default function McpAnalyticsDashboard() {
  const { id } = useParams();
  const [data, setData] = useState<McpAnalyticsData | null>(null);
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
    api.mcpGraphql<{ mcpAnalytics: McpAnalyticsData }>(queryStr, { id })
      .then(res => {
        setData(res.mcpAnalytics);
      })
      .catch(e => setError(e instanceof Error ? e.message : "Failed to load analytics"))
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

  return (
    <div className="space-y-6 animate-in fade-in slide-in-from-bottom-4 duration-500 max-w-5xl mx-auto">
      <div className="flex items-center gap-4">
        <Link to="/integrations/mcp" className="p-2 bg-white border border-slate-200 text-slate-600 rounded-lg hover:bg-slate-50 hover:text-slate-900 transition-colors shadow-sm">
          <ArrowLeft className="w-4 h-4" />
        </Link>
        <div>
          <h1 className="text-xl font-bold text-slate-900 tracking-tight">MCP server activity</h1>
          <p className="text-sm text-slate-500 mt-1">Requests and sessions for this server.</p>
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
          Requests
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
          {(data.recent_sessions?.length ?? 0) > 0 && (
            <span className={`px-2 py-0.5 rounded-full text-xs transition-colors ${
              activeTab === "sessions"
                ? "bg-slate-100 text-slate-600"
                : "bg-slate-200/50 text-slate-600"
            }`}>
              {data.recent_sessions?.length}
            </span>
          )}
        </button>
      </div>

      {activeTab === "analytics" ? (
        <McpAnalyticsPanel data={data} />
      ) : (
        <div className="bg-white rounded-xl border border-slate-200 shadow-sm overflow-hidden">
          <div className="px-5 py-4 border-b border-slate-100 flex items-center gap-2">
            <Clock className="w-4 h-4 text-blue-500" />
            <h3 className="font-semibold text-slate-900">Recent sessions</h3>
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
                    {data.recent_sessions.map((sess) => {
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
