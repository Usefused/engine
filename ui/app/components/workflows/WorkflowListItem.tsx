import { Link, useLocation, useSearchParams } from "@remix-run/react";
import { ArrowRight, ChevronRight } from "lucide-react";
import { workflowSelectionURL, type Workflow } from "~/lib/workflow-library";

/** Shows each workflow as an authored action set while sharing selection state with App Builder. */
export function WorkflowListItem({ workflow, selected, onToggle, disabled = false }: {
  workflow: Workflow;
  selected: string[];
  onToggle: (id: string) => void;
  disabled?: boolean;
}) {
  const checked = selected.includes(workflow.id);
  const location = useLocation();
  const [params] = useSearchParams();
  const details = new URLSearchParams();
  // Catalogue filters belong to its background list; builder-only parameters must not leak into it.
  if (location.pathname.startsWith("/integrations/workflows")) {
    for (const name of ["q", "offset"]) {
      const value = params.get(name);
      // Preserve only filters actually present in the current catalogue URL.
      if (value !== null) details.set(name, value);
    }
  }
  const selectionURL = workflowSelectionURL(`/integrations/workflows/${workflow.id}`, selected);
  // Selection URLs may already have a query, so merge without replacing selected release IDs.
  const detailURL = `${selectionURL}${details.size ? `${selectionURL.includes("?") ? "&" : "?"}${details}` : ""}`;
  const operations = Object.entries(workflow.template.unified_operations);
  const flows = operations.slice(0, 2).map(([name, operation]) => {
    // Unrecognized binding shapes cannot provide named calls for the list preview.
    const bindings = operation.bindings && typeof operation.bindings === "object" && !Array.isArray(operation.bindings)
      ? Object.entries(operation.bindings as Record<string, Record<string, unknown>>) : [];
    // A provider call is derived only from published bindings, never guessed from service requirements.
    const calls = bindings.map(([, binding]) => {
      const serviceKey = String(binding.service ?? "");
      // Owner-qualified provider references retain the short name that helps users recognize the integration.
      const provider = serviceKey.split("/").at(-1) || serviceKey;
      // Action names stay paired with their called service so this preview describes orchestration, not a provider catalogue.
      return `${String(binding.operation ?? "service call")} · ${provider}`;
    });
    return { name, calls };
  });
  return (
    <article className="grid min-w-0 grid-cols-[auto_minmax(0,1fr)_auto] gap-x-3 border-b border-slate-200 py-4 first:border-t">
      {/* Selection remains independent of navigation and can always be undone at the limit. */}
      <input type="checkbox" aria-label={`Select ${workflow.template.name}`} checked={checked}
        disabled={disabled || (!checked && selected.length >= 32)} onChange={() => onToggle(workflow.id)}
        className="mt-1 h-4 w-4 shrink-0 cursor-pointer rounded border-slate-300 accent-slate-800 disabled:opacity-40" />
      <div className="min-w-0">
        <div className="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-1">
          <Link className="font-semibold text-slate-950 underline-offset-4 hover:underline"
            preventScrollReset to={detailURL}>{workflow.template.name}</Link>
          <span className="text-[11px] text-slate-500">v{workflow.template.version}</span>
          {/* An absent category needs no placeholder that repeats the page title. */}
          {workflow.template.category && <span className="text-[11px] text-slate-500">{workflow.template.category}</span>}
        </div>
        <p className="mt-1 line-clamp-2 break-words text-sm leading-5 text-slate-600">{workflow.template.description}</p>
        <div className="mt-3 space-y-1.5" aria-label="Workflow actions and service calls">
          {flows.map((flow) => <div key={flow.name} className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 text-xs">
            <span className="text-slate-500">Action</span><code className="break-all font-medium text-slate-800">{flow.name}</code>
            {flow.calls.length > 0 && <><ArrowRight className="h-3 w-3 text-slate-400" aria-hidden="true" /><span className="text-slate-500">calls</span>
          {/* Separate multiple provider calls with punctuation while leaving their execution order unspecified. */}
          {flow.calls.map((call, index) => <span key={`${call}-${index}`} className="break-all text-slate-700">{index > 0 ? ", " : ""}{call}</span>)}</>}
          </div>)}
          {/* More actions remain reachable in the details panel without crowding the catalogue preview. */}
          {operations.length > flows.length && <p className="text-[11px] text-slate-500">and {operations.length - flows.length} more {operations.length - flows.length === 1 ? "action" : "actions"}</p>}
          {/* Templates with no callable operations should be explicit rather than appear as empty service rows. */}
          {operations.length === 0 && <p className="text-xs text-slate-500">No actions defined</p>}
        </div>
      </div>
      <Link className="mt-0.5 shrink-0 rounded p-1 text-slate-400 hover:bg-slate-100 hover:text-slate-800"
        aria-label={`View details for ${workflow.template.name}`}
        preventScrollReset to={detailURL}><ChevronRight className="h-4 w-4" /></Link>
    </article>
  );
}
