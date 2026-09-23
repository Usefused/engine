import { useState } from "react";
import { Link, useLoaderData, useSearchParams } from "@remix-run/react";
import { listWorkflows } from "~/lib/workflow-api";
import { workflowSelectionURL } from "~/lib/workflow-library";
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

/** Presents reusable templates and keeps a multi-workflow selection explicit in the URL. */
export default function WorkflowLibrary() {
  const { items, total, offset } = useLoaderData<typeof clientLoader>();
  const [params, setParams] = useSearchParams();
  const selected = params.getAll("workflow");
  const [search, setSearch] = useState(params.get("q") ?? "");
  // A checked release remains selected across catalogue paging and detail navigation.
  function toggle(id: string) {
    const ids = new Set(selected);
    if (ids.has(id)) ids.delete(id);
    else if (ids.size < 32) ids.add(id);
    const next = new URLSearchParams(params);
    next.delete("workflow");
    for (const value of ids) next.append("workflow", value);
    setParams(next);
  }
  // Search resets only the page cursor, preserving the user's composition choices.
  function submitSearch(event: React.FormEvent) {
    event.preventDefault();
    const next = new URLSearchParams(params);
    next.set("q", search);
    next.delete("offset");
    setParams(next);
  }
  // Paging retains selection so users can combine workflows from different catalogue pages.
  function pageURL(nextOffset: number) {
    const next = new URLSearchParams(params);
    next.set("offset", String(nextOffset));
    return `?${next}`;
  }
  return (
    <main className="mx-auto max-w-6xl p-4 sm:p-8 space-y-6">
      <WorkflowPageHeader
        title="Workflows"
        description="Combine ready-made operations into one SDK or MCP app."
      />
      <div className="flex flex-wrap items-center justify-between gap-4">
        <form onSubmit={submitSearch} className="flex gap-2">
          <input
            aria-label="Search workflows"
            value={search}
            onChange={(event) => setSearch(event.target.value)}
            placeholder="Search workflows"
            className="min-w-0 rounded-lg border px-3 py-2"
          />
          <button className="rounded-lg border px-4 py-2">Search</button>
        </form>
        {/* Installation requires an explicit nonempty set of immutable release IDs. */}
        {selected.length > 0 && (
          <Link
            className="rounded-lg bg-violet-600 px-4 py-2 text-white"
            to={workflowSelectionURL(
              "/integrations/workflows/install",
              selected
            )}
          >
            Add {selected.length} workflow{selected.length === 1 ? "" : "s"} to
            app
          </Link>
        )}
      </div>
      {/* A true empty catalogue gives publishers a concrete authoring entry point. */}
      {items.length === 0 && (
        <div className="rounded-xl border bg-white p-8">
          <h2 className="font-semibold">No workflows found</h2>
          <p className="mt-2 text-slate-600">
            Publish a workflow template with{" "}
            <code>fused-cli workflow publish template.json</code>, or try
            another search.
          </p>
        </div>
      )}
      <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        {items.map((workflow) => (
          <article
            key={workflow.id}
            className="rounded-xl border border-slate-200 bg-white p-5 space-y-3"
          >
            <div className="flex items-start justify-between gap-3">
              <span className="text-xs font-semibold uppercase tracking-wide text-violet-700">
                {workflow.template.category || "Workflow"}
              </span>
              <input
                type="checkbox"
                aria-label={`Select ${workflow.template.name}`}
                checked={selected.includes(workflow.id)}
                disabled={
                  !selected.includes(workflow.id) && selected.length >= 32
                }
                onChange={() => toggle(workflow.id)}
              />
            </div>
            <h2 className="text-lg font-semibold">
              <Link
                className="hover:text-violet-700"
                to={workflowSelectionURL(
                  `/integrations/workflows/${workflow.id}`,
                  selected
                )}
              >
                {workflow.template.name}
              </Link>
            </h2>
            <p className="text-sm text-slate-600 line-clamp-3">
              {workflow.template.description}
            </p>
            <p className="text-xs text-slate-500">
              {workflow.publisher} · {workflow.template.version}
            </p>
            <div className="flex flex-wrap gap-2">
              {Object.keys(workflow.template.services).map((key) => (
                <span
                  key={key}
                  className="break-all rounded bg-slate-100 px-2 py-1 text-xs"
                >
                  {key}
                </span>
              ))}
            </div>
            <Link
              className="text-sm font-medium text-violet-700"
              to={workflowSelectionURL(
                `/integrations/workflows/${workflow.id}`,
                selected
              )}
            >
              View details and requirements →
            </Link>
          </article>
        ))}
      </div>
      <nav aria-label="Workflow pages" className="flex justify-between text-sm">
        <span>{total} releases</span>
        <div className="flex gap-4">
          {offset > 0 && (
            <Link to={pageURL(Math.max(0, offset - 32))}>Previous</Link>
          )}
          {offset + items.length < total && (
            <Link to={pageURL(offset + 32)}>Next</Link>
          )}
        </div>
      </nav>
    </main>
  );
}
