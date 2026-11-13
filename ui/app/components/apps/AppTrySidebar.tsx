import { OperationDetailsPanel, OperationDetailsHeader, OperationDetailsBody } from "~/components/OperationDetailsParts";
import { Select } from "~/components/forms/Select";
import { useEffect, useState } from "react";
import { Play, Loader2 } from "lucide-react";
import { api } from "~/lib/api";
import { appTryOperations, type TryOperation } from "~/lib/app-try-contract";
import { AppTryRun } from "./AppTryRun";

export type AppTryTarget = { versionID: string; executionID: string; familyID: string; name: string; unified: boolean };

/** Opens one ephemeral Try session; closing clears input, tokens and responses from component memory. */
export function AppTryButton({ target }: { target: AppTryTarget }) {
  const [open, setOpen] = useState(false);
  return <><button type="button" onClick={/** Starts an isolated sidebar session. */ () => setOpen(true)} className="inline-flex min-h-10 items-center justify-center gap-2 rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white shadow-sm transition-colors hover:bg-slate-800"><Play className="h-4 w-4" aria-hidden="true" />Try</button>
    {/* A new immutable target must never inherit the previous app's execution session. */}
    {open && <AppTrySidebar key={target.versionID} target={target} onClose={/** Unmounting discards transient credentials and results. */ () => setOpen(false)} />}</>;
}

/** Reuses Fused’s operation inspector shell, focus lifecycle and responsive sidebar layout. */
export function AppTrySidebar({ target, onClose }: { target: AppTryTarget; onClose: () => void }) {
  const [running, setRunning] = useState(false);
  const [operations, setOperations] = useState<TryOperation[]>([]);
  const [selected, setSelected] = useState("");
  const [error, setError] = useState("");
  const [attempt, setAttempt] = useState(0);
  // Contract reads are abortable and never survive a version switch or closed sidebar.
  useEffect(() => {
    const controller = new AbortController();
    setOperations([]); setError("");
    /** Loads the schema pinned to the displayed version without reading private app source. */
    async function load() {
      try {
        // Authored apps request only their public entry point; APIs need the operation catalogue.
        const document = await api.appConfig.openAPI(target.versionID, target.unified ? "execute" : "", controller.signal);
        const items = appTryOperations(document, target.versionID, target.unified);
        // Late responses cannot populate a newer app's form.
        if (!controller.signal.aborted) { setOperations(items); setSelected(items[0].name); }
      } catch (cause) {
        // Closing is not a load failure and should not flash a stale error.
        if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : "Could not load the operation contract.");
      }
    }
    void load();
    return /** Prevents closed sessions from receiving contract data. */ () => controller.abort();
  }, [target.versionID, target.unified, attempt]);
  const operation = operations.find(/** Keeps the rendered form tied to the selected operation. */ item => item.name === selected);
  return <OperationDetailsPanel label={`Try ${target.name}`} onClose={onClose} header={<OperationDetailsHeader badge="TRY" badgeClass="bg-slate-100 text-slate-700" identity={target.name} copyLabel="Copy app name" kind={target.unified ? "Unified App" : "REST API"} kindClass="border-slate-200 text-slate-500" onClose={onClose} closeLabel="Close Try" />}>
    <OperationDetailsBody>
        {/* Failed schema reads never reveal an unvalidated fallback form. */}
        {error ? <div role="alert" className="rounded-lg bg-red-50 p-4 text-sm text-red-800">{error}<button type="button" onClick={/** Explicit reload retries metadata only, never execution. */ () => setAttempt(/** Changes the request generation. */ value => value + 1)} className="mt-3 block font-semibold underline">Reload contract</button></div> : !operation ? <p role="status" className="flex items-center gap-2 text-sm text-slate-500"><Loader2 className="h-4 w-4 animate-spin" />Loading operation contract…</p> : <>
          {/* Hosted apps own one authored input; physical APIs expose an exact operation picker. */}
          {!target.unified && <label className="block text-sm font-semibold">Operation<Select searchable searchPlaceholder="Search operations…" disabled={running} aria-label="Operation" value={selected} onChange={/** Remounting resets the previous operation’s session. */ event => setSelected(event.target.value)} className="mt-2 w-full rounded-lg border border-slate-200 bg-white px-3 py-2.5 font-normal">{operations.map(/** Uses exact runtime operation names as selector values. */ item => <option key={item.name} value={item.name}>{item.name}</option>)}</Select></label>}
          <AppTryRun key={operation.name} target={target} operation={operation} onRunningChange={setRunning} />
        </>}
    </OperationDetailsBody>
  </OperationDetailsPanel>;
}
