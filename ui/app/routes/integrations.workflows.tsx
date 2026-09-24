import { useEffect, useState, type FormEvent } from "react";
import { Link, Outlet, useLoaderData, useNavigation, useSearchParams } from "@remix-run/react";
import { ArrowRight, ChevronLeft, ChevronRight, Loader2, Search, X } from "lucide-react";
import { listWorkflows } from "~/lib/workflow-api";
import { workflowSelectionURL } from "~/lib/workflow-library";
import { WorkflowListItem } from "~/components/workflows/WorkflowListItem";
import { WorkflowPageHeader } from "~/components/workflows/WorkflowDetails";

/** Loads one bounded catalogue page from the Registry while keeping presentation in Engine. */
export async function clientLoader({ request }: { request: Request }) {
  const params = new URL(request.url).searchParams;
  const offset = Math.max(0, Number(params.get("offset")) || 0);
  return {
    ...(await listWorkflows(params.get("q") ?? "", [], offset)),
    offset,
  };
}

/** Keeps the catalogue mounted beneath nested workflow detail panels so filters and selection survive inspection. */
export default function WorkflowLibrary() {
  const { items, total, offset } = useLoaderData<typeof clientLoader>();
  const [params, setParams] = useSearchParams();
  const navigation = useNavigation();
  const selected = params.getAll("workflow");
  const query = params.get("q") ?? "";
  const [search, setSearch] = useState(query);
  const searching = navigation.state === "loading";

  // Browser history must restore the submitted query as well as its results.
  useEffect(() => { setSearch(query); }, [query]);

  // A checked release remains selected across catalogue paging and detail navigation.
  function toggle(id: string) {
    const ids = new Set(selected);
    // Always allow deselection; the composition contract caps additions at 32 releases.
    if (ids.has(id)) ids.delete(id);
    else if (ids.size < 32) ids.add(id);
    const next = new URLSearchParams(params);
    next.delete("workflow");
    for (const value of ids) next.append("workflow", value);
    setParams(next);
  }

  // A new query resets paging without discarding the user's selected releases.
  function runSearch(value: string) {
    const next = new URLSearchParams(params);
    next.set("q", value);
    next.delete("offset");
    setParams(next);
  }

  // Enter and the inline search icon share the same explicit search action as Services.
  function submitSearch(event: FormEvent) {
    event.preventDefault();
    runSearch(search);
  }

  // Clearing the filter restores the catalogue while retaining app composition choices.
  function clearSearch() {
    setSearch("");
    runSearch("");
  }

  // Paging retains selection so users can combine workflows from different catalogue pages.
  function pageURL(nextOffset: number) {
    const next = new URLSearchParams(params);
    next.set("offset", String(nextOffset));
    return `?${next}`;
  }

  return (
    <div className="space-y-6">
      <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
        <WorkflowPageHeader title="Workflows" description="Combine ready-made operations into one SDK or MCP app." />
        {/* Installation requires an explicit nonempty set of immutable release IDs. */}
        {selected.length > 0 && (
          <Link className="inline-flex shrink-0 items-center justify-center gap-2 rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-slate-700 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-blue-500 focus-visible:ring-offset-2"
            to={workflowSelectionURL("/integrations/builder", selected)}>
            {/* The label reflects the complete selection, including releases on other pages. */}
            Add {selected.length} workflow{selected.length === 1 ? "" : "s"} to app
            <ArrowRight className="h-4 w-4" aria-hidden="true" />
          </Link>
        )}
      </div>
      <form onSubmit={submitSearch} role="search" className="relative w-full">
        <button type="submit" aria-label="Search workflows" disabled={searching}
          className="absolute left-2.5 top-1/2 -translate-y-1/2 cursor-pointer text-slate-400 hover:text-slate-600 disabled:opacity-50">
          {/* Loading feedback stays within the search field, matching the other catalogue pages. */}
          {searching ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Search className="h-3.5 w-3.5" />}
        </button>
        <input aria-label="Search workflows" value={search} onChange={(event) => setSearch(event.target.value)} placeholder="Search workflows"
          className="w-full rounded-lg border border-slate-300 py-2 pl-9 pr-8 text-sm focus:outline-none focus:ring-2 focus:ring-blue-500" />
        {/* A visible query can be cleared without losing selected workflows. */}
        {search && <button type="button" aria-label="Clear search" onClick={clearSearch}
          className="absolute right-2.5 top-1/2 -translate-y-1/2 cursor-pointer text-slate-400 hover:text-slate-600"><X className="h-3.5 w-3.5" /></button>}
      </form>
      {/* An empty result replaces selection rows rather than suggesting there are workflows to choose. */}
      {items.length === 0 ? (
        <div className="rounded-xl border border-slate-200 bg-white px-6 py-12 text-center">
          <h2 className="text-base font-medium text-slate-900">No workflows found</h2>
          <p className="mt-1 text-sm text-slate-500">Try another search, or publish a workflow to make it available here.</p>
        </div>
      ) : (
        <div className="space-y-3">
          {items.map((workflow) => <WorkflowListItem key={workflow.id} workflow={workflow} selected={selected} onToggle={toggle} />)}
        </div>
      )}
      <nav aria-label="Workflow pages" className="flex items-center justify-between gap-3 border-t border-slate-100 px-1 py-3 text-xs text-slate-500">
        {/* Empty results must not display an inverted range such as 1–0. */}
        <span>{total === 0 ? "0 workflows" : `${offset + 1}–${offset + items.length} of ${total}`}</span>
        <div className="flex items-center gap-3">
          {/* Previous and next are only offered when that catalogue page exists. */}
          {offset > 0 && <Link className="inline-flex items-center gap-1 rounded-md p-1.5 hover:bg-slate-100" to={pageURL(Math.max(0, offset - 32))}><ChevronLeft className="h-4 w-4" />Previous</Link>}
          {offset + items.length < total && <Link className="inline-flex items-center gap-1 rounded-md p-1.5 hover:bg-slate-100" to={pageURL(offset + 32)}>Next<ChevronRight className="h-4 w-4" /></Link>}
        </div>
      </nav>
      <Outlet />
    </div>
  );
}
