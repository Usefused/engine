import { useEffect, useRef, useState } from "react";
import { ArrowUp, ChevronDown, Loader2, Sparkles } from "lucide-react";
import { TypeScriptEditor } from "~/components/code/TypeScriptEditor";
import { reviseAppSource } from "~/lib/app-describe-api";
import { describeSelectionKey, type AppServicePin } from "~/lib/app-describe-contract";

/** Proposes source-only revisions for review while preserving the app's selected services and credentials. */
export function UnifiedAppAIAssistant({ source, services, disabled, onApply, onBusyChange }: {
  source: string; services: Record<string, AppServicePin>; disabled?: boolean;
  onApply: (source: string) => void; onBusyChange: (busy: boolean) => void;
}) {
  const [open, setOpen] = useState(false);
  const [goal, setGoal] = useState("");
  const [busy, setBusy] = useState(false);
  const [proposal, setProposal] = useState("");
  const [explanation, setExplanation] = useState("");
  const [error, setError] = useState("");
  const [progress, setProgress] = useState("");
  const generation = useRef(0);
  const selection = describeSelectionKey(services);

  // A changed source or capability selection invalidates proposals and ignores late model replies.
  useEffect(() => {
    generation.current++;
    setProposal(""); setExplanation(""); setError("");
    return () => { generation.current++; };
  }, [source, selection]);

  /** Sends only the current source, exact pins, and explicitly supplied change/error context to Fused. */
  async function propose() {
    // Duplicate requests and empty instructions cannot start paid drafting work.
    if (busy || disabled || !goal.trim()) return;
    const request = ++generation.current;
    setBusy(true); onBusyChange(true); setProposal(""); setExplanation(""); setError("");
    try {
      // One prompt accepts either a requested change or error context alongside the unchanged source baseline.
      const next = await reviseAppSource(goal.trim(), services, setProgress, source);
      // A response for an abandoned draft must never overwrite the current editing session.
      if (request === generation.current) {
        // Identical source is a review finding, not an edit that should invalidate compilation.
        const changed = next.source.trim() !== "" && next.source !== source;
        setProposal(changed ? next.source : "");
        setExplanation(next.explanation || (changed ? "" : "No code changes proposed."));
      }
    } catch (cause) {
      // Failed drafting preserves the source and displays clarification or transport errors beside the request.
      if (request === generation.current) setError(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setBusy(false); onBusyChange(false);
    }
  }

  /** Applying is an explicit source edit; the parent invalidates the old compile plan before deployment. */
  function applyProposal() {
    // Only a reviewed, nonempty response can replace the source; it cannot change YAML or deploy.
    if (!proposal || busy || disabled) return;
    onApply(proposal); setProposal(""); setExplanation(""); setOpen(false);
  }

  /** Discards the complete review so stale findings cannot be mistaken for the next proposal. */
  function discardProposal() { setProposal(""); setExplanation(""); }

  const locked = busy || disabled;
  return <section className="space-y-4" aria-label="Fused AI app editor">
    <button type="button" aria-expanded={open} onClick={() => setOpen(!open)} disabled={locked || !source.trim()} className="flex w-full items-center gap-3 text-left disabled:opacity-50">
      <span className="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-violet-50 text-violet-600"><Sparkles className="h-4 w-4" /></span>
      <span className="flex-1 text-sm font-medium text-slate-900">Edit with Fused AI</span>
      <ChevronDown className={`h-4 w-4 text-slate-400 transition-transform ${open ? "rotate-180" : ""}`} />
    </button>
    {/* Keep error context in the same composer so an AI edit does not become a second configuration form. */}
    {open && <div className="space-y-5">
      <div className="overflow-hidden rounded-xl border border-slate-200 bg-white transition-colors focus-within:border-violet-300 focus-within:ring-2 focus-within:ring-violet-50">
        <label className="sr-only" htmlFor="ai-edit-prompt">Describe a change or paste an error</label>
        <textarea id="ai-edit-prompt" value={goal} onChange={(event) => setGoal(event.target.value)} disabled={locked} maxLength={16384} rows={3} placeholder="Describe a change or paste an error…" className="block min-h-24 w-full resize-y border-0 bg-transparent px-4 pt-4 pb-2 text-sm leading-6 text-slate-900 outline-none placeholder:text-slate-400 focus:ring-0 disabled:opacity-60" />
        <div className="flex flex-wrap items-center justify-between gap-3 px-3 pb-3">
          <span className="px-1 text-xs text-slate-500">Uses your code and selected services</span>
          <button type="button" onClick={propose} disabled={locked || !goal.trim()} className="ml-auto inline-flex min-h-9 items-center gap-2 rounded-lg bg-violet-50 px-3 py-2 text-sm font-medium text-violet-700 transition-colors hover:bg-violet-100 disabled:opacity-50">
            {busy ? <Loader2 className="h-4 w-4 animate-spin" /> : <ArrowUp className="h-4 w-4" />}
            {busy ? "Reviewing…" : "Suggest changes"}
          </button>
        </div>
      </div>
      {/* Progress and failures stay adjacent to the request without claiming a completed review. */}
      {busy && <p role="status" className="text-sm text-slate-500">{progress}</p>}
      {error && <p role="alert" className="text-sm text-red-700">{error}</p>}
      {/* Findings read as an assistant response; only an actual edit exposes apply controls. */}
      {(explanation || proposal) && <div className="space-y-4 border-l-2 border-violet-200 pl-4">
        <div className="flex flex-wrap items-center gap-2.5">
          <h3 className="text-sm font-medium text-slate-900">Fused AI</h3>
          <span className="rounded-md bg-slate-100 px-2 py-1 text-xs text-slate-500">{proposal ? "Changes ready" : "No code changes"}</span>
        </div>
        {explanation && <p role="status" className="whitespace-pre-wrap text-sm leading-6 text-slate-600">{explanation}</p>}
        {proposal && <div className="space-y-4">
          <div className="grid min-w-0 gap-4 lg:grid-cols-2">
            <div className="min-w-0 space-y-2"><h4 className="text-xs font-medium text-slate-500">Current code</h4><TypeScriptEditor id="ai-current-source" value={source} disabled onChange={() => { /* Review never edits the baseline. */ }} /></div>
            <div className="min-w-0 space-y-2"><h4 className="text-xs font-medium text-slate-500">Suggested code</h4><TypeScriptEditor id="ai-suggested-source" value={proposal} disabled onChange={() => { /* Changes are applied explicitly below. */ }} /></div>
          </div>
          <div className="flex flex-wrap items-center gap-3"><button type="button" disabled={locked} onClick={applyProposal} className="rounded-lg bg-violet-50 px-3 py-2 text-sm font-medium text-violet-700 hover:bg-violet-100 disabled:opacity-50">Apply to editor</button><button type="button" disabled={locked} onClick={discardProposal} className="px-3 py-2 text-sm text-slate-500 hover:text-slate-900 disabled:opacity-50">Discard</button></div>
          <p className="text-xs text-slate-500">Validate and compile after applying.</p>
        </div>}
      </div>}
    </div>}
  </section>;
}
