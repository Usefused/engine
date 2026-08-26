import { useCallback, useEffect, useRef, useState, type FormEvent, type ReactNode } from "react";
import { useNavigate } from "@remix-run/react";
import { fetchEventSource } from "@microsoft/fetch-event-source";
import { CheckCircle2, Loader2, PlayCircle, Server } from "lucide-react";
import {
  api,
  discoveryStreamURL,
  handleCredentialedResponse,
  type DiscoveryActionRequest,
  type DiscoveryEnrichmentProposal,
  type DiscoveryReviewSummary,
  type DiscoverySnapshot,
  type DiscoveredOperation,
} from "~/lib/api";
import { APIRequestError } from "~/lib/authorization-error";
import { useToast } from "~/components/Toast";
import DiscoveryReviewSummaryPanel from "~/components/DiscoveryReviewSummaryPanel";
import {
  cancelDiscoveryAction,
  discoveryReviewReceiptsMatch,
  discoveryStateMessage,
  enrichmentDecisionAction,
  initialOperationSelectionIDs,
  initialProposalSelectionIDs,
  operationSelectionID,
  parseDiscoveryEnvelope,
  parseOverlayObject,
  preferNewerSnapshot,
  requestPlanAction,
  reviewableDiscoveryReceipt,
  reviewSummaryMatchesSnapshot,
  selectOperationsAction,
  updateOverlayAction,
  visibleDiscoveryError,
} from "~/lib/extraction-wizard-protocol";

interface ExtractionWizardProps {
  sessionId: string;
  reviewOnly?: boolean;
  onClose: () => void;
  onComplete?: () => void;
}

interface DiscoveryReviewProps {
  snapshot: DiscoverySnapshot;
  selectedProposals: Set<string>;
  setSelectedProposals: (value: Set<string>) => void;
  overlayText: string;
  setOverlayText: (value: string) => void;
  submitting: boolean;
  onDecision: (action: "accept_enrichment" | "reject_enrichment") => void;
  onUpdateOverlay: () => void;
  onRequestPlan: () => void;
  reviewSummary: DiscoveryReviewSummary | null;
  reviewSummaryLoading: boolean;
  reviewSummaryError: string;
}

interface SnapshotSelectionSeeds {
  operationRevision: { current: string };
  proposalRevision: { current: string };
  setOperations: (value: Set<string>) => void;
  setProposals: (value: Set<string>) => void;
}

// seedSnapshotSelections initializes each review form once per committed revision.
function seedSnapshotSelections(snapshot: DiscoverySnapshot, seeds: SnapshotSelectionSeeds) {
  const revisionKey = `${snapshot.session_id}:${snapshot.revision}`;
  seedOperationSelections(snapshot, revisionKey, seeds);
  seedProposalSelections(snapshot, revisionKey, seeds);
}

// seedOperationSelections applies the public max_selections ceiling exactly once for this revision.
function seedOperationSelections(snapshot: DiscoverySnapshot, revisionKey: string, seeds: SnapshotSelectionSeeds) {
  if (snapshot.state !== "awaiting_selection" || seeds.operationRevision.current === revisionKey) return;
  seeds.operationRevision.current = revisionKey;
  seeds.setOperations(initialOperationSelectionIDs(snapshot.payload?.operations || [], snapshot.payload?.max_selections || 0));
}

// seedProposalSelections preserves local choices across reconnects while resetting after a draft action.
function seedProposalSelections(snapshot: DiscoverySnapshot, revisionKey: string, seeds: SnapshotSelectionSeeds) {
  if (snapshot.state !== "awaiting_review" || seeds.proposalRevision.current === revisionKey) return;
  seeds.proposalRevision.current = revisionKey;
  seeds.setProposals(initialProposalSelectionIDs(snapshot.payload?.proposals || []));
}

// discoveryFailureMessage prefers a bounded human diagnostic and otherwise exposes the automation-safe code.
function discoveryFailureMessage(snapshot: DiscoverySnapshot): string {
  const diagnostics = snapshot.payload?.diagnostics || [];
  return diagnostics.at(-1)?.message || snapshot.payload?.failure_code || "The discovery session failed safely.";
}

// ExtractionWizard drives the version-one snapshot/action protocol and leaves CLI-handoff plan application to the terminal.
export default function ExtractionWizard({ sessionId, reviewOnly = false, onClose, onComplete }: ExtractionWizardProps) {
  const navigate = useNavigate();
  const toast = useToast();
  const revisionRef = useRef(0);
  const operationSelectionRevisionRef = useRef("");
  const proposalSelectionRevisionRef = useRef("");
  const [snapshot, setSnapshot] = useState<DiscoverySnapshot | null>(null);
  const [selectedOperations, setSelectedOperations] = useState<Set<string>>(new Set());
  const [selectedProposals, setSelectedProposals] = useState<Set<string>>(new Set());
  const [overlayText, setOverlayText] = useState("{}");
  const [submitting, setSubmitting] = useState(false);
  const [sessionError, setSessionError] = useState("");
  const [transportError, setTransportError] = useState("");
  const [reviewSummary, setReviewSummary] = useState<DiscoveryReviewSummary | null>(null);
  const [reviewSummaryLoading, setReviewSummaryLoading] = useState(false);
  const [reviewSummaryError, setReviewSummaryError] = useState("");

  // commitSnapshot admits only monotonic snapshots and seeds review choices from server-owned payloads.
  const commitSnapshot = useCallback((next: DiscoverySnapshot) => {
    if (next.revision < revisionRef.current) return;
    revisionRef.current = Math.max(revisionRef.current, next.revision);
    setSnapshot((current) => preferNewerSnapshot(current, next));
    seedSnapshotSelections(next, {
      operationRevision: operationSelectionRevisionRef,
      proposalRevision: proposalSelectionRevisionRef,
      setOperations: setSelectedOperations,
      setProposals: setSelectedProposals,
    });
    if (next.state === "error") {
      setSessionError(discoveryFailureMessage(next));
    }
  }, []);

  // loadSnapshot reloads the account-scoped source of truth after reconnects and event notifications.
  const loadSnapshot = useCallback(async () => {
    const next = await api.integrations.getDiscoverySession(sessionId);
    commitSnapshot(next);
    return next;
  }, [commitSnapshot, sessionId]);

  useEffect(() => {
    const controller = new AbortController();
    let retryDelay = 2000;
    const maxRetryDelay = 60000;
    revisionRef.current = 0;
    operationSelectionRevisionRef.current = "";
    proposalSelectionRevisionRef.current = "";
    setSnapshot(null);
    setSelectedOperations(new Set());
    setSelectedProposals(new Set());
    setOverlayText("{}");
    setSessionError("");
    setTransportError("");

    // connect first restores the durable snapshot and then follows validated version-one event envelopes.
    async function connect() {
      await loadSnapshot();
      if (controller.signal.aborted) return;
      await fetchEventSource(discoveryStreamURL(sessionId), {
        credentials: "include",
        signal: controller.signal,
        async onopen(response) {
          handleCredentialedResponse(response);
          if (response.status === 401) controller.abort();
          if (!response.ok) throw new Error(`Failed to connect to discovery stream: ${response.status}`);
          setTransportError("");
          retryDelay = 2000;
          await loadSnapshot();
        },
        async onmessage(message) {
          const envelope = parseDiscoveryEnvelope(message.data, sessionId, revisionRef.current);
          if (!envelope) return;
          setTransportError("");
          await loadSnapshot();
        },
        onerror(error) {
          if (controller.signal.aborted) throw error;
          setTransportError("Connection lost. Reconnecting to discovery...");
          const delay = retryDelay;
          retryDelay = Math.min(retryDelay * 2, maxRetryDelay);
          return delay;
        },
      });
    }

    connect().catch((error) => {
      if (!controller.signal.aborted) {
        setTransportError(error instanceof Error ? error.message : "Failed to load discovery session.");
      }
    });
    return () => controller.abort();
  }, [loadSnapshot, sessionId]);

  const reviewReceipt = reviewableDiscoveryReceipt(snapshot);

  useEffect(() => {
    let cancelled = false;
    if (!snapshot || !reviewReceipt) {
      setReviewSummary(null);
      setReviewSummaryLoading(false);
      setReviewSummaryError("");
      return () => { cancelled = true; };
    }
    setReviewSummary(null);
    setReviewSummaryLoading(true);
    setReviewSummaryError("");

    // reloadAfterReviewConflict refreshes authority and reports only a conflict that survived the reload.
    async function reloadAfterReviewConflict() {
      const reloaded = await loadSnapshot().catch(() => null);
      if (!cancelled && discoveryReviewReceiptsMatch(reviewReceipt, reviewableDiscoveryReceipt(reloaded))) {
        setReviewSummaryError("The current contract summary is unavailable. Reload the discovery run before continuing.");
      }
    }

    // handleReviewSummaryFailure keeps the 409 recovery branch separate from ordinary transport failures.
    async function handleReviewSummaryFailure(error: unknown) {
      if (cancelled) return;
      if (error instanceof APIRequestError && error.status === 409) {
        // A stale receipt is never translated locally; the authoritative
        // snapshot decides whether another review fetch is permitted.
        await reloadAfterReviewConflict();
        return;
      }
      setReviewSummaryError(error instanceof Error ? error.message : "Failed to load the contract review summary.");
    }

    // loadReviewSummary accepts only a response bound to the currently displayed public receipt.
    async function loadReviewSummary() {
      try {
        const summary = await api.integrations.getDiscoveryReviewSummary(sessionId, reviewReceipt);
        if (cancelled) return;
        if (!reviewSummaryMatchesSnapshot(summary, snapshot)) {
          setReviewSummaryError("The contract summary did not match the current review receipt. Reload the discovery run.");
          return;
        }
        setReviewSummary(summary);
      } catch (error) {
        await handleReviewSummaryFailure(error);
      } finally {
        if (!cancelled) setReviewSummaryLoading(false);
      }
    }

    void loadReviewSummary();
    return () => { cancelled = true; };
  }, [loadSnapshot, reviewReceipt?.draft_id, reviewReceipt?.draft_revision, reviewReceipt?.review_hash, sessionId, snapshot?.state]);

  // runAction commits the returned snapshot and refreshes after optimistic-concurrency failures.
  async function runAction(request: DiscoveryActionRequest) {
    setSubmitting(true);
    setSessionError("");
    try {
      const next = await api.integrations.actOnDiscovery(sessionId, request);
      commitSnapshot(next);
      return next;
    } catch (error) {
      await loadSnapshot().catch(() => undefined);
      setSessionError(error instanceof Error ? error.message : "The discovery action failed.");
      return null;
    } finally {
      setSubmitting(false);
    }
  }

  // submitOperations sends the exact selected method/path allowlist without an opaque answer envelope.
  async function submitOperations(event: FormEvent) {
    event.preventDefault();
    if (!snapshot || selectedOperations.size === 0) return;
    if (selectedOperations.size > (snapshot.payload?.max_selections || 0)) {
      setSessionError("The selected operation count exceeds the Registry limit. Review the latest snapshot.");
      return;
    }
    const operations = (snapshot.payload?.operations || [])
      .filter((operation) => selectedOperations.has(operationSelectionID(operation)))
      .map(({ method, path }) => ({ method, path }));
    await runAction(selectOperationsAction(snapshot, operations));
  }

  // decideEnrichment records exact proposal IDs against the immutable draft revision.
  async function decideEnrichment(action: "accept_enrichment" | "reject_enrichment") {
    if (!snapshot || selectedProposals.size === 0) return;
    await runAction(enrichmentDecisionAction(snapshot, action, [...selectedProposals].sort()));
  }

  // updateOverlay parses structured JSON locally before invoking Registry's strict x-fused validator.
  async function updateOverlay() {
    if (!snapshot) return;
    try {
      await runAction(updateOverlayAction(snapshot, parseOverlayObject(overlayText)));
    } catch (error) {
      setSessionError(error instanceof Error ? error.message : "The Fused overlay is invalid.");
    }
  }

  // requestPlan keeps contract review and service mutation as two explicit user decisions.
  async function requestPlan() {
    if (snapshot) await runAction(requestPlanAction(snapshot));
  }

  // applyPlan invokes ordinary UI-owned apply while CLI handoffs remain terminal-owned.
  async function applyPlan() {
    // A CLI handoff owns its apply step, so the browser cannot race the waiting terminal.
    if (reviewOnly) return;
    const plan = snapshot?.payload?.plan;
    // Only the reviewed immutable plan may be submitted.
    if (!plan) return;
    setSubmitting(true);
    setSessionError("");
    try {
      const applied = await api.integrations.applyImport(plan.plan_id, plan.review_hash);
      // Engine apply already materializes and activates the exact version; a second mutation can falsely report failure.
      toast.success("Reviewed service imported.");
      // Clear URL session state before navigating so a parent search-param update cannot reopen the index.
      if (onComplete) onComplete(); else onClose();
      navigate(`/integrations/${applied.service_id}`);
    } catch (error) {
      // APIRequestError includes committed state and recovery rather than hiding the partial result.
      setSessionError(error instanceof Error ? error.message : "Failed to apply the reviewed import plan.");
    } finally {
      setSubmitting(false);
    }
  }

  // cancelDiscovery confirms the destructive session decision and uses the typed action route.
  async function cancelDiscovery() {
    if (!snapshot || snapshot.state === "cancelled" || snapshot.state === "error" || snapshot.state === "plan_ready") {
      onClose();
      return;
    }
    if (!await toast.confirm("Cancel this service discovery run?")) return;
    const next = await runAction(cancelDiscoveryAction(snapshot));
    if (next?.state === "cancelled") {
      toast.success("Discovery cancelled.");
      onClose();
    }
  }

  const error = visibleDiscoveryError(sessionError, transportError);
  return (
    <DiscoveryWizardShell reviewOnly={reviewOnly} onClose={onClose} onCancel={cancelDiscovery} submitting={submitting}>
      <DiscoveryError error={error} />
      <DiscoveryWizardContent
        snapshot={snapshot}
        selectedOperations={selectedOperations}
        setSelectedOperations={setSelectedOperations}
        selectedProposals={selectedProposals}
        setSelectedProposals={setSelectedProposals}
        overlayText={overlayText}
        setOverlayText={setOverlayText}
        submitting={submitting}
        onSubmitOperations={submitOperations}
        onDecision={decideEnrichment}
        onUpdateOverlay={updateOverlay}
        onRequestPlan={requestPlan}
        onApplyPlan={applyPlan}
        reviewOnly={reviewOnly}
        reviewSummary={reviewSummary}
        reviewSummaryLoading={reviewSummaryLoading}
        reviewSummaryError={reviewSummaryError}
      />
    </DiscoveryWizardShell>
  );
}

// DiscoveryWizardShell provides stable overlay chrome while closing preserves resumable sessions.
function DiscoveryWizardShell({ children, reviewOnly, onClose, onCancel, submitting }: {
  children: ReactNode;
  reviewOnly: boolean;
  onClose: () => void;
  onCancel: () => void;
  submitting: boolean;
}) {
  return (
    <>
      <div className="fixed inset-0 bg-slate-900/40 backdrop-blur-sm z-40" onClick={() => !submitting && onClose()} />
      <div className="fixed inset-y-0 right-0 w-full md:w-[650px] lg:w-[760px] bg-slate-50 shadow-2xl z-50 overflow-y-auto border-l border-slate-200 flex flex-col">
        <div className="p-6 border-b border-slate-200 flex items-center justify-between sticky top-0 bg-slate-50/90 backdrop-blur z-10">
          <div>
            <h1 className="text-xl font-bold text-slate-900">Review discovered service</h1>
            <p className="text-xs text-slate-500 mt-1">{reviewOnly ? "Review here, then return to the terminal to continue." : "Nothing is imported until you review and apply a plan."}</p>
          </div>
          <button data-track="cancel_discovery" type="button" onClick={onCancel} disabled={submitting} className="px-3 py-1.5 text-xs font-medium text-slate-500 hover:text-red-600 disabled:opacity-50">
            Cancel discovery
          </button>
        </div>
        <div className="flex-1 p-6">{children}</div>
      </div>
    </>
  );
}

// DiscoveryError renders durable and transport failures without replacing the current review state.
function DiscoveryError({ error }: { error: string }) {
  if (!error) return null;
  return <div className="mb-6 rounded-xl border border-red-200 bg-red-50 px-5 py-4 text-sm text-red-700"><strong>Discovery error</strong><p className="mt-1">{error}</p></div>;
}

// DiscoveryWizardContent chooses exactly one view from the closed protocol state vocabulary.
function DiscoveryWizardContent(props: {
  snapshot: DiscoverySnapshot | null;
  selectedOperations: Set<string>;
  setSelectedOperations: (value: Set<string>) => void;
  selectedProposals: Set<string>;
  setSelectedProposals: (value: Set<string>) => void;
  overlayText: string;
  setOverlayText: (value: string) => void;
  submitting: boolean;
  onSubmitOperations: (event: FormEvent) => void;
  onDecision: (action: "accept_enrichment" | "reject_enrichment") => void;
  onUpdateOverlay: () => void;
  onRequestPlan: () => void;
  onApplyPlan: () => void;
  reviewOnly: boolean;
  reviewSummary: DiscoveryReviewSummary | null;
  reviewSummaryLoading: boolean;
  reviewSummaryError: string;
}) {
  const { snapshot } = props;
  if (!snapshot) return <DiscoveryLoading message="Loading the discovery snapshot..." />;
  if (snapshot.state === "awaiting_selection") return <OperationSelection {...props} snapshot={snapshot} />;
  if (snapshot.state === "awaiting_review") return <DiscoveryReview {...props} snapshot={snapshot} />;
  if (snapshot.state === "plan_ready") return <DiscoveryPlan snapshot={snapshot} summary={props.reviewSummary} summaryLoading={props.reviewSummaryLoading} summaryError={props.reviewSummaryError} submitting={props.submitting} reviewOnly={props.reviewOnly} onApply={props.onApplyPlan} />;
  if (snapshot.state === "error" || snapshot.state === "cancelled") {
    return <DiscoveryTerminal snapshot={snapshot} />;
  }
  return <DiscoveryLoading message={discoveryStateMessage(snapshot.state)} />;
}

// DiscoveryLoading shows state-derived progress without depending on untrusted event prose.
function DiscoveryLoading({ message }: { message: string }) {
  return <div className="flex flex-col items-center justify-center py-20 text-center"><Loader2 className="w-9 h-9 text-[var(--brand-violet)] animate-spin mb-5" /><h2 className="text-lg font-semibold text-slate-900">Discovering service</h2><p className="text-sm text-slate-500 mt-2 max-w-md">{message}</p></div>;
}

// OperationSelection presents only exact shallow identities returned by the Registry snapshot.
function OperationSelection(props: {
  snapshot: DiscoverySnapshot;
  selectedOperations: Set<string>;
  setSelectedOperations: (value: Set<string>) => void;
  submitting: boolean;
  onSubmitOperations: (event: FormEvent) => void;
}) {
  const operations = props.snapshot.payload?.operations || [];
  const maxSelections = props.snapshot.payload?.max_selections || 0;
  return (
    <form onSubmit={props.onSubmitOperations} className="space-y-5">
      <ReviewHeading title="Choose operations" description={`${operations.length} exact operations were discovered. Select up to ${maxSelections} for this run.`} />
      <div className="rounded-2xl border border-slate-200 bg-white divide-y divide-slate-100 overflow-hidden">
        {operations.map((operation) => {
          const selected = props.selectedOperations.has(operationSelectionID(operation));
          return <OperationRow key={operationSelectionID(operation)} operation={operation} selected={selected} disabled={!selected && props.selectedOperations.size >= maxSelections} onToggle={(checked) => toggleSetValue(props.selectedOperations, props.setSelectedOperations, operationSelectionID(operation), checked)} />;
        })}
      </div>
      <button type="submit" disabled={props.submitting || props.selectedOperations.size === 0 || props.selectedOperations.size > maxSelections} className="w-full px-6 py-3 rounded-xl bg-slate-950 text-white disabled:opacity-50 flex items-center justify-center gap-2"><PlayCircle className="w-5 h-5" />Extract {props.selectedOperations.size} operations</button>
    </form>
  );
}

// OperationRow renders one exact method/path selection with bounded discovery context.
function OperationRow({ operation, selected, disabled, onToggle }: { operation: DiscoveredOperation; selected: boolean; disabled: boolean; onToggle: (selected: boolean) => void }) {
  return <label className={`flex gap-3 p-4 ${disabled ? "opacity-50" : "hover:bg-slate-50 cursor-pointer"}`}><input type="checkbox" checked={selected} disabled={disabled} onChange={(event) => onToggle(event.target.checked)} className="mt-1" /><div className="min-w-0"><div className="flex items-center gap-2"><span className="text-[10px] font-bold rounded bg-slate-100 px-2 py-1">{operation.method}</span><code className="text-sm break-all">{operation.path}</code></div>{operation.summary && <p className="mt-2 text-xs text-slate-500">{operation.summary}</p>}<p className="mt-1 text-[10px] text-slate-400">Found {operation.occurrences} time{operation.occurrences === 1 ? "" : "s"}</p></div></label>;
}

// DiscoveryReview exposes immutable receipt data, diagnostics, proposals, and structured overlay editing.
function DiscoveryReview(props: DiscoveryReviewProps) {
  const contract = props.snapshot.payload?.contract;
  const proposals = props.snapshot.payload?.proposals || [];
  return (
    <div className="space-y-6">
      <ReviewHeading title="Review contract draft" description="Review the immutable draft and optional Fused configuration before creating an import plan." />
      {contract && <div className="rounded-xl border border-emerald-200 bg-emerald-50 p-4"><div className="flex items-center gap-2 text-emerald-800 font-semibold"><CheckCircle2 className="w-5 h-5" />Draft revision {contract.draft_revision}</div><ReceiptRow label="Draft ID" value={contract.draft_id} /><ReceiptRow label="Review hash" value={contract.review_hash} /></div>}
      <DiscoveryReviewSummaryPanel summary={props.reviewSummary} loading={props.reviewSummaryLoading} error={props.reviewSummaryError} />
      <Diagnostics snapshot={props.snapshot} />
      <ProposalReview proposals={proposals} selected={props.selectedProposals} setSelected={props.setSelectedProposals} submitting={props.submitting} onDecision={props.onDecision} />
      <div className="rounded-xl border border-slate-200 bg-white p-4 space-y-3"><div><h3 className="font-semibold text-slate-900">Fused overlay</h3><p className="text-xs text-slate-500 mt-1">Optionally add structured x-fused-* configuration. Registry validates it and creates a new draft revision.</p></div><textarea value={props.overlayText} onChange={(event) => props.setOverlayText(event.target.value)} spellCheck={false} className="w-full h-36 rounded-lg border border-slate-300 p-3 font-mono text-xs" /><button type="button" disabled={props.submitting} onClick={props.onUpdateOverlay} className="px-4 py-2 rounded-lg border border-slate-300 text-sm disabled:opacity-50">Validate overlay</button></div>
      <button type="button" disabled={props.submitting || !contract} onClick={props.onRequestPlan} className="w-full px-6 py-3 rounded-xl bg-slate-950 text-white disabled:opacity-50">Create reviewed import plan</button>
    </div>
  );
}

// ProposalReview makes every enrichment acceptance or rejection an exact proposal-ID decision.
function ProposalReview({ proposals, selected, setSelected, submitting, onDecision }: { proposals: DiscoveryEnrichmentProposal[]; selected: Set<string>; setSelected: (value: Set<string>) => void; submitting: boolean; onDecision: (action: "accept_enrichment" | "reject_enrichment") => void }) {
  if (proposals.length === 0) return <div className="rounded-xl border border-slate-200 bg-white p-4 text-sm text-slate-500">No optional Fused enrichment was proposed.</div>;
  return <div className="rounded-xl border border-slate-200 bg-white overflow-hidden"><div className="p-4 border-b border-slate-100"><h3 className="font-semibold">Fused enrichment proposals</h3></div><div className="divide-y divide-slate-100">{proposals.map((proposal) => <label key={proposal.id} className="flex gap-3 p-4"><input type="checkbox" checked={selected.has(proposal.id)} onChange={(event) => toggleSetValue(selected, setSelected, proposal.id, event.target.checked)} className="mt-1" /><div><div className="font-mono text-xs text-slate-800">{proposal.extension} · {proposal.pointer}</div><p className="text-sm text-slate-600 mt-1">{proposal.rationale}</p><p className="text-[10px] uppercase tracking-wide text-slate-400 mt-2">{proposal.confidence} confidence · {proposal.evidence.length} evidence references</p></div></label>)}</div><div className="p-4 flex gap-3"><button type="button" disabled={submitting || selected.size === 0} onClick={() => onDecision("accept_enrichment")} className="px-4 py-2 bg-[var(--brand-violet)] text-white rounded-lg disabled:opacity-50">Accept selected</button><button type="button" disabled={submitting || selected.size === 0} onClick={() => onDecision("reject_enrichment")} className="px-4 py-2 border border-slate-300 rounded-lg disabled:opacity-50">Reject selected</button></div></div>;
}

// DiscoveryPlan shows the exact plan receipt and returns CLI handoffs to their owning terminal.
function DiscoveryPlan({ snapshot, summary, summaryLoading, summaryError, submitting, reviewOnly, onApply }: { snapshot: DiscoverySnapshot; summary: DiscoveryReviewSummary | null; summaryLoading: boolean; summaryError: string; submitting: boolean; reviewOnly: boolean; onApply: () => void }) {
  const plan = snapshot.payload?.plan;
  if (!plan) return <DiscoveryLoading message="Loading the reviewed import plan..." />;
  return <div className="space-y-6"><ReviewHeading title="Import plan ready" description="Discovery has handed off an immutable plan without changing the service catalog." /><DiscoveryReviewSummaryPanel summary={summary} loading={summaryLoading} error={summaryError} /><div className="rounded-xl border border-emerald-200 bg-emerald-50 p-5"><ReceiptRow label="Plan ID" value={plan.plan_id} /><ReceiptRow label="Review hash" value={plan.review_hash} /></div><DiscoveryPlanAction reviewOnly={reviewOnly} summaryReady={Boolean(summary)} submitting={submitting} onApply={onApply} /></div>;
}

// DiscoveryPlanAction prevents browser apply for CLI-owned handoffs while retaining the ordinary UI action.
function DiscoveryPlanAction({ reviewOnly, summaryReady, submitting, onApply }: { reviewOnly: boolean; summaryReady: boolean; submitting: boolean; onApply: () => void }) {
  if (reviewOnly) {
    return <div className="rounded-xl border border-violet-200 bg-violet-50 p-5 text-sm text-violet-900"><strong>Review complete.</strong><p className="mt-1">Return to your terminal to continue with this immutable plan.</p></div>;
  }
  return <button type="button" disabled={submitting || !summaryReady} onClick={onApply} className="w-full px-6 py-3 rounded-xl bg-slate-950 text-white disabled:opacity-50">Apply reviewed plan</button>;
}

// DiscoveryTerminal summarizes a terminal state from the authoritative snapshot.
function DiscoveryTerminal({ snapshot }: { snapshot: DiscoverySnapshot }) {
  return <div className="py-16 text-center"><Server className="w-10 h-10 mx-auto text-slate-400" /><h2 className="mt-4 text-lg font-semibold">{discoveryStateMessage(snapshot.state)}</h2><Diagnostics snapshot={snapshot} /></div>;
}

// Diagnostics renders bounded Registry explanations and never source/provider bytes.
function Diagnostics({ snapshot }: { snapshot: DiscoverySnapshot }) {
  const diagnostics = snapshot.payload?.diagnostics || [];
  if (diagnostics.length === 0) return null;
  return <div className="mt-4 rounded-xl border border-amber-200 bg-amber-50 p-4 text-left"><h3 className="text-sm font-semibold text-amber-900">Review notes</h3><ul className="mt-2 space-y-1 text-sm text-amber-800">{diagnostics.map((diagnostic, index) => <li key={`${diagnostic.code}-${index}`}>{diagnostic.message}</li>)}</ul></div>;
}

// ReviewHeading keeps review-stage titles and explanations visually consistent.
function ReviewHeading({ title, description }: { title: string; description: string }) {
  return <div><h2 className="text-lg font-semibold text-slate-900">{title}</h2><p className="text-sm text-slate-500 mt-1">{description}</p></div>;
}

// ReceiptRow renders long immutable identifiers without truncating their review value.
function ReceiptRow({ label, value }: { label: string; value: string }) {
  return <div className="mt-3"><div className="text-[10px] uppercase tracking-wide text-slate-500">{label}</div><code className="text-xs break-all text-slate-800">{value}</code></div>;
}

// toggleSetValue produces a new Set so React observes one exact selection change.
function toggleSetValue(current: Set<string>, commit: (value: Set<string>) => void, value: string, selected: boolean) {
  const next = new Set(current);
  if (selected) next.add(value); else next.delete(value);
  commit(next);
}
