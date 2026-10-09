import { useCallback, useEffect, useState } from "react";
import type { MetaFunction } from "@remix-run/react";
import { ShieldOff, Unlink } from "lucide-react";
import { useToast } from "~/components/Toast";
import { AccessTabs } from "~/components/access/AccessTabs";
import { api, type OAuthConnectedApp } from "~/lib/api";

export const meta: MetaFunction = () => [{ title: "Connected Apps - Fused" }];

// Every signed-in user manages only their own OAuth consents here; unlike
// People/Teams/OAuth Clients, there is no access.read gate because this page
// never exposes another person's data or requires workspace RBAC.
export default function ConnectedAppsPage() {
  return (
    <>
      <AccessTabs />
      <ConnectedAppsManager />
    </>
  );
}

/** Lists the third-party applications the signed-in user has authorized and lets them revoke access individually. */
function ConnectedAppsManager() {
  const toast = useToast();
  const [apps, setApps] = useState<OAuthConnectedApp[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [revokingId, setRevokingId] = useState("");

  const refresh = useCallback(async () => {
    const payload = await api.connectedApps.list();
    setApps(payload.connected_apps);
  }, []);

  // A failed read must not masquerade as an account with no connected apps.
  useEffect(() => {
    setLoading(true);
    setLoadError("");
    refresh().catch((error: unknown) => setLoadError(errorMessage(error))).finally(() => setLoading(false));
  }, [refresh]);

  async function handleRevoke(app: OAuthConnectedApp) {
    const confirmed = await toast.confirm(`Disconnect "${app.client_name}"? It will immediately lose access to your account.`);
    if (!confirmed) return;
    setRevokingId(app.client_id);
    try {
      await api.connectedApps.revoke(app.client_id);
      await refresh();
      toast.success("App disconnected.");
    } catch (error: unknown) {
      toast.error(errorMessage(error));
    } finally {
      setRevokingId("");
    }
  }

  return (
    <div className="space-y-6">
      <header>
        <p className="text-sm font-medium text-blue-600">Access</p>
        <h1 className="text-2xl font-bold text-slate-900 flex items-center gap-2"><ShieldOff className="w-6 h-6" /> Connected Apps</h1>
        <p className="text-slate-500 mt-1">Third-party applications you've authorized to act on your behalf via Fused's OAuth2 sign-in.</p>
      </header>

      <section className="bg-white border border-slate-200 rounded-xl shadow-sm overflow-hidden">
        <div className="divide-y divide-slate-100">
          {loading && <p className="p-4 text-sm text-slate-500">Loading connected apps…</p>}
          {/* Keep transport and authorization failures visible instead of claiming the account is empty. */}
          {!loading && loadError && <p role="alert" className="p-4 text-sm text-rose-700">Could not load connected apps: {loadError}</p>}
          {/* Show an empty state only after a successful read of this person's authorizations. */}
          {!loading && !loadError && apps.length === 0 && <div className="flex flex-col items-center px-5 py-10 text-center">
            <span className="mb-4 rounded-2xl bg-slate-100 p-3 text-slate-400"><Unlink className="h-6 w-6" aria-hidden="true" /></span>
            <h2 className="text-sm font-semibold text-slate-900">No connected apps yet</h2>
            <p className="mt-2 max-w-sm text-sm leading-6 text-slate-500">Apps you authorize with Fused will appear here. You can review and disconnect them anytime.</p>
          </div>}
          {apps.map((app) => (
            <ConnectedAppRow key={app.client_id} app={app} revoking={revokingId === app.client_id} onRevoke={handleRevoke} />
          ))}
        </div>
      </section>
    </div>
  );
}

/** Stacks the action on phones so long app names and scopes remain readable. */
function ConnectedAppRow({ app, revoking, onRevoke }: { app: OAuthConnectedApp; revoking: boolean; onRevoke: (app: OAuthConnectedApp) => void }) {
  return (
    <div className="flex flex-col gap-3 px-4 py-4 sm:flex-row sm:items-center sm:justify-between sm:gap-4">
      <div className="min-w-0 flex-1">
        <p className="break-words text-sm font-medium text-slate-800">{app.client_name}</p>
        <p className="mt-1 break-words text-xs leading-5 text-slate-500 [overflow-wrap:anywhere]">Scopes: {app.scope.join(", ")}</p>
        <p className="mt-1 text-xs leading-5 text-slate-500">Connected {new Date(app.granted_at).toLocaleString()}</p>
      </div>
      <button type="button" onClick={() => onRevoke(app)} disabled={revoking} className="inline-flex min-h-11 items-center justify-center gap-1.5 rounded-lg border border-slate-300 px-3 text-sm text-slate-950 hover:bg-slate-50 disabled:opacity-50 sm:shrink-0">
        <Unlink className="w-4 h-4" /> Disconnect
      </button>
    </div>
  );
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : "The request could not be completed.";
}
