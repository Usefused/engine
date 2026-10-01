import { AppExecutionEndpoint } from "~/components/apps/AppExecutionEndpoint";
import { useEffect, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "@remix-run/react";
import { UnifiedAppUsage } from "~/components/apps/UnifiedAppUsage";
import { Loader2, Pencil } from "lucide-react";
import { api } from "~/lib/api";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasResourcePermission, hasWorkspacePermission } from "~/lib/current-actor-access";
import { AppDetailBackLink, AppDetailHeader, AppDetailTabs } from "~/components/apps/AppDetailChrome";
import { type AppVersionHistoryItem } from "~/components/apps/AppVersionHistory";
import { AppVersionSelect } from "~/components/apps/AppVersionSelect";
import { AppActivityOverview } from "~/components/activity/AppActivityOverview";
import { AppRequestsPanel } from "~/components/activity/AppRequestsPanel";
import { useToast } from "~/components/Toast";

interface UnifiedApp { app_id: string; app_family_id: string; name: string; description: string; version: string; status: string; kind: string; created_at: string }
interface AppService { service_id: string; service_name: string; service_slug: string; version: string; endpoint_count: number }
type DetailTab = "overview" | "analytics" | "requests" | "changes";

/** Shares app detail chrome and exposes immutable source editing only to family managers. */
export default function UnifiedAppDetails() {
  const { id: routeID } = useParams();
  const id = String(routeID);
  const [params, setParams] = useSearchParams();
  const navigate = useNavigate();
  const toast = useToast();
  const { access } = useCurrentActorAccess();
  const [app, setApp] = useState<UnifiedApp | null>(null);
  const [services, setServices] = useState<AppService[]>([]);
  const [versions, setVersions] = useState<AppVersionHistoryItem[]>([]);
  const [error, setError] = useState("");
  const [deleting, setDeleting] = useState("");
  const [promoting, setPromoting] = useState("");
  const [traffic, setTraffic] = useState<string | null>(null);

  // Late responses from a sibling version must never replace the current immutable app.
  useEffect(() => {
    let current = true;
    setApp(null); setError(""); setServices([]); setVersions([]); setTraffic(null);
    /** Loads exact identity before asking for its authorized family history. */
    async function load() {
      try {
        const data = await api.mcpGraphql<{ app: UnifiedApp; appServices: AppService[] }>(`query UnifiedAppDetails($id: String!) {
          app(app_id: $id) { app_id app_family_id name description version status kind created_at }
          appServices(app_id: $id) { service_id service_name service_slug version endpoint_count }
        }`, { id });
        // Other adapters cannot accidentally render as a hosted Unified App.
        if (data.app?.kind !== "unified_app") throw new Error("Unified App not found.");
        const history = await api.mcpGraphql<{ appVersions: AppVersionHistoryItem[] }>(`query UnifiedAppVersions($family: String!) {
          appVersions(app_family_id: $family) { id: app_id version created_at }
        }`, { family: data.app.app_family_id });
        const deployment = await api.appConfig.traffic(id);
        // Cleanup owns the stale response boundary for both dependent requests.
        if (current) { setApp(data.app); setServices(data.appServices); setVersions(history.appVersions); setTraffic(deployment.active_app_id); }
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
  const tabs: Array<{ value: DetailTab; label: string }> = [{ value: "overview", label: "Overview" }, { value: "changes", label: "Versions" }];
  // Execution data has its own permission even when identity and versions are readable.
  if (canReadActivity) tabs.splice(1, 0, { value: "analytics", label: "Analytics" }, { value: "requests", label: "Requests" });
  // Unknown or denied deep links resolve to Overview without mounting unauthorized consumers.
  const active = selectedTab(tabs, params.get("tab"));
  return <div className="space-y-6">
    <AppDetailBackLink to="/integrations/sdks?type=unified_app" />
    {/* Private source and successor deployment require management of this exact family. */}
    <AppDetailHeader name={app.name} summary={app.description || "Combine approved services into one typed action for onboarding, fulfillment, or reporting."} status={app.status} version={app.version} createdAt={app.created_at} leadingMetadata={<span className="text-[var(--brand-violet)]">Unified App</span>} action={canManage ? <Link to={`/integrations/unified-apps/new?edit=${encodeURIComponent(id)}`} className="inline-flex min-h-11 items-center justify-center gap-2 rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white hover:bg-slate-800"><Pencil className="h-4 w-4" aria-hidden="true" />Edit app</Link> : null} />
    <AppDetailTabs label="Unified App details" active={active} tabs={tabs} onChange={(tab) => setParams({ tab })} />
    {/* Only the selected permission-checked tab mounts its data consumer. */}
    {active === "overview" && <div className="space-y-6"><UnifiedAppUsage app={app} /><section className="rounded-xl border border-slate-200 bg-white p-5"><h2 className="font-semibold text-slate-900">Execution</h2><p className="mt-2 text-sm text-slate-500">Use this stable app URL with an execution token. Switching traffic changes the version that runs without changing your URL or token.</p><AppExecutionEndpoint appID={app.app_family_id} /></section><section className="overflow-hidden rounded-xl border border-slate-200 bg-white"><h2 className="border-b border-slate-100 px-5 py-4 font-semibold">Connected services</h2>{services.map((service) => <div key={service.service_id} className="flex flex-wrap justify-between gap-2 border-b border-slate-100 px-5 py-4 text-sm"><span className="font-medium">{service.service_name}</span><span className="text-slate-500">{service.version} · {service.endpoint_count} operations</span></div>)}</section></div>}
    {active === "analytics" && <AppActivityOverview key={id} appId={id} downloads={null} pendingDriftCount={0} hostedSource services={services} />}
    {active === "requests" && <AppRequestsPanel key={id} appId={id} consumerName={app.name} />}
    {active === "changes" && <AppVersionSelect versions={versions} current={{ id, version: app.version, created_at: app.created_at }} activeId={traffic} busy={[promoting, deleting].some(Boolean)} canDelete={canManage} onSelect={(next) => navigate(`/integrations/unified-apps/${next}?tab=changes`)} onDelete={deleteVersion} actions={versionActions({ id, version: app.version, created_at: app.created_at })} />}
  </div>;
}

/** Invalid or unauthorized tab URLs fall back without loading protected activity. */
function selectedTab(tabs: Array<{ value: DetailTab }>, requested: string | null): DetailTab { return tabs.find((tab) => tab.value === requested)?.value ?? "overview"; }
