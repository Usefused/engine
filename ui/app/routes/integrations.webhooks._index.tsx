import { useEffect, useState } from "react";
import { CataloguePageHeader, catalogueActionClassName } from "~/components/layout/CataloguePageHeader";
import { Link, useSearchParams } from "@remix-run/react";
import { ChevronLeft, ChevronRight, Plus, RefreshCw, Search, Webhook } from "lucide-react";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasAnyPermission, hasWorkspacePermission } from "~/lib/current-actor-access";
import { Select } from "~/components/forms/Select";
import { webhookListings } from "~/lib/webhook-discovery-api";
import { webhookDetailPath, type WebhookCatalogueItem } from "~/lib/webhook-discovery-contract";

// Keep catalogue headers and rows aligned, including at intermediate desktop widths.
const catalogueColumns = "md:grid-cols-[minmax(0,1fr)_10rem_7rem_1rem] md:gap-x-4 md:px-6";

/** Gives webhook pages a clear browser identity distinct from service detail routes. */
export const meta = () => [{ title: "Webhooks - Fused" }];

/** Lists authorized receiving URLs with server filters, including a registration search linked from an app. */
export default function WebhooksPage() {
  const { access, loading: accessLoading } = useCurrentActorAccess();
  const [params, setParams] = useSearchParams();
  const [items, setItems] = useState<WebhookCatalogueItem[]>([]);
  // App registration links prefill the existing server-side search instead of loading every webhook.
  const [search, setSearch] = useState(params.get("search") || ""), [error, setError] = useState("");
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
    <CataloguePageHeader title="Webhooks" icon={Webhook} description="Receive service events and trigger actions in your apps." actions={
      // The title action preserves the existing webhook creation permission.
      canCreate && <Link to={createURL} className={catalogueActionClassName}><Plus className="h-3.5 w-3.5" aria-hidden="true" />Create webhook</Link>
    } />
    {/* Older apply responses may send the user back to search for the new registration. */}
    {params.get("created") && <p role="status" className="rounded-lg border border-emerald-200 bg-emerald-50 p-4 text-sm text-emerald-800">Webhook created. Open its details to copy the receiving URL.</p>}
    <section aria-label="Registered webhooks" className="overflow-hidden rounded-xl border border-slate-200 bg-white">
    {/* Search queries persisted webhooks directly without a catalogue-backed service selector. */}
    <div className="grid grid-cols-[minmax(0,1fr)_2.25rem] items-center gap-2 border-b border-slate-100 px-4 py-3 sm:px-5">
      <div className="relative min-w-0">
        <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-slate-400" />
        <input aria-label="Search webhooks" placeholder="Search webhooks" value={search} onChange={(event) => setSearch(event.target.value)} className="h-9 w-full rounded-md border border-slate-300 bg-white pl-9 pr-3 text-sm outline-none focus:border-slate-500 focus:ring-1 focus:ring-gray-500" />
      </div>
      <button type="button" aria-label="Refresh webhooks" title="Refresh webhooks" disabled={loading} onClick={() => setRefresh((value) => value + 1)} className="inline-flex h-9 w-9 items-center justify-center rounded-lg text-slate-400 hover:bg-slate-100 hover:text-slate-700 focus-visible:outline-2 focus-visible:outline-slate-950 disabled:opacity-40">
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
    {accessLoading || loading ? <p role="status" className="p-8 text-center text-sm text-slate-500">Loading webhooks…</p> : !canRead ? <p className="p-6 text-sm text-slate-600">Service read access is needed to view registered webhooks.</p> : items.length ? <RegistrationList items={items} /> : !error && <section className="px-6 py-12 text-center"><Webhook className="mx-auto mb-3 h-8 w-8 text-slate-400" /><h2 className="text-sm font-semibold text-slate-900">{search || selected ? "No matching webhooks" : "No webhooks yet"}</h2><p className="mx-auto mt-2 max-w-md text-sm text-slate-500">{search ? "Try another name or service." : "Create a receiving URL, then add it to your provider’s webhook settings."}</p>{canCreate && !search && <Link to={createURL} className="mt-5 inline-flex items-center gap-2 text-sm font-semibold text-[var(--brand-violet)]"><Plus className="h-4 w-4" />Create your first webhook</Link>}</section>}
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

/** Mirrors the app catalogue's column headings while keeping each registration a single native link. */
function RegistrationList({ items }: { items: WebhookCatalogueItem[] }) {
  return <>
    <div aria-hidden="true" className={`hidden border-b border-slate-200 bg-slate-50 py-3 text-sm font-medium text-slate-500 md:grid ${catalogueColumns}`}>
      <span className="pl-11">Webhook</span><span>Status</span><span>Created</span>
    </div>
    <ul aria-label="Webhook registrations" className="divide-y divide-slate-100">
      {items.map((item) => <RegistrationRow key={`${item.service_id}:${item.slug}`} item={item} />)}
    </ul>
  </>;
}

/** Summarizes the registration so URL setup, verification controls, and activity stay in details. */
function RegistrationRow({ item }: { item: WebhookCatalogueItem }) {
  // Older registrations can still identify their service by display name.
  const service = item.service_ref || item.service_name;
  // Managed receivers use their connection for delivery; only direct verification uses a signing secret.
  const status = item.delivery_mode === "managed" ? "Managed delivery" : item.signature === "set" ? "Signing secret set" : "No signing secret";
  // Legacy registrations can lack a timestamp; avoid implying a creation date in that case.
  const created = item.created_at ? new Date(item.created_at).toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" }) : "";
  return <li className="min-w-0">
    <Link to={webhookDetailPath(item)} className={`group grid min-w-0 cursor-pointer grid-cols-[minmax(0,1fr)_1rem] items-center gap-x-3 gap-y-1 px-4 py-3 text-sm transition-colors hover:bg-slate-50 focus-visible:bg-slate-50 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-blue-500 ${catalogueColumns}`}>
      <div className="col-start-1 row-start-1 flex min-w-0 items-center gap-3">
        <span className="flex h-8 w-8 shrink-0 items-center justify-center rounded bg-blue-100 text-blue-600"><Webhook aria-hidden="true" className="h-4 w-4" /></span>
        <div className="min-w-0">
          <span title={item.label} className="block font-semibold leading-5 text-slate-900 [overflow-wrap:anywhere] md:truncate">{item.label}</span>
          <span title={service} className="block truncate text-xs leading-5 text-slate-500">{service}</span>
        </div>
      </div>
      <div className="col-start-1 row-start-2 pl-11 md:col-start-2 md:row-start-1 md:pl-0">
        <span className="inline-flex rounded-md bg-slate-100 px-2 py-0.5 text-xs font-medium text-slate-600">{status}</span>
      </div>
      {/* Creation is secondary on narrow screens; the detail page retains the timestamp. */}
      {created && <time dateTime={item.created_at} className="hidden text-slate-500 md:col-start-3 md:row-start-1 md:block"><span className="sr-only">Created </span>{created}</time>}
      <ChevronRight aria-hidden="true" className="col-start-2 row-span-2 row-start-1 h-4 w-4 text-slate-400 group-hover:text-slate-700 md:col-start-4 md:row-span-1" />
    </Link>
  </li>;
}
