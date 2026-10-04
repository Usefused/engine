import { useEffect, useState } from "react";
import { createPortal } from "react-dom";
import { Link, useSearchParams } from "@remix-run/react";
import { ChevronLeft, ChevronRight, Plus, RefreshCw, Search, Webhook } from "lucide-react";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasAnyPermission, hasWorkspacePermission } from "~/lib/current-actor-access";
import { WebhookSigningSecret } from "~/components/webhooks/WebhookSigningSecret";
import { CopyValue } from "~/components/CopyValue";
import { Select } from "~/components/forms/Select";
import { webhookListings } from "~/lib/webhook-discovery-api";
import { copyableWebhookURL, type WebhookListing } from "~/lib/webhook-discovery-contract";

/** Gives webhook pages a clear browser identity distinct from service detail routes. */
export const meta = () => [{ title: "Webhooks - Fused" }];

/** Lists authorized receiving URLs with server filters and the same page controls as the services list. */
export default function WebhooksPage() {
  const { access, loading: accessLoading } = useCurrentActorAccess();
  const [params, setParams] = useSearchParams();
  const [items, setItems] = useState<WebhookListing[]>([]);
  const [search, setSearch] = useState(""), [error, setError] = useState("");
  const [loading, setLoading] = useState(true), [refresh, setRefresh] = useState(0);
  const selected = params.get("service") || "";
  const [page, setPage] = useState({ filter: "", offset: 0 });
  const [total, setTotal] = useState(0);
  const filterKey = JSON.stringify([selected, search]);
  // A filter change always starts at its first page without issuing an obsolete-offset request.
  const offset = page.filter === filterKey ? page.offset : 0;
  const pageSize = 20;
  const currentPage = Math.floor(offset / pageSize) + 1;
  const totalPages = Math.ceil(total / pageSize);
  /** Converts the shared one-based page controls into server offsets without losing the active filters. */
  function changePage(nextPage: number) {
    // Keep dropdown and arrow navigation within the server-reported page boundaries.
    const boundedPage = Math.max(1, Math.min(nextPage, totalPages));
    setPage({ filter: filterKey, offset: (boundedPage - 1) * pageSize });
  }
  const canRead = hasAnyPermission(access, "service.read"), canCreate = hasWorkspacePermission(access, "app.webhook.create");
  // Filters and pagination are evaluated by Engine; the browser displays only the returned page.
  useEffect(() => {
    let active = true;
    const controller = new AbortController();
    // Read permission is independent of creation; do not issue unauthorized discovery requests.
    if (accessLoading || !canRead) { setLoading(false); return; }
    setLoading(true); setError(""); setItems([]);
    /** Ignore results after navigation so one service's URLs cannot overwrite a newer filter. */
    async function load() {
      try {
        const result = await webhookListings({ limit: pageSize, offset, serviceId: selected || undefined, search }, controller.signal);
        // A stale request has no authority to replace the current page's state.
        if (!active) return;
        setItems(result.items); setTotal(result.total);
      } catch (cause) {
        // A cancelled navigation must not surface an error on the next page.
        if (active) setError(String(cause));
      } finally {
        // Only the currently mounted load owns the spinner state.
        if (active) setLoading(false);
      }
    }
    // Coalesce rapid typing into one server query and cancel obsolete work on navigation.
    const timer = setTimeout(() => { void load(); }, 250);
    // Cleanup owns both the debounce timer and the in-flight request.
    return () => { active = false; clearTimeout(timer); controller.abort(); };
  }, [canRead, accessLoading, selected, search, offset, refresh]);
  const createURL = `/integrations/webhooks/new${selected ? `?service=${encodeURIComponent(selected)}` : ""}`;
  return <div className="min-w-0 space-y-5 sm:space-y-6">
    <header className="space-y-3">
      <div className="flex min-w-0 items-center gap-3">
        <span className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-slate-100 text-slate-600"><Webhook className="h-5 w-5" aria-hidden="true" /></span>
        <div className="min-w-0"><h1 className="text-xl font-semibold text-slate-900">Webhooks</h1></div>
      </div>
      <p className="text-sm text-slate-600">Receive service events and trigger actions in your apps.</p>
      {/* Route actions share the same utility row as Service details and notifications. */}
      {canCreate && <WebhookHeaderAction to={createURL} />}
    </header>
    {params.get("created") && <p role="status" className="rounded-lg border border-emerald-200 bg-emerald-50 p-4 text-sm text-emerald-800">Webhook created. Copy its receiving URL into your provider’s webhook settings.</p>}
    <section aria-label="Registered webhooks" className="overflow-hidden rounded-lg border border-slate-200 bg-white">
    {/* Search queries persisted webhooks directly without a catalogue-backed service selector. */}
    <div className="grid grid-cols-[minmax(0,1fr)_2.25rem] items-center gap-2 border-b border-slate-100 px-4 py-3 sm:px-5">
      <div className="relative min-w-0">
        <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-slate-400" />
        <input aria-label="Search webhooks" placeholder="Search webhooks" value={search} onChange={(event) => setSearch(event.target.value)} className="h-9 w-full rounded-md border border-slate-300 bg-white pl-9 pr-3 text-sm outline-none focus:border-slate-500 focus:ring-1 focus:ring-gray-500" />
      </div>
      <button type="button" aria-label="Refresh webhooks" title="Refresh webhooks" disabled={loading} onClick={() => setRefresh((value) => value + 1)} className="inline-flex h-9 w-9 items-center justify-center rounded-lg text-slate-400 hover:bg-slate-100 hover:text-slate-700 focus-visible:outline-2 focus-visible:outline-[var(--brand-violet)] disabled:opacity-40">
        <RefreshCw className="h-4 w-4" aria-hidden="true" />
      </button>
    </div>
    {/* A service-detail link supplies an optional server filter without loading a separate service picker. */}
    {selected && <div className="border-b border-slate-100 px-5 py-2 text-sm text-slate-600">Filtered by service. <button type="button" className="underline" onClick={() => {
      const next = new URLSearchParams(params);
      next.delete("service"); next.delete("created"); setParams(next);
    }}>Show all services</button></div>}
    {error && <p role="alert" className="rounded-lg bg-red-50 p-4 text-sm text-red-700">{error}</p>}
    {/* Loading, denial, failure, and a verified empty catalogue remain distinct states. */}
    {accessLoading || loading ? <p role="status" className="p-8 text-center text-sm text-slate-500">Loading webhook URLs…</p> : !canRead ? <p className="rounded-xl border border-slate-200 bg-white p-6 text-sm text-slate-600">Service read access is needed to view registered URLs.</p> : items.length ? <div className="divide-y divide-slate-100">{items.map((item) => <RegistrationCard key={`${item.service_id}:${item.slug}`} item={item} canManage={hasWorkspacePermission(access, "app.webhook.manage")} onSaved={() => setRefresh((value) => value + 1)} />)}</div> : !error && <section className="px-6 py-12 text-center"><Webhook className="mx-auto mb-3 h-8 w-8 text-slate-400" /><h2 className="text-sm font-semibold text-slate-900">{search || selected ? "No matching webhooks" : "No webhooks yet"}</h2><p className="mx-auto mt-2 max-w-md text-sm text-slate-500">{search ? "Try another name or service." : "Create a receiving URL, then add it to your provider’s webhook settings."}</p>{canCreate && !search && <Link to={createURL} className="mt-5 inline-flex items-center gap-2 text-sm font-semibold text-[var(--brand-violet)]"><Plus className="h-4 w-4" />Create your first webhook</Link>}</section>}
    {/* Completed server totals drive the services-style range, page selector, and bounded arrow controls. */}
    {!loading && !error && total > pageSize && <nav aria-label="Webhook pages" className="flex flex-wrap items-center justify-between gap-3 border-t border-slate-100 px-4 py-3 sm:px-5">
      <p className="text-xs text-slate-500">{offset + 1}-{Math.min(offset + pageSize, total)} of {total}</p>
      <div className="flex items-center gap-1">
        <button type="button" disabled={currentPage === 1} onClick={() => changePage(currentPage - 1)} className="rounded-md p-1.5 text-slate-500 hover:bg-slate-100 disabled:opacity-40 disabled:hover:bg-transparent" aria-label="Previous page" title="Previous">
          <ChevronLeft className="h-4 w-4" aria-hidden="true" />
        </button>
        <span className="pl-2 text-xs text-slate-500">Page</span>
        <Select aria-label="Webhook page" density="compact" className="mx-1" value={currentPage} onChange={(event) => changePage(Number(event.target.value))}>
          {Array.from({ length: totalPages }, (_, index) => <option key={index + 1} value={index + 1}>{index + 1}</option>)}
        </Select>
        <span className="pr-2 text-xs font-medium text-slate-500">of {totalPages}</span>
        <button type="button" disabled={currentPage >= totalPages} onClick={() => changePage(currentPage + 1)} className="rounded-md p-1.5 text-slate-500 hover:bg-slate-100 disabled:opacity-40 disabled:hover:bg-transparent" aria-label="Next page" title="Next">
          <ChevronRight className="h-4 w-4" aria-hidden="true" />
        </button>
      </div>
    </nav>}
    </section>
  </div>;
}

/** Uses the same authenticated utility row as Service details without reserving another header column. */
function WebhookHeaderAction({ to }: { to: string }) {
  const [host, setHost] = useState<HTMLElement | null>(null);
  useEffect(() => { setHost(document.getElementById("integrations-header-actions")); }, []);
  // The portal target only exists after the shared layout has mounted in the browser.
  if (!host) return null;
  return createPortal(<Link to={to} className="inline-flex h-9 items-center gap-2 rounded-md border border-slate-200 bg-white px-3 text-sm font-medium text-slate-700 shadow-sm hover:bg-slate-50"><Plus className="h-3.5 w-3.5" aria-hidden="true" />Create webhook</Link>, host);
}

/** Keeps provider URLs visible and copyable while distinguishing managed subscriptions from direct ingress. */
function RegistrationCard({ item, canManage, onSaved }: { item: WebhookListing; canManage: boolean; onSaved: () => void }) {
  const url = copyableWebhookURL(item);
  // Older listings preserve their known name when a canonical provider reference is unavailable.
  const serviceTag = item.service_ref || item.service_name;
  return <article className="min-w-0 space-y-4 px-4 py-5 sm:px-5 sm:py-6">
    <div className="flex flex-wrap items-start justify-between gap-3">
      <div className="min-w-0 space-y-2">
        <h2 className="text-sm font-semibold text-slate-900">{item.label}</h2>
        <Link to={`/integrations/${item.service_id}?tab=webhooks`} className="inline-flex max-w-full rounded-md bg-slate-100 px-2 py-0.5 text-[10px] font-medium text-slate-600 hover:bg-slate-200 hover:text-slate-900"><span className="break-all">{serviceTag}</span></Link>
      </div>
      {/* Managed subscriptions and direct verification have distinct setup requirements. */}
      <span className="rounded-md bg-slate-100 px-2 py-1 text-[10px] font-medium text-slate-600">{item.delivery_mode === "managed" ? "Managed delivery" : item.signature === "set" ? "Signing secret configured" : "No signing secret"}</span>
    </div>
    {/* Only Engine-projected URLs are copied; local browser origins are never substituted. */}
    {url ? <CopyValue value={url} label="webhook URL" /> : <p className="text-sm text-slate-500">{item.delivery_mode === "managed" ? "Events arrive through your managed connection. No provider URL is needed." : <>Public URL not configured. Receiving path: <code className="select-all break-all">/webhook/{item.slug}</code></>}</p>}
    {/* Receiving URLs remain readable without granting authority to change verification. */}
    <div className="flex flex-wrap items-center gap-x-4 gap-y-2 pt-1">{item.signing_secret && <Link to={`/integrations/buckets?${new URLSearchParams({bucket:item.signing_secret.bucket_id,tab:"secrets",secret:item.signing_secret.key_name})}`} className="text-[11px] font-medium text-slate-500 hover:text-[var(--brand-violet)]">Manage secret</Link>}{canManage && item.delivery_mode === "direct" && <WebhookSigningSecret slug={item.slug} onSaved={onSaved} />}</div>
  </article>;
}
