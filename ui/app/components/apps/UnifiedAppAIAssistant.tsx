import { useEffect, useRef, useState } from "react";
import { Loader2, Sparkles } from "lucide-react";
import { TypeScriptEditor } from "~/components/code/TypeScriptEditor";
import { draftAppSource } from "~/lib/app-describe-api";
import { describeSelectionKey, type AppServicePin } from "~/lib/app-describe-contract";

/** Proposes source-only revisions for review while preserving the app's selected services and credentials. */
export function UnifiedAppAIAssistant({ source, services, disabled, onApply, onBusyChange }: {
  source: string; services: Record<string, AppServicePin>; disabled?: boolean;
  onApply: (source: string) => void; onBusyChange: (busy: boolean) => void;
}) {
  const [open, setOpen] = useState(false);
  const [goal, setGoal] = useState("");
  const [failure, setFailure] = useState("");
  const [busy, setBusy] = useState(false);
  const [proposal, setProposal] = useState("");
  const [error, setError] = useState("");
  const [progress, setProgress] = useState("");
  const generation = useRef(0);
  const selection = describeSelectionKey(services);

  // A changed source or capability selection invalidates proposals and ignores late model replies.
  useEffect(() => {
    generation.current++;
    setProposal(""); setError("");
    return () => { generation.current++; };
  }, [source, selection]);

  /** Sends only the current source, exact pins, and explicitly supplied change/error context to Fused. */
  async function propose() {
    // Duplicate requests and empty instructions cannot start paid drafting work.
    if (busy || disabled || !goal.trim()) return;
    const request = ++generation.current;
    setBusy(true); onBusyChange(true); setProposal(""); setError("");
    try {
      const instructions = failure.trim() ? `${goal.trim()}\n\nReported error to investigate:\n${failure.trim()}` : goal.trim();
      const next = await draftAppSource(instructions, services, setProgress, source);
      // A response for an abandoned draft must never overwrite the current editing session.
      if (request === generation.current) setProposal(next);
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
    onApply(proposal); setProposal(""); setOpen(false);
  }

  const locked = busy || disabled;
  return <section className="space-y-4 rounded-lg border border-slate-200 bg-slate-50 p-4" aria-label="Fused AI app editor">
    <button type="button" aria-expanded={open} onClick={() => setOpen(!open)} disabled={locked || !source.trim()} className="inline-flex items-center gap-2 text-sm font-medium text-[var(--brand-violet)] disabled:opacity-50"><Sparkles className="h-4 w-4" />Edit with Fused AI</button>
    {open && <div className="space-y-4">
      <label className="block space-y-2 text-sm font-medium text-slate-700">What should Fused change?
        <textarea value={goal} onChange={(event) => setGoal(event.target.value)} disabled={locked} maxLength={8192} placeholder="Fix the checkout error while keeping the current inputs and output." className="block min-h-24 w-full rounded-lg border border-slate-300 bg-white px-3 py-2 font-normal text-slate-900" />
      </label>
      <label className="block space-y-2 text-sm font-medium text-slate-700">Error or context <span className="font-normal text-slate-500">(optional)</span>
        <textarea value={failure} onChange={(event) => setFailure(event.target.value)} disabled={locked} maxLength={6000} placeholder="Paste the error you want to fix" className="block min-h-20 w-full rounded-lg border border-slate-300 bg-white px-3 py-2 font-mono text-sm font-normal text-slate-900" />
      </label>
      <p className="text-xs text-slate-500">Uses your current code and selected provider contracts. Review the suggestion before applying.</p>
      <button type="button" onClick={propose} disabled={locked || !goal.trim()} className="inline-flex items-center gap-2 rounded-lg border border-slate-300 bg-white px-3 py-2 text-sm font-medium text-slate-700 disabled:opacity-50">{busy && <Loader2 className="h-4 w-4 animate-spin" />}{busy ? "Preparing changes…" : "Suggest changes"}</button>
      {busy && <p role="status" className="text-sm text-slate-600">{progress}</p>}
      {error && <p role="alert" className="text-sm text-red-700">{error}</p>}
      {proposal && <div className="space-y-3">
        <div className="grid min-w-0 gap-4 lg:grid-cols-2">
          <div className="min-w-0 space-y-2"><h3 className="text-sm font-medium text-slate-700">Current code</h3><TypeScriptEditor id="ai-current-source" value={source} disabled onChange={() => { /* Review never edits the baseline. */ }} /></div>
          <div className="min-w-0 space-y-2"><h3 className="text-sm font-medium text-slate-700">Suggested code</h3><TypeScriptEditor id="ai-suggested-source" value={proposal} disabled onChange={() => { /* Changes are applied explicitly below. */ }} /></div>
        </div>
        <div className="flex flex-wrap gap-3"><button type="button" disabled={locked} onClick={applyProposal} className="rounded-lg border border-violet-200 bg-violet-50 px-3 py-2 text-sm font-medium text-violet-700">Apply to editor</button><button type="button" disabled={locked} onClick={() => setProposal("")} className="px-3 py-2 text-sm text-slate-600">Discard</button></div>
        <p className="text-xs text-slate-500">Applying changes requires validation and compilation before you can deploy.</p>
      </div>}
    </div>}
  </section>;
}
