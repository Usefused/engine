import { useEffect, useRef, useState, type ReactNode } from "react";
import { Link, useLoaderData, useNavigate, useSearchParams } from "@remix-run/react";
import { X } from "lucide-react";
import { listWorkflows } from "~/lib/workflow-api";
import { workflowSelectionURL } from "~/lib/workflow-library";
import { WorkflowRequirements } from "~/components/workflows/WorkflowDetails";

import { WorkflowOperations } from "~/components/workflows/WorkflowOperations";
import { WorkflowVisibility } from "~/components/workflows/WorkflowVisibility";

/** Resolves one immutable release; unavailable dependencies cannot render an installable stale page. */
export async function clientLoader({ params }: { params: { id?: string } }) {
  // Missing identities must not fall back to an arbitrary catalogue entry.
  if (!params.id) throw new Response("Workflow not found", { status: 404 });
  return (await listWorkflows("", [params.id])).items[0];
}

/** Presents the selected workflow over the persistent catalogue before handing its selection to App Builder. */
export default function WorkflowDetailsSidebar() {
  const loaded = useLoaderData<typeof clientLoader>();
  const [workflow, setWorkflow] = useState(loaded);
  // Navigating between immutable releases resets local metadata to the newly authorized loader result.
  useEffect(() => { setWorkflow(loaded); }, [loaded]);
  const [params] = useSearchParams();
  const [activeTab, setActiveTab] = useState("operations");
  const selected = [...new Set([...params.getAll("workflow"), workflow.id])];
  const operationCount = Object.keys(workflow.template.unified_operations).length;
  const serviceCount = Object.keys(workflow.template.services).length;
  return (
    <WorkflowDrawer>
    <div className="min-w-0">
      <section className="border-b border-slate-200 bg-slate-50/60 px-5 py-6 sm:px-7">
        <div className="flex items-center gap-2 text-xs font-medium uppercase tracking-wide text-slate-500">
          <span>Workflow recipe</span><span aria-hidden="true">·</span><span>Version {workflow.template.version}</span>
        </div>
        <h2 className="mt-3 break-words text-2xl font-semibold leading-tight tracking-tight text-slate-950">{workflow.template.name}</h2>
        <p className="mt-2 break-words text-sm leading-6 text-slate-600">{workflow.template.description}</p>
        <div className="mt-4 flex flex-wrap items-center gap-3">
          <Link className="inline-flex items-center rounded-lg bg-slate-900 px-4 py-2.5 text-sm font-medium text-white hover:bg-slate-700"
            to={workflowSelectionURL("/integrations/builder", selected)}>Add to app</Link>
          <WorkflowVisibility key={workflow.id} workflow={workflow} onChanged={setWorkflow} />
        </div>
        <p className="mt-4 text-xs text-slate-500">{/* Keep count grammar natural without inventing missing category metadata. */}{operationCount} {operationCount === 1 ? "operation" : "operations"} · {serviceCount} {serviceCount === 1 ? "service" : "services"} · Published by <span className="font-medium text-slate-700">{workflow.publisher}</span>{/* Omit a missing category instead of inventing a label. */}{workflow.template.category ? ` · ${workflow.template.category}` : ""}</p>
      </section>
      <div className="px-4 py-5 sm:px-6">
        <div role="tablist" aria-label="Workflow details" className="mb-5 flex max-w-full gap-5 overflow-x-auto border-b border-slate-200">
          {[["operations", `Operations (${operationCount})`], ["requirements", "Requirements"]].map(([id, label]) => (
            // Underlined tabs keep the recipe sections distinct without adding another card layer.
            <button key={id} type="button" role="tab" id={`workflow-tab-${id}`} aria-selected={activeTab === id} aria-controls="workflow-panel" onClick={() => setActiveTab(id)}
              className={`whitespace-nowrap border-b-2 px-1 pb-3 text-sm font-semibold transition-colors ${activeTab === id ? "border-slate-800 text-slate-900" : "border-transparent text-slate-500 hover:text-slate-700"}`}>{label}</button>
          ))}
        </div>
        <div role="tabpanel" id="workflow-panel" aria-labelledby={`workflow-tab-${activeTab}`}>
          {/* Requirements explain preparation; operations show the reusable sequence. */}
          {activeTab === "requirements" ? <WorkflowRequirements workflows={[workflow]} /> : <WorkflowOperations key={workflow.id} workflow={workflow} inline />}
        </div>
      </div>
    </div>
    </WorkflowDrawer>
  );
}

/** A route-backed modal preserves deep links, native keyboard focus, and the catalogue's query and selection. */
function WorkflowDrawer({ children }: { children: ReactNode }) {
  const dialog = useRef<HTMLDialogElement>(null);
  const navigate = useNavigate();
  const [params] = useSearchParams();
  // The catalogue remains mounted while the browser traps focus inside the selected workflow.
  useEffect(() => {
    const node = dialog.current;
    node?.showModal();
    return () => node?.close();
  }, []);
  // Explicit parent navigation also works for direct links with no catalogue entry in browser history.
  function close() {
    dialog.current?.close();
    navigate(`/integrations/workflows?${params}`, { replace: true, preventScrollReset: true });
  }
  return <dialog ref={dialog} aria-labelledby="workflow-details-title" onCancel={(event) => { event.preventDefault(); close(); }}
    onClick={(event) => { /* Backdrop clicks close the route; interactions inside the panel must not dismiss it. */ if (event.target === event.currentTarget) close(); }}
    className="fixed inset-y-0 left-auto right-0 m-0 h-dvh max-h-none w-full max-w-none overflow-y-auto overflow-x-hidden border-l border-slate-200 bg-white p-0 text-slate-900 shadow-2xl backdrop:bg-slate-900/20 md:w-[600px]">
    <div className="flex min-h-full flex-col">
      <header className="sticky top-0 z-20 flex items-center justify-between border-b border-slate-100 bg-white/95 p-4 backdrop-blur sm:px-6">
        <h2 id="workflow-details-title" className="text-sm font-semibold">Workflow details</h2>
        <button type="button" aria-label="Close workflow details" onClick={close} className="rounded-full p-2 text-slate-400 hover:bg-slate-100"><X className="h-4 w-4" /></button>
      </header>
      {children}
    </div>
  </dialog>;
}

/** Failed deep links remain dismissible without replacing the working catalogue with a full-page error. */
export function ErrorBoundary() {
  return <WorkflowDrawer><div className="space-y-2 p-6" role="alert">
    <h3 className="text-lg font-semibold">Workflow unavailable</h3>
    <p className="text-sm text-slate-500">This workflow could not be loaded. Close this panel to return to the library and try again.</p>
  </div></WorkflowDrawer>;
}
