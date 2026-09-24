import { useEffect, useRef, useState } from "react";
import { ChevronDown, Globe2, Loader2, Lock } from "lucide-react";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";
import { setWorkflowVisibility } from "~/lib/workflow-api";
import type { Workflow } from "~/lib/workflow-library";

/** Centralizes visibility labels and switch colors so every control reflects the same persisted state. */
function visibilityAppearance(visible: boolean) {
  // Publication changes presentation only after the server returns its saved value.
  return visible ? {
    label: "Public workflow", badge: "bg-slate-100 text-slate-600", track: "bg-slate-900", thumb: "translate-x-4",
    description: "Discoverable outside your workspace.", icon: <Globe2 className="h-4 w-4 text-slate-500" />,
  } : {
    label: "Private workflow", badge: "bg-slate-100 text-slate-600", track: "bg-slate-300", thumb: "translate-x-0.5",
    description: "Only your workspace can discover it.", icon: <Lock className="h-4 w-4 text-slate-500" />,
  };
}

/** Matches the service visibility menu while requiring both Registry ownership and Engine catalogue management. */
export function WorkflowVisibility({ workflow, onChanged }: { workflow: Workflow; onChanged: (workflow: Workflow) => void }) {
  const appearance = visibilityAppearance(workflow.public);
  const { access } = useCurrentActorAccess();
  const [open, setOpen] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const container = useRef<HTMLDivElement>(null);
  const canManage = workflow.is_owner === true && hasWorkspacePermission(access, "catalogue.manage");
  // Outside clicks and Escape dismiss this header menu without affecting the workflow selection.
  useEffect(() => {
    function dismiss(event: PointerEvent) { /* Keep switch interactions inside the menu active. */ if (!container.current?.contains(event.target as Node)) setOpen(false); }
    function escape(event: KeyboardEvent) { /* Escape only dismisses disclosure state, never submits a visibility change. */ if (event.key === "Escape") setOpen(false); }
    document.addEventListener("pointerdown", dismiss);
    document.addEventListener("keydown", escape);
    return () => { document.removeEventListener("pointerdown", dismiss); document.removeEventListener("keydown", escape); };
  }, []);
  // The server rechecks ownership and dependencies; failed changes retain the current visible state.
  async function toggle() {
    setSaving(true); setError("");
    try { onChanged(await setWorkflowVisibility(workflow.id, !workflow.public)); }
    catch (cause) { setError(cause instanceof Error ? cause.message : "Could not update workflow visibility."); }
    finally { setSaving(false); }
  }
  // Readers see discoverability but never an actionable publication control.
  if (!canManage) return <span className={`rounded px-2 py-1 text-xs font-medium ${appearance.badge}`}>{appearance.label}</span>;
  return <div className="relative" ref={container}>
    <button type="button" aria-haspopup="dialog" aria-expanded={open} onClick={() => setOpen(!open)} className="inline-flex h-9 items-center gap-2 rounded-md border border-slate-200 bg-white px-3 text-sm font-medium text-slate-700 shadow-sm hover:bg-slate-50">
      {/* Visibility reflects the saved Registry release, not an optimistic local toggle. */}
      {appearance.icon}
      {appearance.label}<ChevronDown className="h-3.5 w-3.5 text-slate-400" />
    </button>
    {/* The owner explicitly opens publication controls; catalogue reads never mutate visibility. */}
    {open && <div role="dialog" aria-label="Workflow visibility" className="absolute left-0 top-full z-30 mt-2 w-[min(22rem,calc(100vw-2rem))] rounded-md border border-slate-200 bg-white p-4 shadow-xl">
      <h2 className="text-sm font-semibold text-slate-900">Visibility</h2>
      <div className="mt-3 flex items-start justify-between gap-4">
        <div><p className="text-sm font-medium text-slate-800">Workflow release</p><p className="mt-0.5 text-xs leading-5 text-slate-500">{/* Private authoring remains workspace-only until an owner explicitly publishes it. */}{appearance.description}</p></div>
        <button type="button" role="switch" aria-label="Make workflow public" aria-checked={workflow.public} disabled={saving} onClick={toggle} className={`relative mt-0.5 inline-flex h-5 w-9 shrink-0 rounded-full disabled:opacity-50 ${appearance.track}`}>
          {/* The switch thumb follows confirmed visibility, including a rejected publication attempt. */}
          <span className={`absolute top-0.5 h-4 w-4 rounded-full bg-white shadow-sm transition-transform ${appearance.thumb}`} />
        </button>
      </div>
      <p className="mt-3 border-t border-slate-100 pt-3 text-xs leading-5 text-slate-500">All required services and their pinned versions must be public. Existing installed apps are unchanged.</p>
      {/* Mutation progress and failures stay beside the switch that initiated them. */}
      {saving && <p role="status" className="mt-2 flex items-center gap-2 text-xs text-slate-500"><Loader2 className="h-3 w-3 animate-spin" />Saving visibility…</p>}
      {error && <p role="alert" className="mt-2 text-xs leading-5 text-red-700">{error}</p>}
    </div>}
  </div>;
}
