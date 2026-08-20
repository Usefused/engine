import { useState, useEffect } from "react";
import { useParams, Link, useSearchParams, type MetaFunction } from "@remix-run/react";

// meta retains the workspace chrome metadata while identifying the scoped MCP
// Activity page in the browser title.
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
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasAnyPermission, hasWorkspacePermission } from "~/lib/current-actor-access";

/** Loads MCP overview data and separately gates request receipts. */
export default function McpAnalyticsDashboard() {
  const { access, loading: accessLoading } = useCurrentActorAccess();
  const canReadOverview = hasAnyPermission(access, "app.read") && hasWorkspacePermission(access, "audit.read");
  const canReadRequests = canReadOverview;
  const { id } = useParams();
  const [data, setData] = useState<McpAnalyticsData | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [searchParams, setSearchParams] = useSearchParams();
  const activeTab = mcpActivityTab(searchParams.get("tab"));

  // handleTabChange keeps Overview as the canonical URL and records only
  // non-default Activity selections in the query string.
  const handleTabChange = (newTab: "overview" | "requests" | "sessions") => {
    setSearchParams(prev => {
      if (newTab === "overview") prev.delete("tab");
      else prev.set("tab", newTab);
      return prev;
    }, { replace: true });
  };

  /** Refreshes analytics only after the broad client-side access preflight. */
  const fetchAnalytics = () => {
    // The route carries an immutable app ID while grants use family IDs, so
    // this preflight prevents known denials and the Engine enforces the exact
    // family permission when resolving mcpAnalytics.
    if (!id || !canReadOverview) return;
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
    if (accessLoading) return;
    if (!canReadOverview) {
      setLoading(false);
      setData(null);
      setError("");
      return;
    }
    fetchAnalytics();
    const interval = setInterval(fetchAnalytics, 60000);
    return () => clearInterval(interval);
  }, [accessLoading, canReadOverview, id]);

  return (
    <McpActivityState id={id} data={data} loading={loading || accessLoading} error={error} activeTab={activeTab} onTabChange={handleTabChange} canReadOverview={canReadOverview} canReadRequests={canReadRequests} />
  );
}

type McpActivityTab = "overview" | "requests" | "sessions";

// mcpActivityTab accepts only known Activity sections from the URL.
function mcpActivityTab(value: string | null): McpActivityTab {
  return value === "requests" || value === "sessions" ? value : "overview";
}

/** Selects the MCP activity loading, error, or content state. */
function McpActivityState({ id, data, loading, error, activeTab, onTabChange, canReadOverview, canReadRequests }: {
  id?: string;
  data: McpAnalyticsData | null;
  loading: boolean;
  error: string;
  activeTab: McpActivityTab;
  onTabChange: (tab: McpActivityTab) => void;
  canReadOverview: boolean;
  canReadRequests: boolean;
}) {
  if (loading && !data) return <div className="text-center py-12 text-slate-500">Loading analytics...</div>;
  if (!canReadOverview) return <div className="rounded-lg border border-slate-200 bg-white px-6 py-12 text-center text-sm text-slate-500">MCP activity access is not available for your account.</div>;
  if (error) return <div className="text-center py-12 text-red-500">Error: {error}</div>;
  if (!data) return null;
  return <McpActivityContent id={id} data={data} activeTab={activeTab} onTabChange={onTabChange} canReadRequests={canReadRequests} />;
}

/** Renders MCP activity tabs without mounting protected request data. */
function McpActivityContent({ id, data, activeTab, onTabChange, canReadRequests }: {
  id?: string;
  data: McpAnalyticsData;
  activeTab: McpActivityTab;
  onTabChange: (tab: McpActivityTab) => void;
  canReadRequests: boolean;
}) {
  return (
    <div className="mx-auto min-w-0 max-w-5xl space-y-5 overflow-x-hidden animate-in fade-in slide-in-from-bottom-4 duration-500 sm:space-y-6">
      <div className="flex min-w-0 items-center gap-3 sm:gap-4">
        <Link to="/integrations/mcp" aria-label="Back to MCP servers" className="shrink-0 rounded-lg border border-slate-200 bg-white p-2 text-slate-600 shadow-sm transition-colors hover:bg-slate-50 hover:text-slate-900">
          <ArrowLeft className="w-4 h-4" />
        </Link>
        <div className="min-w-0">
          <h1 className="text-lg font-bold tracking-tight text-slate-900 sm:text-xl">MCP server activity</h1>
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
      {activeTab === "requests" && canReadRequests && id && <AppRequestsPanel appId={id} consumerName="MCP server" transport="mcp" />}
      {activeTab === "requests" && !canReadRequests && <div className="rounded-lg border border-slate-200 bg-white px-6 py-12 text-center text-sm text-slate-500">Request activity access is not available for your account.</div>}
      {activeTab === "sessions" && <McpSessionsPanel sessions={data.recent_sessions || []} />}
    </div>
  );
}

// McpSessionCard keeps long session identifiers and timestamps within the
// mobile viewport while preserving the same status shown by the desktop row.
function McpSessionCard({ session }: { session: NonNullable<McpAnalyticsData["recent_sessions"]>[number] }) {
  const isLive = !session.ended_at;
  return (
    <div className="p-4">
      <div className="flex min-w-0 items-start justify-between gap-3">
        <span className="min-w-0 break-all font-mono text-sm text-slate-700">{session.session_id}</span>
        <SessionStatus live={isLive} />
      </div>
      <dl className="mt-4 grid gap-3 text-sm">
        <div><dt className="text-xs text-slate-500">Started</dt><dd className="mt-0.5 text-slate-700">{new Date(session.started_at).toLocaleString()}</dd></div>
        <div><dt className="text-xs text-slate-500">Ended</dt><dd className="mt-0.5 text-slate-700">{session.ended_at ? new Date(session.ended_at).toLocaleString() : "Not ended"}</dd></div>
      </dl>
    </div>
  );
}

// SessionStatus centralizes the live and disconnected status treatment across
// mobile cards and desktop rows.
function SessionStatus({ live }: { live: boolean }) {
  // Live sessions use the active treatment; completed sessions remain static.
  if (live) {
    return <span className="inline-flex shrink-0 items-center gap-1.5 rounded-full border border-emerald-100 bg-emerald-50 px-2.5 py-1 text-xs font-medium text-emerald-600"><span className="h-1.5 w-1.5 rounded-full bg-emerald-500 motion-safe:animate-pulse" />Live</span>;
  }
  return <span className="inline-flex shrink-0 items-center rounded-full border border-slate-200 bg-slate-100 px-2.5 py-1 text-xs font-medium text-slate-600">Disconnected</span>;
}

// McpSessionsPanel presents session history as mobile cards and retains the
// comparative table at wider breakpoints.
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
          <>
          <div className="divide-y divide-slate-100 md:hidden">
            {sessions.map((session) => <McpSessionCard key={session.id} session={session} />)}
          </div>
          <div className="hidden overflow-x-auto md:block">
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
                      <td className="px-6 py-4 text-right"><SessionStatus live={isLive} /></td>
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
