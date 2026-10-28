import { IntegrationSourceTabs } from "~/components/layout/IntegrationSourceTabs";
import { CataloguePageHeader, catalogueActionClassName } from "~/components/layout/CataloguePageHeader";
import { useEffect, useState } from "react";
import { Link, type MetaFunction } from "@remix-run/react";
import { Cable, ChevronRight, Download, Loader2, RefreshCw, Unlink } from "lucide-react";
import { WorkspacePermissionGate, useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { useToast } from "~/components/Toast";
import { hasWorkspacePermission } from "~/lib/current-actor-access";
import { api, type BridgeInstallation } from "~/lib/api";

/** Identify the Bridge catalogue within integration sources. */
export const meta: MetaFunction = () => [{ title: "Bridges - Fused" }];

const secondaryButton = "inline-flex min-h-11 items-center justify-center gap-2 rounded-lg border border-slate-300 bg-white px-3 text-sm font-medium text-slate-700 hover:bg-slate-50 disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-blue-500";

/** Share the official download destination while keeping the current Engine page open. */
function BridgeDownloadLink() {
  return <a href="https://github.com/Usefused/bridge/pkgs/container/fused-bridge" target="_blank" rel="noopener noreferrer" className={catalogueActionClassName} aria-label="Download Fused Bridge (opens in a new tab)">
    <Download className="h-3.5 w-3.5" aria-hidden="true" />
    <span>Download<span className="hidden sm:inline"> Fused Bridge</span></span>
  </a>;
}

/** Keep installation discovery behind the existing workspace permission boundary. */
export default function BridgesPage() {
  const { access } = useCurrentActorAccess();
  return <>
    <IntegrationSourceTabs />
    <WorkspacePermissionGate permission="bridge.read" area="paired Bridges">
      <BridgeManager canManage={hasWorkspacePermission(access, "bridge.manage")} />
    </WorkspacePermissionGate>
  </>;
}

/** Use the shared Services header with a download entry point for both new and existing Bridge users. */
function BridgeManager({ canManage }: { canManage: boolean }) {
  const toast = useToast();
  const [items, setItems] = useState<BridgeInstallation[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [revoking, setRevoking] = useState("");
  const [revision, setRevision] = useState(0);

  useEffect(/** Ignore responses belonging to a previous refresh or unmounted page. */ () => {
    let active = true;
    setLoading(true);
    setError("");
    api.bridges.list().then(/** Only the current request may replace the visible catalogue. */ result => {
      // A late response must not undo a newer revocation or navigation.
      if (active) setItems(result.bridges);
    }).catch(/** Distinguish a failed read from an empty workspace. */ () => {
      // Unmounted views must not publish obsolete errors.
      if (active) setError("Unable to load Bridges. Refresh to try again.");
    }).finally(/** Settle loading only for the request the page still owns. */ () => {
      // A newer request owns its own loading indicator.
      if (active) setLoading(false);
    });
    return /** Invalidate pending promises when leaving this request. */ () => { active = false; };
  }, [revision]);

  /** Match the existing Access confirmation and toast flow for revoking an installation. */
  async function revoke(item: BridgeInstallation) {
    const confirmed = await toast.confirm(`Disconnect "${item.name}"? Apps will immediately lose access to its profiles and operations.`);
    // Cancellation preserves the live workspace connection.
    if (!confirmed) return;
    setRevoking(item.pairing_id);
    try {
      await api.bridges.revoke(item.pairing_id);
      setRevision(/** Refresh from the latest counter after an asynchronous mutation. */ value => value + 1);
      toast.success("Bridge disconnected.");
    } catch {
      toast.error("Unable to disconnect this Bridge. Refresh and try again.");
    } finally {
      setRevoking("");
    }
  }

  return <div className="min-w-0 space-y-6">
    <CataloguePageHeader title="Bridges" icon={Cable} description="Connect hardware and services through Fused Bridge, including non-web protocols." actions={<BridgeDownloadLink />} />

    <section aria-label="Connected Bridges" className="min-w-0 overflow-hidden rounded-xl border border-slate-200 bg-white shadow-sm">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-slate-200 px-4 py-3 sm:px-5">
        <h2 className="font-semibold text-slate-900">Connected Bridges</h2>
        <button type="button" className={secondaryButton} disabled={loading} onClick={/** Request a fresh authoritative list without changing selection. */ () => setRevision(revision + 1)}>
          <RefreshCw className={`h-4 w-4 ${loading ? "animate-spin motion-reduce:animate-none" : ""}`} aria-hidden="true" /> Refresh
        </button>
      </div>
      {/* Loading and failures must never masquerade as an empty connection list. */}
      {loading && <p role="status" className="flex items-center gap-2 p-5 text-sm text-slate-500"><Loader2 className="h-4 w-4 animate-spin motion-reduce:animate-none" aria-hidden="true" /> Loading Bridges…</p>}
      {!loading && error && <p role="alert" className="p-5 text-sm text-rose-700">{error}</p>}
      {!loading && !error && items.length === 0 && <BridgeEmptyState />}
      {!loading && !error && <div className="divide-y divide-slate-100">{items.map(/** Pairing identity keeps profile links attached to the correct installation. */ item => <BridgeRow
        key={item.pairing_id} item={item} canManage={canManage} busy={revoking !== ""}
        onRevoke={/** Keep the row action bound to the reviewed installation. */ () => void revoke(item)}
      />)}</div>}
    </section>
    <p className="text-sm leading-6 text-slate-500">Pair from <span className="font-medium text-slate-700">Fused Bridge Studio → Fused</span>. Profiles appear automatically; each app chooses which operations it can use.</p>
  </div>;
}

/** Give first-time users a download action before explaining how their Bridge appears here. */
function BridgeEmptyState() {
  return <div className="flex flex-col items-center px-5 py-10 text-center">
    <span className="mb-4 rounded-2xl bg-slate-100 p-3 text-slate-400"><Cable className="h-6 w-6" aria-hidden="true" /></span>
    <h3 className="text-sm font-semibold text-slate-900">No Bridges connected yet</h3>
    <p className="mt-2 max-w-sm text-sm leading-6 text-slate-500">Download and run Fused Bridge, then connect it to this workspace from Bridge Studio → Fused.</p>
    <div className="mt-4"><BridgeDownloadLink /></div>
  </div>;
}

/** Keep installations distinct while linking each profile to its own operation catalogue. */
function BridgeRow({ item, canManage, busy, onRevoke }: {
  item: BridgeInstallation; canManage: boolean; busy: boolean; onRevoke: () => void;
}) {
  const legacy = item.scope !== "workspace";
  return <div>
    <div className="flex flex-col gap-3 px-4 py-4 sm:flex-row sm:items-center sm:justify-between sm:gap-4 sm:px-5">
      <div className="flex min-w-0 flex-1 items-center gap-3">
        <span className="shrink-0 rounded-lg bg-slate-100 p-2 text-slate-500"><Cable className="h-5 w-5" aria-hidden="true" /></span>
        <div className="min-w-0 flex-1">
          <h3 className="break-words text-sm font-semibold text-slate-900">{item.name}</h3>
          {/* Older grants need fresh consent; never imply they already authorize the workspace. */}
          <p className={`mt-1 text-xs leading-5 ${legacy ? "text-amber-700" : "text-slate-500"}`}>{legacy ? "Reconnect in Bridge Studio to approve workspace access" : `Profiles: ${item.profile_ids.length} · Workspace connection`}</p>
        </div>
      </div>
      {/* Read-only users may inspect profiles but cannot change the pairing. */}
      {canManage && <button type="button" onClick={onRevoke} disabled={busy} className={`${secondaryButton} sm:shrink-0`}><Unlink className="h-4 w-4" aria-hidden="true" /> Disconnect</button>}
    </div>
    <div className="divide-y divide-slate-100 border-t border-slate-100">
      {/* An empty installation remains a valid connection while awaiting profile discovery. */}
      {item.profile_ids.length === 0 && <p className="px-5 py-4 text-sm text-slate-500">No profiles discovered yet.</p>}
      {item.profile_ids.map(/** Profile identity is URL encoded so names cannot change route boundaries. */ profile => <Link key={profile} to={`/integrations/bridges/${encodeURIComponent(item.pairing_id)}/${encodeURIComponent(profile)}`} className="flex min-h-14 items-center justify-between gap-3 px-4 py-3 text-sm text-slate-700 hover:bg-slate-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-slate-400 sm:px-5">
        <span><span className="block font-medium text-slate-900">{profile}</span><span className="mt-1 block text-xs text-slate-500">View profile and operations</span></span><ChevronRight className="h-4 w-4 shrink-0 text-slate-400" aria-hidden="true" />
      </Link>)}
    </div>
  </div>;
}
