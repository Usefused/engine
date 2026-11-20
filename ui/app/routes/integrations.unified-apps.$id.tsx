import { UnifiedAppHeaderActions } from "~/components/apps/UnifiedAppHeaderActions";
import { UnifiedAppWebhooks } from "~/components/apps/UnifiedAppWebhooks";
import { unifiedAppTabs, selectedUnifiedAppTab } from "~/lib/unified-app-tabs";
import { UnifiedAppSchedules } from "~/components/apps/UnifiedAppSchedules";
import { useEffect, useState, type ReactNode } from "react";
import { useNavigate, useParams, useSearchParams } from "@remix-run/react";
import { UnifiedAppContract } from "~/components/apps/UnifiedAppContract";
import { UnifiedAppUsage } from "~/components/apps/UnifiedAppUsage";
import { AppConnectedServices } from "~/components/apps/AppConnectedServices";
import { AppExecutionEndpoint } from "~/components/apps/AppExecutionEndpoint";
import { AppExecutionTokens } from "~/components/apps/AppExecutionTokens";
import { Loader2 } from "lucide-react";
import { api } from "~/lib/api";
import { appConnectedServiceSelections, type AppConnectedServiceSelection, type AppServiceSummary } from "~/lib/app-connected-services";
import type { AppSelectionPayload } from "~/lib/app-selection-v3";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasResourcePermission, hasWorkspacePermission } from "~/lib/current-actor-access";
import { AppDetailBackLink, AppDetailHeader, AppDetailTabs } from "~/components/apps/AppDetailChrome";
import { type AppVersionHistoryItem } from "~/components/apps/AppVersionHistory";
import { AppVersionSelect } from "~/components/apps/AppVersionSelect";
import { AppActivityOverview } from "~/components/activity/AppActivityOverview";
import { AppRequestsPanel } from "~/components/activity/AppRequestsPanel";
import { useToast } from "~/components/Toast";

interface UnifiedApp { app_id: string; app_family_id: string; name: string; description: string; version: string; status: string; kind: string; created_at: string; selections: AppSelectionPayload[] }

/** Groups app automation and source actions behind family management and catalogue publication permissions. */
export default function UnifiedAppDetails() {
  const { id: routeID } = useParams();
  const id = String(routeID);
  const [params, setParams] = useSearchParams();
  const navigate = useNavigate();
  const toast = useToast();
  const { access } = useCurrentActorAccess();
  const [app, setApp] = useState<UnifiedApp | null>(null);
  const [services, setServices] = useState<AppServiceSummary[]>([]);
  const [selections, setSelections] = useState<AppConnectedServiceSelection[]>([]);
  const [versions, setVersions] = useState<AppVersionHistoryItem[]>([]);
  const [error, setError] = useState("");
  const [deleting, setDeleting] = useState("");
  const [promoting, setPromoting] = useState("");
  const [traffic, setTraffic] = useState<string | null>(null);

  // Late responses from a sibling version must never replace the current immutable app.
  useEffect(() => {
    let current = true;
    setApp(null); setError(""); setServices([]); setSelections([]); setVersions([]); setTraffic(null);
    /** Loads exact identity before asking for its authorized family history. */
    async function load() {
      try {
        const data = await api.mcpGraphql<{ app: UnifiedApp; appServices: AppServiceSummary[] }>(`query UnifiedAppDetails($id: String!) {
          app(app_id: $id) {
            app_id app_family_id name description version status kind created_at
            selections { mcp_capabilities { kind name operation_id } service_id service_version_id schema_version endpoint_ids operation_names webhook_ids webhook_names select_all webhook_select_all }
          }
          appServices(app_id: $id) { service_id service_name service_slug version select_all endpoint_count webhook_count }
        }`, { id });
        // Other adapters cannot accidentally render as a hosted Unified App.
        if (data.app?.kind !== "unified_app") throw new Error("Unified App not found.");
        const detailedSelections = appConnectedServiceSelections(data.app.selections, data.appServices);
        const history = await api.mcpGraphql<{ appVersions: AppVersionHistoryItem[] }>(`query UnifiedAppVersions($family: String!) {
          appVersions(app_family_id: $family) { id: app_id version created_at }
        }`, { family: data.app.app_family_id });
        const deployment = await api.appConfig.traffic(id);
        // Cleanup owns the stale response boundary for both dependent requests.
        if (current) { setApp(data.app); setServices(data.appServices); setSelections(detailedSelections); setVersions(history.appVersions); setTraffic(deployment.active_app_id); }
      } catch (cause) { if (current) setError(String(cause)); }
    }
    void load();
    return () => { current = false; };
  }, [id]);

  /** Deletes one selected immutable version using the shared app lifecycle endpoint. */
  async function deleteVersion(version: AppVersionHistoryItem) {
    const confirmed = await toast.confirm(`Permanently delete Unified App version ${version.version}? If it receives traffic, execution stops until another version is deployed.`);
    // Cancelled deletion must preserve the active runtime and its historical views.
    if (!confirmed) return;
    setDeleting(version.id);
    try {
      await api.sdks.deactivate(version.id);
      // Removing the viewed immutable resource requires leaving its now-invalid URL.
      if (version.id === id) navigate("/integrations/sdks?type=unified_app");
      else {
        setVersions((items) => items.filter((item) => item.id !== version.id));
        setTraffic(null);
        setTraffic((await api.appConfig.traffic(id)).active_app_id);
      }
    } catch (cause) { toast.error(String(cause)); }
    finally { setDeleting(""); }
  }

  /** Switches new traffic only after the user reviews the exact retained version. */
  async function promoteVersion(version: AppVersionHistoryItem) {
    // Unknown deployment state must not authorize a blind switch.
    if (traffic === null) return;
    const confirmed = await toast.confirm(`Switch new traffic to version ${version.version}? The current version will stop accepting new executions. Clients using an exact version URL must use the selected version's URL.`);
    // Cancelling keeps the serving pointer unchanged.
    if (!confirmed) return;
    setPromoting(version.id);
    try {
      const result = await api.appConfig.promote(version.id, traffic);
      setTraffic(result.active_app_id);
      toast.success(`Version ${version.version} is now receiving traffic.`);
    } catch (cause) {
      toast.error(String(cause));
      // An uncertain response requires fresh state before another user-driven attempt.
      setTraffic(null);
      try { setTraffic((await api.appConfig.traffic(id)).active_app_id); }
      catch { toast.error("Could not refresh traffic status. Reload this page before switching traffic."); }
    } finally { setPromoting(""); }
  }

  /** Keeps the serving badge independent of the version currently being viewed. */
  function versionActions(version: AppVersionHistoryItem) {
    // The active badge represents the authoritative serving pointer, not version ordering.
    if (version.id === traffic) return <span className="rounded-md bg-emerald-50 px-2 py-1 text-xs font-medium text-emerald-700">Receiving traffic</span>;
    // Only family managers can promote a retained version; readiness is checked by Engine.
    if (!canManage) return null;
    return <button type="button" disabled={traffic === null || Boolean(promoting) || Boolean(deleting)} onClick={() => promoteVersion(version)} className="inline-flex min-h-11 items-center justify-center rounded-lg border border-slate-200 bg-white px-4 text-sm font-medium text-slate-700 hover:bg-slate-50 disabled:opacity-50 sm:min-h-8 sm:px-3 sm:text-xs">{promoting === version.id ? "Switching…" : "Make active"}</button>;
  }

  // Failed loading never fabricates an empty successful app or activity summary.
  if (error) return <div className="space-y-4"><AppDetailBackLink to="/integrations/sdks?type=unified_app" /><p role="alert" className="text-red-700">{error}</p></div>;
  if (!app) return <p role="status" className="flex items-center gap-2 text-slate-500"><Loader2 className="h-4 w-4 animate-spin" />Loading Unified App…</p>;
  const canReadActivity = hasResourcePermission(access, "app.unified_app.read", "APP", app.app_family_id) && hasWorkspacePermission(access, "audit.read");
  const canManage = hasResourcePermission(access, "app.unified_app.manage", "APP", app.app_family_id);
  // Publishing requires access to both the private source and catalogue management.
  const canPublishTemplate = canManage && hasWorkspacePermission(access, "catalogue.manage");
  // Historical schemas remain readable, but only the current version offers live invocation controls.
  const executionFamilyID = traffic === id ? app.app_family_id : undefined;
  const tabs = unifiedAppTabs(canManage, canReadActivity);
  // Unknown or denied deep links resolve to Overview without mounting unauthorized consumers.
  const active = selectedUnifiedAppTab(tabs, params.get("tab"));
  return <div className="space-y-6">
    <AppDetailBackLink to="/integrations/sdks?type=unified_app" />
    {/* Private source and successor deployment require management of this exact family. */}
    <AppDetailHeader kind="unified_app" name={app.name} summary={app.description} status={app.status} version={app.version} createdAt={app.created_at} action={<div className="flex shrink-0 items-center gap-2"><UnifiedAppUsage key={id} app={app} executionFamilyID={executionFamilyID} />{canManage && <UnifiedAppHeaderActions key={id} appID={id} canPublishTemplate={canPublishTemplate} />}</div>} />
    <AppDetailTabs label="Unified App details" active={active} tabs={tabs} onChange={(tab) => setParams({ tab })} />
    <UnifiedAppTabBody
      active={active}
      app={app}
      id={id}
      services={services}
      selections={selections}
      versions={versions}
      traffic={traffic}
      busy={[promoting, deleting].some(Boolean)}
      canManage={canManage}
      onSelectVersion={(next) => navigate(`/integrations/unified-apps/${next}?tab=changes`)}
      onDeleteVersion={deleteVersion}
      versionActions={versionActions({ id, version: app.version, created_at: app.created_at })}
    />
  </div>;
}

/** Renders the active Unified App tab so only the selected permission-checked panel mounts its consumers. */
function UnifiedAppTabBody({
  active,
  app,
  id,
  services,
  selections,
  versions,
  traffic,
  busy,
  canManage,
  onSelectVersion,
  onDeleteVersion,
  versionActions,
}: {
  active: string;
  app: UnifiedApp;
  id: string;
  services: AppServiceSummary[];
  selections: AppConnectedServiceSelection[];
  versions: AppVersionHistoryItem[];
  traffic: string | null;
  busy: boolean;
  canManage: boolean;
  onSelectVersion: (next: string) => void;
  onDeleteVersion: (version: AppVersionHistoryItem) => void;
  versionActions: ReactNode;
}) {
  // Each tab branch mounts its own data consumer only when active.
  if (active === "overview") {
    return (
      <div className="space-y-6">
        <section aria-label="API endpoint" className="rounded-xl border border-slate-200 bg-white p-5">
          <div className="flex items-center justify-between gap-3">
            <h2 className="font-semibold text-slate-900">API endpoint</h2>
            <AppExecutionTokens familyID={app.app_family_id} kind="unified_app" />
          </div>
          <AppExecutionEndpoint appID={app.app_family_id} appName={app.name} />
          <p className="mt-2 text-xs text-slate-500">This URL follows the active version. Your URL and tokens stay the same when traffic switches.</p>
        </section>
        <UnifiedAppContract key={id} appID={id} version={app.version} />
        <section aria-label="Connected services" className="space-y-3">
          <h2 className="font-semibold text-slate-900">Connected services</h2>
          <AppConnectedServices selections={selections} />
        </section>
      </div>
    );
  }
  if (active === "analytics") return <AppActivityOverview key={id} appId={id} downloads={null} pendingDriftCount={0} hostedSource services={services} />;
  if (active === "requests") return <AppRequestsPanel key={id} appId={id} consumerName={app.name} />;
  // Webhook management follows current traffic even when viewing a retained version.
  if (active === "webhooks") return <UnifiedAppWebhooks key={traffic || id} appID={traffic || id} active={Boolean(traffic)} />;
  // Scheduling reuses the current family traffic target even when viewing a retained version.
  if (active === "schedules") return <UnifiedAppSchedules key={app.app_family_id} appID={id} />;
  if (active === "changes") return <AppVersionSelect versions={versions} current={{ id, version: app.version, created_at: app.created_at }} activeId={traffic} busy={busy} canDelete={canManage} onSelect={onSelectVersion} onDelete={onDeleteVersion} actions={versionActions} />;
  return null;
}
