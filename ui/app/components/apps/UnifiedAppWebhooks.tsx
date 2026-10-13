import { useEffect, useState } from "react";
import { Link } from "@remix-run/react";
import { ArrowUpRight, Loader2, Plus, Webhook } from "lucide-react";
import { api } from "~/lib/api";
import type { UnifiedAppSource } from "~/lib/unified-app-edit";

const action = "inline-flex min-h-11 items-center justify-center gap-2 rounded-lg bg-slate-950 px-4 py-2 text-sm font-semibold text-white hover:bg-slate-800";

/** Inspects the serving version's event selections and routes edits through the existing immutable deployment workflow. */
export function UnifiedAppWebhooks({ appID, active }: { appID: string; active: boolean }) {
  const [source, setSource] = useState<UnifiedAppSource | null>(null);
  const [error, setError] = useState("");
  const [refresh, setRefresh] = useState(0);
  // Only the mounted app owns its private configuration response.
  useEffect(() => {
    let current = true;
    setSource(null); setError("");
    /** Loads persisted selections without consulting a newer provider catalogue. */
    async function load() {
      try {
        const result = await api.appConfig.source<UnifiedAppSource>(appID);
        // Navigation discards stale source instead of showing another app's trigger settings.
        if (current) setSource(result);
      } catch (cause) {
        // A departed tab must not receive the previous app's error.
        if (current) setError(String(cause));
      }
    }
    void load();
    /** Prevents late requests from changing the next app's trigger panel. */
    return () => { current = false; };
  }, [appID, refresh]);
  // Failed reads stay distinct from a successfully loaded app with no triggers.
  if (error) return <section role="alert" className="rounded-xl border border-red-200 bg-red-50 p-5 text-sm text-red-800">{error}<button type="button" onClick={() => setRefresh((value) => value + 1)} className="ml-3 font-semibold underline">Try again</button></section>;
  // Trigger actions require a known saved version and cannot use guessed configuration.
  if (!source) return <p role="status" className="flex items-center gap-2 text-sm text-slate-500"><Loader2 className="h-4 w-4 animate-spin" />Loading webhook triggers…</p>;
  const services = Object.entries(source.config.services).filter(([, service]) => Boolean(service.webhooks?.length) || service.webhooks_select_all === true);
  const attachment = source.config.webhook_attachment;
  const editURL = `/integrations/unified-apps/new?edit=${encodeURIComponent(appID)}#app-service-selection`;
  return <section className="space-y-5">
    <header className="flex flex-wrap items-start justify-between gap-4">
      <div><h2 className="text-lg font-semibold text-slate-950">Webhook triggers</h2><p className="mt-1 max-w-xl text-sm text-slate-500">Run this app when a selected service event arrives.</p></div>
      {/* Editing creates a reviewed successor version instead of mutating the deployed event scope. */}
      <Link to={editURL} className={action}><Plus className="h-4 w-4" />{services.length ? "Edit triggers" : "Add webhook trigger"}</Link>
    </header>
    {/* A cleared serving pointer must never make retained selections look operational. */}
    {!active && <p className="rounded-lg border border-amber-200 bg-amber-50 p-4 text-sm text-amber-900">No version is receiving traffic. These are saved settings for version {source.config.version}; webhook events will not run this app until a version is active.</p>}
    {/* Empty selection is actionable without pretending a registration alone triggers execution. */}
    {services.length ? <div className="overflow-hidden rounded-xl border border-slate-200 bg-white">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-slate-100 px-5 py-4"><span className="text-sm font-medium text-slate-900">Selected events</span><span className="text-xs text-slate-500">Version {source.config.version}</span></div>
      {services.map(([name, service]) => <article key={name} className="space-y-3 border-b border-slate-100 p-5 last:border-0"><div className="flex flex-wrap items-center gap-2"><Webhook className="h-4 w-4 text-slate-400" /><h3 className="text-sm font-semibold text-slate-900">{name}</h3><span className="text-xs text-slate-500">{service.version}</span></div><div className="flex flex-wrap gap-2">{/* Explicit select-all is distinct from a list of saved event names. */}{service.webhooks_select_all === true ? <span className="rounded-md bg-violet-50 px-2.5 py-1 text-xs text-violet-800">All selected-version events</span> : service.webhooks?.map((event) => <code key={event} className="break-all rounded-md bg-slate-100 px-2.5 py-1 text-xs text-slate-700">{event}</code>)}</div></article>)}
    </div> : <div className="rounded-xl border border-dashed border-slate-300 bg-white px-6 py-12 text-center"><Webhook className="mx-auto h-8 w-8 text-slate-400" /><h3 className="mt-4 font-semibold text-slate-900">No webhook triggers yet</h3><p className="mx-auto mt-2 max-w-md text-sm text-slate-500">Choose the events that should run this app and attach a webhook registration to receive them.</p></div>}
    {/* Registration management is separate from changing the app's immutable event selections. */}
    {attachment && <div className="flex flex-wrap items-center justify-between gap-4 rounded-xl border border-slate-200 bg-white p-5"><div><p className="text-xs font-medium text-slate-500">Webhook registration</p><p className="mt-1 break-all text-sm font-semibold text-slate-900">{attachment}</p></div><Link to={`/integrations/webhooks?search=${encodeURIComponent(attachment)}`} className="inline-flex min-h-10 items-center gap-2 text-sm font-semibold text-[var(--brand-violet)]">Manage registration<ArrowUpRight className="h-4 w-4" /></Link></div>}
    <p className="text-xs text-slate-500">Trigger changes are reviewed and deployed as a new app version. Existing schedules and the execution URL continue to follow the active version.</p>
  </section>;
}
