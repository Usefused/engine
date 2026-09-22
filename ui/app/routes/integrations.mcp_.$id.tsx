import { useEffect, useState } from "react";
import { useNavigate, useParams, useSearchParams, type MetaFunction } from "@remix-run/react";
import { Copy } from "lucide-react";
import { AppDetailBackLink, AppDetailHeader, AppDetailPrimaryAction, AppDetailTabs, AppVersionSwitcher, type AppDetailTab } from "~/components/apps/AppDetailChrome";
import { AppChangesBody, AppDetailBody, AppDetailSection, AppOverviewBody } from "~/components/apps/AppDetailBody";
import { type AppConnectedServiceSelection } from "~/components/apps/AppConnectedServices";
import { type AppVersionHistoryItem } from "~/components/apps/AppVersionHistory";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { McpTransportEndpoints, McpVersionTransportEndpoints, type McpTransportEndpointData, type McpTransportName, type McpTransportURLs } from "~/components/mcp/McpTransportEndpoints";
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

type McpDetailTab = "overview" | "activity" | "changes";

interface McpServerDetail extends McpTransportEndpointData {
  fused_intelligent_classifier?: boolean;
  app_id: string;
  app_family_id: string;
  name: string;
  version: string;
  kind: string;
  status: string;
  created_at?: string;
  selections: AppSelectionPayload[];
  detailed_selections: AppConnectedServiceSelection[];
}

interface McpVersion extends AppVersionHistoryItem {
  status: string;
  transport_urls?: McpTransportURLs | null;
}

/** Accepts only detail tabs owned by the MCP page. */
function mcpDetailTab(value: string | null): McpDetailTab {
  // Only the two non-default tabs are accepted from URL state.
  return value === "activity" || value === "changes" ? value : "overview";
}

/** Loads one exact immutable MCP version and all service labels in one GraphQL request. */
function readMcpDetails(appId: string): Promise<McpServerDetail> {
  const document = `
    query MCPServerDetails($appId: String!) {
      app(app_id: $appId) {
        app_id
        app_family_id
        name
        fused_intelligent_classifier
        version
        kind
        status
        created_at
        default_transport
        stable
        stable_version_id
        transport_urls { streamable_http sse versioned_streamable_http versioned_sse }
        selections { service_id service_version_id schema_version endpoint_ids operation_names webhook_ids webhook_names select_all webhook_select_all }
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

/** Loads every immutable family version with Engine-projected pinned transport URLs. */
function readMcpVersions(appFamilyId: string): Promise<McpVersion[]> {
  return api.mcpGraphql<{ appVersions: McpVersion[] }>(`
    query MCPServerVersions($appFamilyId: String!) {
      appVersions(app_family_id: $appFamilyId) {
        id: app_id
        version
        status
        created_at
        transport_urls { versioned_streamable_http versioned_sse }
      }
    }
  `, { appFamilyId }).then(({ appVersions }) => appVersions);
}

/** Selects the stable family URL so exact-version addresses remain in History. */
function mcpPrimaryURL(server: McpServerDetail): string {
  const value = server.transport_urls?.streamable_http;
  // The browser must never synthesize an endpoint when Engine omitted its authoritative projection.
  return typeof value === "string" ? value.trim() : "";
}

/** Keeps pinned URLs copyable only while their immutable runtime remains executable. */
function mcpVersionRunnable(status: string): boolean {
  // Deprecated versions stay executable until hard deactivation, exactly like active versions.
  return status === "active" || status === "deprecated";
}

/** Builds only the MCP detail tabs the current actor is authorized to open. */
function mcpDetailTabs(canReadActivity: boolean): Array<AppDetailTab<McpDetailTab>> {
  const tabs: Array<AppDetailTab<McpDetailTab>> = [{ value: "overview", label: "Overview" }];
  // Activity must stay undiscoverable when the actor lacks both app and audit reads.
  if (canReadActivity) tabs.push({ value: "activity", label: "Activity" });
  tabs.push({ value: "changes", label: "Changes" });
  return tabs;
}

/** Shows MCP-only transport identities before the shared connected-service section. */
function McpOverviewDetails({ server, onCopied }: { server: McpServerDetail; onCopied: (transport: McpTransportName) => void }) {
  // Deprecated versions remain runnable until their scheduled hard deactivation.
  const enabled = server.status === "active" || server.status === "deprecated";
  return (
    <AppDetailSection title="Connection endpoints">
      <McpTransportEndpoints endpoints={server} enabled={enabled} onCopied={onCopied} />
    </AppDetailSection>
  );
}

/** Gives each copied stable, pinned, or legacy route an unambiguous toast label. */
function mcpTransportLabel(transport: McpTransportName): string {
  const labels: Record<McpTransportName, string> = {
    streamable_http: "Stable Streamable HTTP",
    versioned_streamable_http: "Version-pinned Streamable HTTP",
    sse: "Stable SSE",
    versioned_sse: "Version-pinned SSE",
  };
  return labels[transport];
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

  /** Removes one deleted immutable version from the loaded family without refetching unchanged details. */
  const removeVersion = (versionId: string) => setVersions((current) => current.filter((version) => version.id !== versionId));

  return { server, versions, loading, error, removeVersion };
}

/** Applies one primary tab change without leaving stale nested Activity state. */
function updateMcpDetailTab(current: URLSearchParams, tab: McpDetailTab): URLSearchParams {
  // Overview is the canonical URL and therefore owns no explicit tab parameter.
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
  const [deletingVersionId, setDeletingVersionId] = useState("");
  const state = useMcpServerDetail(id);

  const canReadActivity = Boolean(state.server) && hasResourcePermission(access, "app.mcp.read", "APP", state.server?.app_family_id ?? "") && hasWorkspacePermission(access, "audit.read");
  const canManageVersions = Boolean(state.server) && hasResourcePermission(access, "app.mcp.manage", "APP", state.server?.app_family_id ?? "");
  const requestedTab = mcpDetailTab(searchParams.get("tab"));
  const activeTab = requestedTab === "activity" && !canReadActivity ? "overview" : requestedTab;
  const setActiveTab = (tab: McpDetailTab) => setSearchParams((current) => updateMcpDetailTab(current, tab), { replace: true });

  /** Permanently removes one exact MCP version and keeps Changes open on a surviving sibling. */
  const handleDeleteVersion = async (version: McpVersion) => {
    // A loaded exact server is required to scope confirmation copy and fallback navigation.
    if (!state.server || !id) return;
    const confirmed = await toast.confirm(`Delete MCP server "${state.server.name}" version "${version.version}"? This permanently removes its runtime.`);
    // Cancellation must not alter the family history or active transport route.
    if (!confirmed) return;
    setDeletingVersionId(version.id);
    try {
      await api.sdks.deactivate(version.id);
      const remaining = state.versions.filter((candidate) => candidate.id !== version.id);
      toast.success(`MCP server version "${version.version}" deleted.`);
      // The deleted current resource cannot remain mounted after hard deactivation.
      if (version.id === id) {
        // Preserve the family Changes workflow when a sibling survives; otherwise return to the MCP catalogue.
        if (remaining.length > 0) navigate(`/integrations/mcp/${remaining[0].id}?tab=changes`);
        else navigate("/integrations/sdks");
        return;
      }
      state.removeVersion(version.id);
    } catch (cause) {
      toast.error(`Failed to delete MCP server version: ${cause instanceof Error ? cause.message : "Unknown error"}`);
    } finally {
      setDeletingVersionId("");
    }
  };

  return <McpDetailState id={id} state={state} activeTab={activeTab} canReadActivity={canReadActivity} canManageVersions={canManageVersions} deletingVersionId={deletingVersionId} onNavigate={navigate} onTabChange={setActiveTab} onDeleteVersion={handleDeleteVersion} onCopyPrimary={(url) => { navigator.clipboard.writeText(url); toast.success("Stable Streamable HTTP URL copied to clipboard!"); }} onCopied={(transport) => toast.success(`${mcpTransportLabel(transport)} URL copied to clipboard!`)} />;
}

/** Selects the bounded loading, failure, or immutable-version detail surface. */
function McpDetailState({ id, state, activeTab, canReadActivity, canManageVersions, deletingVersionId, onNavigate, onTabChange, onDeleteVersion, onCopyPrimary, onCopied }: {
  id?: string;
  state: ReturnType<typeof useMcpServerDetail>;
  activeTab: McpDetailTab;
  canReadActivity: boolean;
  canManageVersions: boolean;
  deletingVersionId: string;
  onNavigate: (path: string) => void;
  onTabChange: (tab: McpDetailTab) => void;
  onDeleteVersion: (version: McpVersion) => void;
  onCopyPrimary: (url: string) => void;
  onCopied: (transport: McpTransportName) => void;
}) {
  // Loading and failure states must not mount version lifecycle controls without an authorized family.
  if (state.loading) return <div className="flex flex-col items-center justify-center py-20 text-slate-500"><div className="mb-4 h-8 w-8 animate-spin rounded-full border-4 border-blue-500 border-t-transparent" />Loading MCP server details...</div>;
  if (state.error || !state.server || !id) return <div className="space-y-6"><AppDetailBackLink to="/integrations/sdks" /><div className="rounded-lg border border-red-200 bg-red-50 p-4 text-red-700">{state.error || "MCP server not found"}</div></div>;
  return <McpLoadedContent id={id} server={state.server} versions={state.versions} activeTab={activeTab} canReadActivity={canReadActivity} canManageVersions={canManageVersions} deletingVersionId={deletingVersionId} onNavigate={onNavigate} onTabChange={onTabChange} onDeleteVersion={onDeleteVersion} onCopyPrimary={onCopyPrimary} onCopied={onCopied} />;
}

/** Renders one loaded MCP version while routing transport copy events through fixed labels. */
function McpLoadedContent({ id, server, versions, activeTab, canReadActivity, canManageVersions, deletingVersionId, onNavigate, onTabChange, onDeleteVersion, onCopyPrimary, onCopied }: {
  id: string;
  server: McpServerDetail;
  versions: McpVersion[];
  activeTab: McpDetailTab;
  canReadActivity: boolean;
  canManageVersions: boolean;
  deletingVersionId: string;
  onNavigate: (path: string) => void;
  onTabChange: (tab: McpDetailTab) => void;
  onDeleteVersion: (version: McpVersion) => void;
  onCopyPrimary: (url: string) => void;
  onCopied: (transport: McpTransportName) => void;
}) {
  const primaryURL = mcpPrimaryURL(server);

  return (
    <div className="min-w-0 space-y-6">
      <AppDetailBackLink to="/integrations/sdks" />
      {/* The header action copies the stable family URL; immutable routes belong to Changes. */}
      <AppDetailHeader
        name={server.name}
        summary="A reusable interface for the services and operations this MCP server exposes."
        status={server.status}
        version={server.version}
        createdAt={server.created_at}
        action={primaryURL ? <AppDetailPrimaryAction icon={<Copy className="h-4 w-4" />} label="Copy server URL" onClick={() => onCopyPrimary(primaryURL)} /> : null}
      />

      <AppVersionSwitcher label="App version" versions={versions} currentId={id} onSelect={(appId) => onNavigate(`/integrations/mcp/${appId}`)} />

      <AppDetailTabs label="App details" active={activeTab} tabs={mcpDetailTabs(canReadActivity)} onChange={onTabChange} />

      <AppDetailBody>
        {/* Disclosure follows this exact immutable version, including versions created through CLI. */}
        {server.fused_intelligent_classifier === true && <p className="mb-4 text-sm text-slate-600">Intelligent search uses Jev through Fused Registry. Search intent and authorized operation names and descriptions are sent to Jev. Fused manages the Jev API key; no additional key is required.</p>}
        {/* MCP contributes only its transport controls; service selection remains shared. */}
        {activeTab === "overview" ? (
          <AppOverviewBody
            selections={server.detailed_selections}
            adapterDetails={<McpOverviewDetails server={server} onCopied={onCopied} />}
          />
        ) : null}
        {activeTab === "activity" && canReadActivity ? <McpActivitySection appId={server.app_id} appFamilyId={server.app_family_id} serverName={server.name} /> : null}
        {activeTab === "changes" ? (
          // Version detail navigation leaves History so selecting the already-open version still has a visible result.
          <AppChangesBody
            versions={versions}
            currentId={id}
            canDelete={canManageVersions}
            deletingVersionId={deletingVersionId}
            onSelect={(appId) => onNavigate(`/integrations/mcp/${appId}`)}
            onDelete={onDeleteVersion}
            renderDetails={(version) => (
              <McpVersionTransportEndpoints
                endpoints={{ transport_urls: version.transport_urls }}
                enabled={mcpVersionRunnable(version.status)}
                onCopied={onCopied}
              />
            )}
          />
        ) : null}
      </AppDetailBody>
    </div>
  );
}
