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
import { AppRequestsPanel } from "~/components/activity/AppRequestsPanel";
import { NestedActivityTabs } from "~/components/activity/NestedActivityTabs";

export default function McpAnalyticsDashboard() {
  const { id } = useParams();
  const [data, setData] = useState<McpAnalyticsData | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [searchParams, setSearchParams] = useSearchParams();
  const activeTab = mcpActivityTab(searchParams.get("tab"));

  const handleTabChange = (newTab: "overview" | "requests" | "sessions") => {
    setSearchParams(prev => {
      if (newTab === "overview") prev.delete("tab");
      else prev.set("tab", newTab);
      return prev;
    }, { replace: true });
  };

  const fetchAnalytics = () => {
    if (!id) return;
    setLoading(true);
    const queryStr = `
      query($id: String!) {
        mcpAnalytics(app_id: $id) {
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

  return (
    <McpActivityState id={id} data={data} loading={loading} error={error} activeTab={activeTab} onTabChange={handleTabChange} />
  );
}

type McpActivityTab = "overview" | "requests" | "sessions";

function mcpActivityTab(value: string | null): McpActivityTab {
  return value === "requests" || value === "sessions" ? value : "overview";
}

function McpActivityState({ id, data, loading, error, activeTab, onTabChange }: {
  id?: string;
  data: McpAnalyticsData | null;
  loading: boolean;
  error: string;
  activeTab: McpActivityTab;
  onTabChange: (tab: McpActivityTab) => void;
}) {
  if (loading && !data) return <div className="text-center py-12 text-slate-500">Loading analytics...</div>;
  if (error) return <div className="text-center py-12 text-red-500">Error: {error}</div>;
  if (!data) return null;
  return <McpActivityContent id={id} data={data} activeTab={activeTab} onTabChange={onTabChange} />;
}

function McpActivityContent({ id, data, activeTab, onTabChange }: {
  id?: string;
  data: McpAnalyticsData;
  activeTab: McpActivityTab;
  onTabChange: (tab: McpActivityTab) => void;
}) {
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

      <NestedActivityTabs
        active={activeTab}
        ariaLabel="MCP server activity"
        onChange={onTabChange}
        options={[
          { value: "overview", label: "Overview", trackingId: "view_mcp_overview_tab" },
          { value: "requests", label: "Requests", trackingId: "view_mcp_requests_tab" },
          { value: "sessions", label: "Sessions", badge: data.recent_sessions?.length, trackingId: "view_mcp_sessions_tab" },
        ]}
      />

      {activeTab === "overview" && <McpAnalyticsPanel data={data} />}
      {activeTab === "requests" && id && <AppRequestsPanel appId={id} transport="mcp" />}
      {activeTab === "sessions" && <McpSessionsPanel sessions={data.recent_sessions || []} />}
    </div>
  );
}

function McpSessionsPanel({ sessions }: { sessions: NonNullable<McpAnalyticsData["recent_sessions"]> }) {
  return (
    <div className="bg-white rounded-xl border border-slate-200 shadow-sm overflow-hidden">
      <div className="px-5 py-4 border-b border-slate-100 flex items-center gap-2">
        <Clock className="w-4 h-4 text-blue-500" />
        <h3 className="font-semibold text-slate-900">Recent sessions</h3>
      </div>
      <div className="p-0">
        {sessions.length === 0 ? (
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
                {sessions.map((sess) => {
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
  );
}
