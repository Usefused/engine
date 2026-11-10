import { useEffect, useState } from "react";
import { Link } from "@remix-run/react";
import { webhookListings, type WebhookPage } from "~/lib/webhook-discovery-api";

const pageSize = 20;

/** Offers persisted, authorized registrations without creating ingress or changing a deployed app. */
export function WebhookRegistrationPicker({ selected, onSelect }: { selected?: string; onSelect: (name: string) => void }) {
  const [search, setSearch] = useState("");
  const [offset, setOffset] = useState(0);
  const [refresh, setRefresh] = useState(0);
  const [page, setPage] = useState<WebhookPage | null>(null);
  const [error, setError] = useState("");
  // Search and pagination replace the request so departed results cannot supply selectable registrations.
  useEffect(() => {
    const controller = new AbortController();
    setPage(null); setError("");
    /** Keeps transport failures distinct from a workspace with no registered webhooks. */
    async function load() {
      try {
        const result = await webhookListings({ limit: pageSize, offset, search }, controller.signal);
        // Some transports can resolve after cancellation; only the current query owns this list.
        if (!controller.signal.aborted) setPage(result);
      } catch (cause) {
        // Navigation and newer searches should not surface stale failures.
        if (!controller.signal.aborted) setError(String(cause));
      }
    }
    void load();
    /** Cancels registration reads when a new query or page takes ownership. */
    return () => controller.abort();
  }, [search, offset, refresh]);

  return <section aria-label="Choose an existing webhook" className="space-y-4 rounded-xl border border-slate-200 bg-white p-5">
    <div><h3 className="font-semibold text-slate-900">Choose an existing webhook</h3><p className="mt-1 text-sm text-slate-500">Select a registration, then choose the service events that should run this app. Changes take effect after you review and deploy a new version.</p></div>
    <input aria-label="Search existing webhooks" placeholder="Search by webhook or service name" value={search} onChange={(event) => { setSearch(event.target.value); setOffset(0); }} className="w-full rounded-lg border border-slate-300 px-3 py-2 text-sm" />
    {/* Failed reads remain retryable and must never appear as an empty registration list. */}
    {error && <p role="alert" className="text-sm text-red-700">{error}<button type="button" onClick={() => setRefresh((value) => value + 1)} className="ml-3 font-semibold underline">Try again</button></p>}
    {/* A pending query cannot offer choices from the previous result page. */}
    {!page && !error && <p role="status" className="text-sm text-slate-500">Loading existing webhooks…</p>}
    {/* Only a successful empty response can establish that no registrations matched. */}
    {page && page.items.length === 0 && <p className="text-sm text-slate-500">No matching webhooks. Create a registration or try another search.</p>}
    <div className="divide-y divide-slate-100">{page?.items.map((item) => <div key={`${item.service_id}:${item.slug}`} className="flex flex-wrap items-center justify-between gap-3 py-3">
      <div className="min-w-0"><p className="break-all text-sm font-semibold text-slate-900">{item.label}</p><p className="break-all text-xs text-slate-500">{item.service_name} · {item.service_ref}</p></div>
      {/* Bundle labels, rather than receiving URL slugs, identify app attachments across services. */}
      <button type="button" onClick={() => onSelect(item.label)} className="min-h-10 rounded-lg border border-slate-300 px-3 py-2 text-sm font-semibold text-slate-900">{selected === item.label ? "Use selected webhook" : "Use webhook"}</button>
    </div>)}</div>
    {/* Server totals keep registrations beyond the first page reachable without an unbounded read. */}
    {page && page.total > pageSize && <nav aria-label="Existing webhook pages" className="flex items-center justify-between gap-3 text-sm">
      <button type="button" disabled={offset === 0} onClick={() => setOffset((value) => Math.max(0, value - pageSize))} className="min-h-10 disabled:opacity-40">Previous</button>
      <span>{offset + 1}–{Math.min(offset + pageSize, page.total)} of {page.total}</span>
      <button type="button" disabled={offset + pageSize >= page.total} onClick={() => setOffset((value) => value + pageSize)} className="min-h-10 disabled:opacity-40">Next</button>
    </nav>}
    <Link to="/integrations/webhooks/new" target="_blank" rel="noreferrer" className="inline-flex min-h-10 items-center text-sm font-semibold text-[var(--brand-violet)]">Create a webhook registration</Link>
  </section>;
}
