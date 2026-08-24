import { useState, useEffect } from "react";
import { Navigate, useParams, useSearchParams, type MetaFunction } from "@remix-run/react";

// meta retains the workspace chrome metadata while identifying the scoped MCP
// Activity page in the browser title.
export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m) => m.id === "root").flatMap((m) => m.meta ?? []);
  return [
    ...parentMeta.filter((m) => !('title' in m)),
    { title: "MCP server activity - Fused" },
  ];
};
import { Clock, KeyRound } from "lucide-react";
import { api } from "~/lib/api";
import { McpAnalyticsPanel, type McpAnalyticsData } from "~/components/mcp/McpAnalyticsPanel";
import { AppRequestsPanel } from "~/components/activity/AppRequestsPanel";
import { NestedActivityTabs } from "~/components/activity/NestedActivityTabs";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasResourcePermission, hasWorkspacePermission } from "~/lib/current-actor-access";

interface McpActivitySectionProps {
  appId: string;
  appFamilyId: string;
  serverName: string;
}

/** Keeps the historical analytics URL working while Activity moves into details. */
export default function McpAnalyticsRedirect() {
  const { id } = useParams();
  if (!id) return <Navigate to="/integrations/mcp" replace />;
  return <Navigate to={`/integrations/mcp/${id}?tab=activity`} replace />;
}

/** Loads exact-app MCP activity only after family and audit access are both known. */
export function McpActivitySection({ appId, appFamilyId, serverName }: McpActivitySectionProps) {
  const { access, loading: accessLoading } = useCurrentActorAccess();
  const canReadOverview = hasResourcePermission(access, "app.read", "APP", appFamilyId) && hasWorkspacePermission(access, "audit.read");
  const canReadRequests = canReadOverview;
  const [data, setData] = useState<McpAnalyticsData | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [searchParams, setSearchParams] = useSearchParams();
  const activeTab = mcpActivityTab(searchParams.get("activity"));

  // handleTabChange keeps Overview as the canonical URL and records only
  // non-default Activity selections in the query string.
  const handleTabChange = (newTab: McpActivityTab) => {
    setSearchParams(prev => {
      if (newTab === "overview") prev.delete("activity");
      else prev.set("activity", newTab);
      return prev;
    }, { replace: true });
  };

  /** Refreshes analytics only after the exact family access preflight. */
  const fetchAnalytics = () => {
    // Grants attach to the stable family while the query remains scoped to one
    // immutable version, preventing sibling-family requests from being mounted.
    if (!canReadOverview) return;
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
            app_token_id
            session_id
            protocol_version
            started_at
            last_activity_at
            ended_at
            end_reason
          }
          token_activity {
            id
            name
            binding_mode
            status
            issued_by_subject_id
            execution_count
            session_count
            created_at
            last_used_at
            terminated_at
            termination_reason
          }
        }
      }
    `;
    // mcpAnalytics lives on the Engine's own MCP GraphQL schema now
    // (internal/engine/api/mcp_graphql.go), not the Registry-proxied
    // api.graphql this page used to call.
    api.mcpGraphql<{ mcpAnalytics: McpAnalyticsData }>(queryStr, { id: appId })
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
  }, [accessLoading, canReadOverview, appId]);

  return (
    <McpActivityState id={appId} serverName={serverName} data={data} loading={loading || accessLoading} error={error} activeTab={activeTab} onTabChange={handleTabChange} canReadOverview={canReadOverview} canReadRequests={canReadRequests} />
  );
}

type McpActivityTab = "overview" | "requests" | "sessions" | "tokens";

// mcpActivityTab accepts only known Activity sections from the URL.
function mcpActivityTab(value: string | null): McpActivityTab {
  return value === "requests" || value === "sessions" || value === "tokens" ? value : "overview";
}

/** Selects the MCP activity loading, error, or content state. */
function McpActivityState({ id, serverName, data, loading, error, activeTab, onTabChange, canReadOverview, canReadRequests }: {
  id: string;
  serverName: string;
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
  return <McpActivityContent id={id} serverName={serverName} data={data} activeTab={activeTab} onTabChange={onTabChange} canReadRequests={canReadRequests} />;
}

/** Renders MCP activity tabs without mounting protected request data. */
function McpActivityContent({ id, serverName, data, activeTab, onTabChange, canReadRequests }: {
  id: string;
  serverName: string;
  data: McpAnalyticsData;
  activeTab: McpActivityTab;
  onTabChange: (tab: McpActivityTab) => void;
  canReadRequests: boolean;
}) {
  return (
    <div className="min-w-0 max-w-full space-y-5 overflow-x-hidden animate-in fade-in slide-in-from-bottom-4 duration-500 sm:space-y-6">
      <p className="text-sm text-slate-500">
        Requests and sessions are scoped to this immutable server version. Execution token history is scoped to the server family because tokens remain valid across its versions.
      </p>
      <NestedActivityTabs
        active={activeTab}
        ariaLabel="MCP server activity"
        onChange={onTabChange}
        options={[
          { value: "overview", label: "Overview", trackingId: "view_mcp_overview_tab" },
          { value: "requests", label: "Requests", trackingId: "view_mcp_requests_tab" },
          { value: "sessions", label: "Sessions", badge: data.recent_sessions?.length, trackingId: "view_mcp_sessions_tab" },
          { value: "tokens", label: "Tokens", badge: data.token_activity?.length, trackingId: "view_mcp_tokens_tab" },
        ]}
      />

      <McpActivityTabContent activeTab={activeTab} id={id} serverName={serverName} data={data} canReadRequests={canReadRequests} />
    </div>
  );
}

function McpActivityTabContent({ activeTab, id, serverName, data, canReadRequests }: { activeTab: McpActivityTab; id: string; serverName: string; data: McpAnalyticsData; canReadRequests: boolean }) {
  switch (activeTab) {
    case "requests":
      if (!canReadRequests) return <div className="rounded-lg border border-slate-200 bg-white px-6 py-12 text-center text-sm text-slate-500">Request activity access is not available for your account.</div>;
      return <AppRequestsPanel appId={id} consumerName={serverName} transport="mcp" />;
    case "sessions":
      return <McpSessionsPanel sessions={data.recent_sessions || []} />;
    case "tokens":
      return <McpTokenActivityPanel tokens={data.token_activity || []} />;
    default:
      return <McpAnalyticsPanel data={data} />;
  }
}

type McpTokenActivity = NonNullable<McpAnalyticsData["token_activity"]>[number];

function McpTokenStatus({ status }: { status: McpTokenActivity["status"] }) {
  const style = status === "active"
    ? "border-emerald-100 bg-emerald-50 text-emerald-700"
    : status === "expired"
      ? "border-amber-100 bg-amber-50 text-amber-700"
      : "border-rose-100 bg-rose-50 text-rose-700";
  return <span className={`inline-flex rounded-full border px-2.5 py-1 text-xs font-medium capitalize ${style}`}>{status}</span>;
}

function tokenActivityTime(value?: string): string {
  return value ? new Date(value).toLocaleString() : "Never";
}

function tokenTermination(token: McpTokenActivity): string {
  if (token.status === "active") return "—";
  const reason = (token.termination_reason || token.status).replaceAll("_", " ");
  return `${reason} · ${tokenActivityTime(token.terminated_at)}`;
}

function McpTokenActivityCard({ token }: { token: McpTokenActivity }) {
  return (
    <article className="space-y-4 p-4">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <h4 className="truncate text-sm font-semibold text-slate-900">{token.name}</h4>
          <p className="mt-1 truncate font-mono text-xs text-slate-500" title={token.id}>{token.id}</p>
        </div>
        <McpTokenStatus status={token.status} />
      </div>
      <dl className="grid grid-cols-2 gap-3 text-sm">
        <div><dt className="text-xs text-slate-500">Binding</dt><dd className="mt-0.5 capitalize text-slate-700">{token.binding_mode}</dd></div>
        <div><dt className="text-xs text-slate-500">Usage</dt><dd className="mt-0.5 tabular-nums text-slate-700">{token.execution_count} calls · {token.session_count} sessions</dd></div>
        <div><dt className="text-xs text-slate-500">Issued</dt><dd className="mt-0.5 text-slate-700">{tokenActivityTime(token.created_at)}</dd></div>
        <div><dt className="text-xs text-slate-500">Last used</dt><dd className="mt-0.5 text-slate-700">{tokenActivityTime(token.last_used_at)}</dd></div>
      </dl>
      {token.issued_by_subject_id && <p className="truncate text-xs text-slate-500" title={token.issued_by_subject_id}>Issued by {token.issued_by_subject_id}</p>}
      {token.status !== "active" && <p className="text-xs text-slate-500">Ended: <span className="font-medium capitalize text-slate-700">{tokenTermination(token)}</span></p>}
    </article>
  );
}

function McpTokenActivityPanel({ tokens }: { tokens: McpTokenActivity[] }) {
  return (
    <section className="overflow-hidden rounded-xl border border-slate-200 bg-white shadow-sm">
      <header className="flex items-center gap-2 border-b border-slate-100 px-5 py-4">
        <KeyRound className="h-4 w-4 text-blue-500" />
        <div>
          <h3 className="font-semibold text-slate-900">Execution token activity</h3>
          <p className="mt-0.5 text-xs text-slate-500">Credential-free issue, use, expiry, and revocation history.</p>
        </div>
      </header>
      {tokens.length === 0 ? <div className="px-6 py-12 text-center text-sm text-slate-500">No execution tokens recorded yet.</div> : <McpTokenActivityRows tokens={tokens} />}
    </section>
  );
}

function McpTokenActivityRows({ tokens }: { tokens: McpTokenActivity[] }) {
  return (
    <>
      <div className="divide-y divide-slate-100 md:hidden">{tokens.map((token) => <McpTokenActivityCard key={token.id} token={token} />)}</div>
      <div className="hidden overflow-x-auto md:block">
        <table className="w-full min-w-[900px] text-left text-sm">
          <thead className="bg-slate-50/50 text-xs uppercase tracking-wider text-slate-500">
            <tr><th className="px-5 py-3 font-medium">Token</th><th className="px-5 py-3 font-medium">Status</th><th className="px-5 py-3 font-medium">Binding</th><th className="px-5 py-3 text-right font-medium">Executions</th><th className="px-5 py-3 text-right font-medium">Sessions</th><th className="px-5 py-3 font-medium">Last used</th><th className="px-5 py-3 font-medium">Ended</th><th className="px-5 py-3 font-medium">Issued by</th></tr>
          </thead>
          <tbody className="divide-y divide-slate-100 text-slate-700">{tokens.map((token) => <McpTokenActivityTableRow key={token.id} token={token} />)}</tbody>
        </table>
      </div>
    </>
  );
}

function McpTokenActivityTableRow({ token }: { token: McpTokenActivity }) {
  return (
    <tr className="hover:bg-slate-50">
      <td className="px-5 py-4"><div className="font-medium text-slate-900">{token.name}</div><div className="mt-1 max-w-44 truncate font-mono text-xs text-slate-400" title={token.id}>{token.id}</div></td>
      <td className="px-5 py-4"><McpTokenStatus status={token.status} /></td>
      <td className="px-5 py-4 capitalize">{token.binding_mode}</td>
      <td className="px-5 py-4 text-right tabular-nums">{token.execution_count}</td>
      <td className="px-5 py-4 text-right tabular-nums">{token.session_count}</td>
      <td className="whitespace-nowrap px-5 py-4">{tokenActivityTime(token.last_used_at)}</td>
      <td className="whitespace-nowrap px-5 py-4 capitalize">{tokenTermination(token)}</td>
      <td className="max-w-52 truncate px-5 py-4 font-mono text-xs" title={token.issued_by_subject_id}>{token.issued_by_subject_id || "Unknown"}</td>
    </tr>
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
      <dl className="mt-4 grid grid-cols-2 gap-3 text-sm">
        <div><dt className="text-xs text-slate-500">Started</dt><dd className="mt-0.5 text-slate-700">{new Date(session.started_at).toLocaleString()}</dd></div>
        <div><dt className="text-xs text-slate-500">Ended</dt><dd className="mt-0.5 text-slate-700">{session.ended_at ? new Date(session.ended_at).toLocaleString() : "Not ended"}</dd></div>
        <div><dt className="text-xs text-slate-500">Protocol</dt><dd className="mt-0.5 font-mono text-slate-700">{session.protocol_version}</dd></div>
        <div><dt className="text-xs text-slate-500">Last activity</dt><dd className="mt-0.5 text-slate-700">{tokenActivityTime(session.last_activity_at)}</dd></div>
      </dl>
      {session.app_token_id && <p className="truncate font-mono text-xs text-slate-500" title={session.app_token_id}>Token {session.app_token_id}</p>}
      {session.end_reason && <p className="text-xs text-slate-500">Ended because: <span className="font-medium text-slate-700">{session.end_reason.replaceAll("_", " ")}</span></p>}
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
            <table className="w-full min-w-[1050px] text-sm text-left">
              <thead className="bg-slate-50/50 text-slate-500 text-xs uppercase tracking-wider">
                <tr>
                  <th className="px-6 py-3 font-medium">Session ID</th>
                  <th className="px-6 py-3 font-medium">Token ID</th>
                  <th className="px-6 py-3 font-medium">Protocol</th>
                  <th className="px-6 py-3 font-medium">Started At</th>
                  <th className="px-6 py-3 font-medium">Last Activity</th>
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
                      <td className="max-w-48 truncate px-6 py-4 font-mono text-xs text-slate-500" title={sess.app_token_id}>{sess.app_token_id || "Legacy"}</td>
                      <td className="px-6 py-4 font-mono text-xs text-slate-600">{sess.protocol_version}</td>
                      <td className="px-6 py-4">{new Date(sess.started_at).toLocaleString()}</td>
                      <td className="px-6 py-4">{tokenActivityTime(sess.last_activity_at)}</td>
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
