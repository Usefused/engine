import { useEffect, useState, type FormEvent } from "react";
import { Search, Loader2, X, ChevronDown, ListChecks } from "lucide-react";
import { listWorkflows } from "~/lib/workflow-api";
import type { Workflow } from "~/lib/workflow-library";
import { WorkflowListItem } from "./WorkflowListItem";
import { WorkflowRequirements } from "./WorkflowDetails";

/** Lets App Builder browse and combine workflows without leaving its in-progress form. */
export function BuilderWorkflows({ selected, onChange, disabled }: { selected: Workflow[]; onChange: (items: Workflow[]) => void; disabled: boolean }) {
  const [search, setSearch] = useState("");
  const [query, setQuery] = useState("");
  const [offset, setOffset] = useState(0);
  const [page, setPage] = useState<{ items: Workflow[]; total: number }>({ items: [], total: 0 });
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  // Catalogue requests are read-only and stale responses cannot replace a later search.
  useEffect(() => {
    let active = true;
    setLoading(true);
    setError("");
    listWorkflows(query, [], offset).then((result) => {
      // Ignore a response after navigation or a newer search.
      if (active) setPage(result);
    }).catch((cause) => {
      // Only the current request may report an error in this pane.
      if (active) setError(String(cause));
    }).finally(() => {
      // Stale completion must not hide a newer request's progress indicator.
      if (active) setLoading(false);
    });
    return () => { active = false; };
  }, [query, offset]);

  // Query submission resets paging without clearing the chosen release set.
  function submit(event: FormEvent) { event.preventDefault(); setOffset(0); setQuery(search.trim()); }
  // Clearing returns to the first catalogue page while preserving selection.
  function clear() { setSearch(""); setQuery(""); setOffset(0); }
  // Selected releases remain available above search results even when they are on another page.
  function toggle(id: string) {
    // Removal is always legal, including at the composition limit.
    if (selected.some((item) => item.id === id)) { onChange(selected.filter((item) => item.id !== id)); return; }
    const workflow = page.items.find((item) => item.id === id);
    // Only loaded, verified releases may enter the bounded selection.
    if (workflow && selected.length < 32) onChange([...selected, workflow]);
  }
  const ids = selected.map((item) => item.id);
  const unselected = page.items.filter((item) => !ids.includes(item.id));
  return <div className="space-y-4">
    {/* Selected definitions stay visible once, rather than repeating among catalogue results. */}
    {selected.length > 0 && <section aria-label="Selected workflows" className="space-y-3">
      <h2 className="text-sm font-semibold text-slate-900">Selected workflows ({selected.length})</h2>
      {selected.map((workflow) => <WorkflowListItem key={workflow.id} workflow={workflow} selected={ids} onToggle={toggle} disabled={disabled} />)}
      <details className="group/requirements overflow-hidden rounded-xl border border-slate-200 bg-white">
        <summary className="flex cursor-pointer list-none items-center gap-2.5 px-4 py-3.5 text-sm font-medium text-slate-700 transition-colors hover:bg-slate-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-violet-400 [&::-webkit-details-marker]:hidden">
          <ListChecks className="h-4 w-4 text-slate-400" aria-hidden="true" />
          <span className="flex-1">Requirements and services</span>
          <ChevronDown className="h-4 w-4 text-slate-400 transition-transform group-open/requirements:rotate-180" aria-hidden="true" />
        </summary>
        <div className="border-t border-slate-100 p-4 sm:p-5"><WorkflowRequirements workflows={selected} embedded /></div>
      </details>
    </section>}
    <form onSubmit={submit} role="search" className="relative w-full">
      <button aria-label="Search workflows" disabled={loading} className="absolute left-3 top-1/2 -translate-y-1/2 text-slate-400">
        {/* The search control owns progress without hiding the selected workflows. */}
        {loading ? <Loader2 className="h-5 w-5 animate-spin" /> : <Search className="h-5 w-5" />}
      </button>
      <input aria-label="Search workflows" placeholder="Search workflows" value={search} onChange={(event) => setSearch(event.target.value)}
        className="w-full rounded-xl border border-slate-200 bg-white py-3 pl-10 pr-10 text-sm shadow-sm focus:outline-none focus:ring-2 focus:ring-blue-500/20" />
      {/* The clear control is meaningful only while a query is visible. */}
      {search && <button type="button" aria-label="Clear workflow search" onClick={clear} className="absolute right-3 top-1/2 -translate-y-1/2 text-slate-400"><X className="h-5 w-5" /></button>}
    </form>
    {/* Catalogue errors leave the verified selection available and never remove it silently. */}
    {error && <p role="alert" className="text-sm text-red-700">{error}</p>}
    {unselected.map((workflow) => <WorkflowListItem key={workflow.id} workflow={workflow} selected={ids} onToggle={toggle} disabled={disabled || loading} />)}
    {/* The empty state distinguishes exhausted results from a pending query. */}
    {!loading && !error && unselected.length === 0 && <p className="py-4 text-sm text-slate-500">No more workflows in these results.</p>}
    <nav aria-label="Workflow catalogue pages" className="flex justify-between text-xs text-slate-500">
      <button type="button" disabled={loading || offset === 0} onClick={() => setOffset(Math.max(0, offset - 32))} className="disabled:opacity-40">Previous</button>
      <button type="button" disabled={loading || offset + page.items.length >= page.total} onClick={() => setOffset(offset + 32)} className="disabled:opacity-40">Next</button>
    </nav>
  </div>;
}
