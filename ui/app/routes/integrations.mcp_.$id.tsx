import { useEffect, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams, type MetaFunction } from "@remix-run/react";
import { ArrowLeft, TerminalSquare } from "lucide-react";
import { AppRuntimeStatus } from "~/components/apps/AppRuntimeStatus";
import { AppConnectedServices, type AppConnectedServiceSelection } from "~/components/apps/AppConnectedServices";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { McpTransportEndpoints, type McpTransportEndpointData } from "~/components/mcp/McpTransportEndpoints";
import { useToast } from "~/components/Toast";
import { api } from "~/lib/api";
import { appConnectedServiceSelections, type AppServiceSummary } from "~/lib/app-connected-services";
import type { AppSelectionPayload } from "~/lib/app-selection-v3";
import { hasResourcePermission, hasWorkspacePermission } from "~/lib/current-actor-access";
import { McpActivitySection } from "~/routes/integrations.mcp_.$id.analytics";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((match) => match.id === "root").flatMap((match) => match.meta ?? []);
  return [...parentMeta.filter((item) => !("title" in item)), { title: "MCP server details - Fused" }];
};

type McpDetailTab = "overview" | "activity";

interface McpServerDetail extends McpTransportEndpointData {
  app_id: string;
  app_family_id: string;
  name: string;
  description?: string;
  version: string;
  kind: string;
  status: string;
  created_at?: string;
  selections: AppSelectionPayload[];
  detailed_selections: AppConnectedServiceSelection[];
}

interface McpVersion {
  app_id: string;
  version: string;
  created_at: string;
}

/** Accepts only detail tabs owned by the MCP page. */
function mcpDetailTab(value: string | null): McpDetailTab {
  return value === "activity" ? "activity" : "overview";
}

function detailTabClass(active: boolean): string {
  const tone = active ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700";
  return `shrink-0 rounded-md px-4 py-1.5 text-sm font-medium transition-all ${tone}`;
}

/** Loads one exact immutable MCP version and all service labels in one GraphQL request. */
function readMcpDetails(appId: string): Promise<McpServerDetail> {
  const document = `
    query MCPServerDetails($appId: String!) {
      app(app_id: $appId) {
        app_id
        app_family_id
        name
        description
        version
        kind
        status
        created_at
        default_transport
        transport_urls { streamable_http sse }
        selections { service_id service_version_id definition_schema_version endpoint_ids operation_names webhook_ids webhook_names select_all webhook_select_all }
      }
      appServices(app_id: $appId) { service_id service_slug service_name version select_all endpoint_count webhook_count }
    }
  `;
  return api.mcpGraphql<{ app: Omit<McpServerDetail, "detailed_selections">; appServices: AppServiceSummary[] }>(document, { appId }).then((result) => {
    // Kind validation prevents an SDK identifier from being rendered through
    // MCP controls even though both adapters share the app catalogue query.
    if (result.app.kind !== "mcp") throw new Error("MCP server not found");
    return { ...result.app, detailed_selections: appConnectedServiceSelections(result.app.selections, result.appServices) };
  });
}

function readMcpVersions(appFamilyId: string): Promise<McpVersion[]> {
  return api.mcpGraphql<{ appVersions: McpVersion[] }>(`
    query MCPServerVersions($appFamilyId: String!) {
      appVersions(app_family_id: $appFamilyId) { app_id version created_at }
    }
  `, { appFamilyId }).then(({ appVersions }) => appVersions);
}

function McpVersionSwitcher({ versions, currentId, onSelect }: { versions: McpVersion[]; currentId: string; onSelect: (id: string) => void }) {
  if (versions.length <= 1) return null;
  return (
    <div className="flex flex-wrap items-center gap-2">
      <span className="mr-1 text-xs font-medium uppercase tracking-wider text-slate-500">Server version</span>
      <div className="flex flex-wrap gap-0.5 rounded-lg bg-slate-100/80 p-1">
        {versions.map((version) => <button key={version.app_id} type="button" onClick={() => onSelect(version.app_id)} className={`rounded-md px-3 py-1 text-xs font-medium transition-all ${version.app_id === currentId ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700"}`}>{version.version}</button>)}
      </div>
    </div>
  );
}

function McpOverview({ server, onCopied }: { server: McpServerDetail; onCopied: (transport: "streamable_http" | "sse") => void }) {
  const enabled = server.status === "active" || server.status === "deprecated";
  return (
    <div className="space-y-7">
      <section>
        <h2 className="mb-3 text-sm font-semibold uppercase tracking-wider text-slate-700">Connection endpoints</h2>
        <McpTransportEndpoints endpoints={server} enabled={enabled} onCopied={onCopied} />
      </section>
      <section>
        <h2 className="mb-3 text-sm font-semibold uppercase tracking-wider text-slate-700">Connected services</h2>
        <AppConnectedServices selections={server.detailed_selections} />
      </section>
    </div>
  );
}

/** Owns exact-version loading so the route component stays presentation-only. */
function useMcpServerDetail(appId?: string) {
  const [server, setServer] = useState<McpServerDetail | null>(null);
  const [versions, setVersions] = useState<McpVersion[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  useEffect(() => {
    if (!appId) return;
    setLoading(true);
    setError("");
    readMcpDetails(appId)
      .then((value) => {
        setServer(value);
        document.title = `${value.name} - Fused`;
        return readMcpVersions(value.app_family_id);
      })
      .then(setVersions)
      .catch((cause) => setError(cause instanceof Error ? cause.message : "Failed to load MCP server"))
      .finally(() => setLoading(false));
  }, [appId]);

  return { server, versions, loading, error };
}

/** Applies one primary tab change without leaving stale nested Activity state. */
function updateMcpDetailTab(current: URLSearchParams, tab: McpDetailTab): URLSearchParams {
  if (tab === "overview") {
    current.delete("tab");
    current.delete("activity");
    return current;
  }
  current.set("tab", tab);
  return current;
}

/** Renders MCP identity, immutable selections, transports, and scoped Activity together. */
export default function McpServerDetails() {
  const { id } = useParams();
  const navigate = useNavigate();
  const toast = useToast();
  const { access } = useCurrentActorAccess();
  const [searchParams, setSearchParams] = useSearchParams();
  const state = useMcpServerDetail(id);

  const canReadActivity = Boolean(state.server) && hasResourcePermission(access, "app.read", "APP", state.server?.app_family_id ?? "") && hasWorkspacePermission(access, "audit.read");
  const requestedTab = mcpDetailTab(searchParams.get("tab"));
  const activeTab = requestedTab === "activity" && !canReadActivity ? "overview" : requestedTab;
  const setActiveTab = (tab: McpDetailTab) => setSearchParams((current) => updateMcpDetailTab(current, tab), { replace: true });

  return <McpDetailState id={id} state={state} activeTab={activeTab} canReadActivity={canReadActivity} onNavigate={navigate} onTabChange={setActiveTab} onCopied={(transport) => toast.success(`${transport === "streamable_http" ? "Streamable HTTP" : "SSE"} URL copied to clipboard!`)} />;
}

function McpDetailState({ id, state, activeTab, canReadActivity, onNavigate, onTabChange, onCopied }: {
  id?: string;
  state: ReturnType<typeof useMcpServerDetail>;
  activeTab: McpDetailTab;
  canReadActivity: boolean;
  onNavigate: (path: string) => void;
  onTabChange: (tab: McpDetailTab) => void;
  onCopied: (transport: "streamable_http" | "sse") => void;
}) {
  if (state.loading) return <div className="flex flex-col items-center justify-center py-20 text-slate-500"><div className="mb-4 h-8 w-8 animate-spin rounded-full border-4 border-blue-500 border-t-transparent" />Loading MCP server details...</div>;
  if (state.error || !state.server || !id) return <div className="space-y-6"><Link to="/integrations/mcp" className="inline-flex items-center text-sm text-slate-500 hover:text-slate-800"><ArrowLeft className="mr-2 h-4 w-4" />Back to MCP servers</Link><div className="rounded-lg border border-red-200 bg-red-50 p-4 text-red-700">{state.error || "MCP server not found"}</div></div>;
  return <McpLoadedContent id={id} server={state.server} versions={state.versions} activeTab={activeTab} canReadActivity={canReadActivity} onNavigate={onNavigate} onTabChange={onTabChange} onCopied={onCopied} />;
}

function McpLoadedContent({ id, server, versions, activeTab, canReadActivity, onNavigate, onTabChange, onCopied }: {
  id: string;
  server: McpServerDetail;
  versions: McpVersion[];
  activeTab: McpDetailTab;
  canReadActivity: boolean;
  onNavigate: (path: string) => void;
  onTabChange: (tab: McpDetailTab) => void;
  onCopied: (transport: "streamable_http" | "sse") => void;
}) {

  return (
    <div className="min-w-0 space-y-6">
      <Link to="/integrations/mcp" className="inline-flex items-center text-sm text-slate-500 hover:text-slate-800"><ArrowLeft className="mr-2 h-4 w-4" />Back to MCP servers</Link>
      <header className="flex min-w-0 items-start gap-3">
        <div className="mt-0.5 flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-violet-100 text-violet-700"><TerminalSquare className="h-5 w-5" /></div>
        <div className="min-w-0">
          <h1 className="break-words text-2xl font-bold text-slate-900">{server.name}</h1>
          <p className="mt-1 text-slate-500">{server.description || "Services and operations exposed to agents through this MCP server."}</p>
          <AppRuntimeStatus className="mt-1.5" status={server.status} />
          <div className="mt-3 flex flex-wrap items-center gap-3 text-sm text-slate-600"><span className="rounded bg-slate-100 px-2.5 py-1 font-medium text-slate-700">{server.version}</span><span>Created {server.created_at ? new Date(server.created_at).toLocaleDateString() : ""}</span></div>
        </div>
      </header>

      <McpVersionSwitcher versions={versions} currentId={id} onSelect={(appId) => onNavigate(`/integrations/mcp/${appId}`)} />

      <nav aria-label="MCP server details" className="flex max-w-full overflow-x-auto whitespace-nowrap rounded-lg bg-slate-100/80 p-1 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
        <button type="button" onClick={() => onTabChange("overview")} className={detailTabClass(activeTab === "overview")}>Overview</button>
        {canReadActivity ? <button type="button" onClick={() => onTabChange("activity")} className={detailTabClass(activeTab === "activity")}>Activity</button> : null}
      </nav>

      {activeTab === "overview" ? <McpOverview server={server} onCopied={onCopied} /> : null}
      {activeTab === "activity" && canReadActivity ? <McpActivitySection appId={server.app_id} appFamilyId={server.app_family_id} serverName={server.name} /> : null}
    </div>
  );
}
