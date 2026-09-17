import { useCallback, useEffect, useState } from "react";
import type { MetaFunction } from "@remix-run/react";
import { ShieldOff, Unlink } from "lucide-react";
import { useToast } from "~/components/Toast";
import { SectionTabs } from "~/components/layout/SectionTabs";
import { api, type OAuthConnectedApp } from "~/lib/api";

const ACCESS_TABS = [
  { label: "People", to: "/integrations/access/people" },
  { label: "Teams", to: "/integrations/access/teams" },
  { label: "OAuth Clients", to: "/integrations/access/oauth-clients" },
  { label: "Connected Apps", to: "/integrations/access/connected-apps" },
];

export const meta: MetaFunction = () => [{ title: "Connected Apps - Fused" }];

// Every signed-in user manages only their own OAuth consents here; unlike
// People/Teams/OAuth Clients, there is no access.read gate because this page
// never exposes another person's data or requires workspace RBAC.
export default function ConnectedAppsPage() {
  return (
    <>
      <SectionTabs tabs={ACCESS_TABS} />
      <ConnectedAppsManager />
    </>
  );
}

/** Lists the third-party applications the signed-in user has authorized and lets them revoke access individually. */
function ConnectedAppsManager() {
  const toast = useToast();
  const [apps, setApps] = useState<OAuthConnectedApp[]>([]);
  const [loading, setLoading] = useState(true);
  const [revokingId, setRevokingId] = useState("");

  const refresh = useCallback(async () => {
    const payload = await api.connectedApps.list();
    setApps(payload.connected_apps);
  }, []);

  useEffect(() => {
    setLoading(true);
    refresh().catch((error: unknown) => toast.error(errorMessage(error))).finally(() => setLoading(false));
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
          {!loading && apps.length === 0 && <p className="p-4 text-sm text-slate-500">You haven't connected any third-party apps.</p>}
          {apps.map((app) => (
            <ConnectedAppRow key={app.client_id} app={app} revoking={revokingId === app.client_id} onRevoke={handleRevoke} />
          ))}
        </div>
      </section>
    </div>
  );
}

function ConnectedAppRow({ app, revoking, onRevoke }: { app: OAuthConnectedApp; revoking: boolean; onRevoke: (app: OAuthConnectedApp) => void }) {
  return (
    <div className="flex items-center justify-between gap-4 px-4 py-3">
      <div className="min-w-0">
        <p className="text-sm font-medium text-slate-800 truncate">{app.client_name}</p>
        <p className="text-xs text-slate-500 mt-0.5 truncate">Scopes: {app.scope.join(", ")}</p>
        <p className="text-xs text-slate-500 mt-0.5">Connected {new Date(app.granted_at).toLocaleString()}</p>
      </div>
      <button type="button" onClick={() => onRevoke(app)} disabled={revoking} className="inline-flex items-center gap-1.5 text-sm text-rose-600 hover:text-rose-700 disabled:opacity-50 shrink-0">
        <Unlink className="w-4 h-4" /> Disconnect
      </button>
    </div>
  );
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : "The request could not be completed.";
}
