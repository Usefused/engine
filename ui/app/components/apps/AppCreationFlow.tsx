import { FieldLabel } from "~/components/forms/FieldLabel";
import { useEffect, useRef, useState, type FormEvent, type ReactNode } from "react";
import { Check, AlertCircle, Loader2, Sparkles } from "lucide-react";
import { Select } from "~/components/forms/Select";
import { describeGoalWithAnswers, type DescribeAnswer, type ChooseDescribeService, type DescribeServiceCandidate, type DescribeProgress, type DescribeStage } from "~/lib/app-describe-contract";
import { AppDescriptionClarificationError } from "~/lib/unified-app-contract";

interface AppCreationFlowProps {
  onDescribe: (goal: string, progress: DescribeProgress, chooseService: ChooseDescribeService) => Promise<void>;
  children: ReactNode;
  generatesSource?: boolean;
  disabled?: boolean;
  initialManual?: boolean;
  hasSelection?: boolean;
  onBusyChange?: (busy: boolean) => void;
}

/** Shares describe, clarification, and manual review while the parent owns one persistent selection. */
export function AppCreationFlow({ onDescribe, children, generatesSource, disabled, initialManual, hasSelection, onBusyChange }: AppCreationFlowProps) {
  const [mode, setMode] = useState(initialManual ? "manual" : "describe");
  const [reviewing, setReviewing] = useState(false);
  const [goal, setGoal] = useState("");
  const [busy, setBusy] = useState(false);
  const [stage, setStage] = useState<DescribeStage>("intent");
  const [steps, setSteps] = useState<Partial<Record<DescribeStage, string>>>({});
  const [finished, setFinished] = useState(false);
  const [error, setError] = useState("");
  const [question, setQuestion] = useState("");
  const [answer, setAnswer] = useState("");
  const [answers, setAnswers] = useState<DescribeAnswer[]>([]);

  const [choice, setChoice] = useState<{ reference: string; candidates: DescribeServiceCandidate[] } | null>(null);
  const mounted = useRef(true);
  const pendingChoice = useRef<{ resolve: (id: string) => void; reject: (cause: Error) => void } | null>(null);

  // Navigation releases the suspended discovery rather than leaving a pending choice behind.
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      pendingChoice.current?.reject(new DOMException("Description cancelled", "AbortError"));
      pendingChoice.current = null;
    };
  }, []);

  /** Suspends discovery in place so choosing a publisher does not repeat intent parsing. */
  const chooseService: ChooseDescribeService = (reference, candidates) => new Promise((resolve, reject) => {
    // A late discovery response must not suspend a request in a form that has already closed.
    if (!mounted.current) { reject(new DOMException("Description cancelled", "AbortError")); return; }
    pendingChoice.current = { resolve, reject };
    setChoice({ reference, candidates });
  });

  /** Settles a choice exactly once, preserving earlier reviewed selections when cancelled. */
  function finishChoice(id?: string) {
    const pending = pendingChoice.current;
    pendingChoice.current = null;
    setChoice(null);
    // Cancellation returns to the editable goal without turning it into a service-selection error.
    if (id) pending?.resolve(id);
    else pending?.reject(new DOMException("Description cancelled", "AbortError"));
  }

  /** Retains real stage outcomes so progress remains understandable after success or failure. */
  const reportProgress: DescribeProgress = (message, nextStage) => {
    // Navigation must not revive a progress panel for an unmounted request.
    if (!mounted.current) return;
    // Older callers may report detail without changing the current structured stage.
    if (nextStage) { setStage(nextStage); setSteps((previous) => ({ ...previous, [nextStage]: message })); }
  };

  /** Replaces the proposal only after discovery succeeds, preserving the original goal and explicit answers. */
  async function runDescription(nextAnswers: DescribeAnswer[]) {
    setBusy(true); onBusyChange?.(true); setError(""); setSteps({}); setStage("intent"); setFinished(false);
    setQuestion(""); setAnswer(""); setAnswers(nextAnswers);
    try { await onDescribe(describeGoalWithAnswers(goal, nextAnswers), reportProgress, chooseService); setReviewing(true); setStage("review"); setFinished(true); }
    catch (cause) {
      // A question is an expected authoring step, not a failed app or an invitation to rewrite the whole goal.
      if (cause instanceof AppDescriptionClarificationError) setQuestion(cause.message);
      // User cancellation is an ordinary return to editing; actual failures remain visible as errors.
      else if (!(cause instanceof DOMException && cause.name === "AbortError")) setError(String(cause));
      else setSteps({});
    }
    finally { setBusy(false); onBusyChange?.(false); }
  }

  /** Starts or retries discovery using all answers already supplied for this goal. */
  function describe(event: FormEvent) { event.preventDefault(); void runDescription(answers); }

  /** Continues only after the user explicitly answers the pending business question. */
  function answerQuestion(event: FormEvent) {
    event.preventDefault();
    // Empty submissions cannot silently choose an interpretation on the user's behalf.
    if (!question || !answer.trim() || busy) return;
    void runDescription([...answers, { question, answer: answer.trim() }]);
  }

  /** Editing the original request invalidates answers that belonged to its previous intent. */
  function changeGoal(value: string) { setGoal(value); setQuestion(""); setAnswer(""); setAnswers([]); setError(""); }

  const locked = busy || Boolean(disabled);
  const showReview = [hasSelection, reviewing, mode === "manual"].some(Boolean);
  return <div className="space-y-6">
    <CreationModes mode={mode} setMode={setMode} disabled={locked} />
    {/* Switching modes preserves the description and the single parent-owned selection. */}
    {mode === "describe" && <DescribeForm goal={goal} setGoal={changeGoal} disabled={locked} onSubmit={describe} />}
    {/* Prior answers remain visible so later questions never hide what the user already decided. */}
    {mode === "describe" && answers.length > 0 && <dl className="space-y-3 rounded-lg bg-slate-50 p-4 text-sm">
      {answers.map((item, index) => <div key={index}><dt className="font-medium text-slate-700">{item.question}</dt><dd className="mt-1 whitespace-pre-wrap text-slate-600">{item.answer}</dd></div>)}
    </dl>}
    {/* Ask inline at either intent or source stage, preserving both the goal and any earlier valid draft. */}
    {mode === "describe" && question && <form onSubmit={answerQuestion} className="space-y-3 rounded-xl border border-blue-200 bg-blue-50 p-5">
      <h2 className="font-semibold text-slate-900">One detail before we continue</h2>
      <label htmlFor="app-description-answer" className="block text-sm text-slate-800">{question}</label>
      <textarea id="app-description-answer" required autoFocus maxLength={2048} disabled={locked} value={answer} onChange={(event) => setAnswer(event.target.value)} className="min-h-20 w-full rounded-lg border border-slate-300 bg-white px-3 py-2 text-sm" />
      <button type="submit" disabled={locked || !answer.trim()} className="rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white disabled:opacity-50">Continue</button>
    </form>}
    {/* Progress and clarification stay beside the editable goal, without replacing an earlier valid draft. */}
    {mode === "describe" && Object.keys(steps).length > 0 && <DescribeSteps stage={stage} steps={steps} finished={finished} failed={Boolean(error)} busy={busy && !choice} generatesSource={Boolean(generatesSource)} />}
    {/* Provider ambiguity is resolved within the same shared flow for every app type. */}
    {choice && <ServiceChoice key={choice.reference} reference={choice.reference} candidates={choice.candidates} onFinish={finishChoice} />}
    {error && <p role="alert" className="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700">{error}</p>}
    {/* Keep the picker mounted across mode changes so browsing and unfinished manual edits survive. */}
    <fieldset disabled={locked} hidden={!showReview} className="min-w-0 space-y-6">
      {hasSelection && <h2 className="text-lg font-semibold text-slate-900">Review your app</h2>}
      {children}
    </fieldset>
  </div>;
}

/** Offers two entrances to the same persistent selection, never a second builder state. */
function CreationModes({ mode, setMode, disabled }: { mode: string; setMode: (mode: string) => void; disabled: boolean }) {
  return <div role="group" aria-label="Choose how to build" className="inline-flex gap-1 rounded-lg bg-slate-100 p-1">
    {[{ id: "describe", label: "Describe" }, { id: "manual", label: "Select manually" }].map(({ id, label }) => <button key={id} type="button" aria-pressed={mode === id} disabled={disabled} onClick={() => setMode(id)} className={`rounded-md px-4 py-2 text-sm font-medium ${mode === id ? "bg-white shadow-sm" : "text-slate-500"}`}>{label}</button>)}
  </div>;
}

/** Presents the same bounded natural-language input and disclosure for every app type. */
function DescribeForm({ goal, setGoal, disabled, onSubmit }: { goal: string; setGoal: (goal: string) => void; disabled: boolean; onSubmit: (event: FormEvent) => void }) {
  return <form onSubmit={onSubmit} className="space-y-4 rounded-xl border border-slate-200 bg-white p-5 sm:p-6">
    <label htmlFor="app-goal" className="block font-semibold text-slate-900"><FieldLabel required>What would you like to build?</FieldLabel></label>
    <textarea id="app-goal" required maxLength={16384} value={goal} disabled={disabled} onChange={(event) => setGoal(event.target.value)} className="min-h-36 w-full resize-y rounded-lg border border-slate-300 px-3 py-2 text-sm focus:ring-2 focus:ring-[var(--brand-violet)]" placeholder="Describe what you want the app to do. We'll ask if anything needs clarifying." />
    <p className="text-xs leading-relaxed text-slate-500">Your description is sent to Fused Registry’s configured model. Operation discovery uses Jev. Unified App drafting also sends selected operation contracts. Provider credentials and execution data are not included.</p>
    <button type="submit" disabled={disabled || !goal.trim()} className="inline-flex items-center gap-2 rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white disabled:opacity-50"><Sparkles className="h-4 w-4" />Describe app</button>
  </form>;
}

/** Requires an explicit publisher choice before the suspended request can discover operations. */
function ServiceChoice({ reference, candidates, onFinish }: { reference: string; candidates: DescribeServiceCandidate[]; onFinish: (id?: string) => void }) {
  const [selected, setSelected] = useState("");
  /** Native required validation keeps the placeholder from becoming an implicit default provider. */
  function submit(event: FormEvent) { event.preventDefault(); onFinish(selected); }
  return <form onSubmit={submit} className="space-y-4 rounded-xl border border-slate-200 bg-white p-5 sm:p-6">
    <div>
      <h2 className="font-semibold text-slate-900">Choose a service for “{reference}”</h2>
      <p className="mt-1 text-sm text-slate-500">More than one service matches. Choose the publisher you want to use to continue your description.</p>
    </div>
    <label htmlFor="describe-service" className="block text-sm font-medium text-slate-700"><FieldLabel required>Service and publisher</FieldLabel></label>
    <Select id="describe-service" required autoFocus value={selected} onChange={(event) => setSelected(event.target.value)} className="w-full">
      <option value="" disabled>Select a service…</option>
      {candidates.map((candidate) => <option key={candidate.id} value={candidate.id}>{candidate.name} — @{candidate.provider?.handle}/{candidate.slug}</option>)}
    </Select>
    <div className="flex gap-3">
      <button type="submit" disabled={!selected} className="rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white disabled:opacity-50">Continue</button>
      <button type="button" onClick={() => onFinish()} className="rounded-lg border border-slate-200 px-4 py-2 text-sm font-medium text-slate-700">Cancel</button>
    </div>
  </form>;
}

const describeStages: Array<{ id: DescribeStage; label: string }> = [
  { id: "intent", label: "Understand request" }, { id: "services", label: "Find services" },
  { id: "operations", label: "Select operations" }, { id: "source", label: "Generate TypeScript" },
  { id: "review", label: "Prepare review" },
];

/** Gives real progress a connected step track and a prominent active detail without invented timing. */
function DescribeSteps({ stage, steps, finished, failed, busy, generatesSource }: { stage: DescribeStage; steps: Partial<Record<DescribeStage, string>>; finished: boolean; failed: boolean; busy: boolean; generatesSource: boolean }) {
  // SDK and MCP expose capabilities directly and have no source-generation stage.
  const visible = describeStages.filter((step) => generatesSource || step.id !== "source");
  const current = visible.findIndex((step) => step.id === stage);
  // Completion and failure get distinct summaries; a suspended chooser must not appear to be running.
  const title = finished ? "Ready for your review" : failed ? "Let’s resolve this" : visible[current]?.label;
  const badge = finished ? "Complete" : failed ? "Needs attention" : busy ? "Building your app" : "Your input needed";
  const StatusIcon = finished ? Check : failed ? AlertCircle : Sparkles;
  return <section aria-label="Describe progress" className="overflow-hidden rounded-2xl border border-violet-100 bg-white shadow-sm">
    <div className="flex items-start gap-4 bg-gradient-to-r from-violet-50/80 via-white to-white px-5 py-5 sm:px-6">
      <span className="flex h-11 w-11 shrink-0 items-center justify-center rounded-xl bg-[var(--brand-violet-tint,#eee9ff)] text-[var(--brand-violet)]"><StatusIcon aria-hidden="true" className="h-5 w-5" /></span>
      <div className="min-w-0 flex-1">
        <p className="text-[10px] font-semibold uppercase tracking-[0.16em] text-[var(--brand-violet)]">{badge}</p>
        <h3 className="mt-1 text-base font-semibold tracking-tight text-slate-900">{title}</h3>
        {/* Announce stage detail once, leaving decorative motion outside the live region. */}
        <p role="status" aria-live="polite" className="mt-1 text-sm leading-relaxed text-slate-500">{finished ? "Your selected operations are ready below." : steps[stage]}</p>
      </div>
    </div>
    <ol className="flex border-t border-slate-100 px-3 py-5 sm:px-5">
      {visible.map((step, index) => {
        // Advancing confirms earlier stages; a failed active stage never receives a completed check.
        const complete = finished || index < current;
        const active = !finished && index === current;
        const status = complete ? "Complete" : active && failed ? "Needs attention" : active ? "In progress" : "Waiting";
        // Emphasize only the current stage, with quiet connected checks for completed work.
        const nodeClass = complete ? "bg-violet-100 text-[var(--brand-violet)]" : active ? "bg-[var(--brand-violet)] text-white shadow-[0_0_0_4px_#eee9ff]" : "border border-slate-200 bg-white text-slate-400";
        const labelClass = active ? "font-semibold text-slate-900" : complete ? "text-slate-600" : "text-slate-400";
        return <li key={step.id} aria-current={active ? "step" : undefined} className="relative flex min-w-0 flex-1 flex-col items-center gap-3 text-center">
          {/* Connect only adjacent nodes; narrow layouts retain one evenly spaced track. */}
          {index < visible.length - 1 && <span aria-hidden="true" className={`absolute left-1/2 top-3.5 h-px w-full ${complete ? "bg-violet-200" : "bg-slate-200"}`} />}
          <span className={`relative z-10 flex h-7 w-7 items-center justify-center rounded-full text-[11px] font-semibold transition-colors ${nodeClass}`}>
            {/* Only a running stage spins, and reduced-motion users receive a static indicator. */}
            {complete ? <Check aria-hidden="true" className="h-3.5 w-3.5" /> : active && failed ? <AlertCircle aria-hidden="true" className="h-3.5 w-3.5" /> : active && busy ? <Loader2 aria-hidden="true" className="h-3.5 w-3.5 motion-safe:animate-spin" /> : <span aria-hidden="true">{index + 1}</span>}
          </span>
          <span className={`max-w-24 px-1 text-[10px] leading-4 sm:text-xs ${labelClass}`}>{step.label}<span className="sr-only">: {status}</span></span>
        </li>;
      })}
    </ol>
  </section>;
}
