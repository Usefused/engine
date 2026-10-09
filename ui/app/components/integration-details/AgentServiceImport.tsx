import DiscoveryReviewSummaryPanel from "~/components/DiscoveryReviewSummaryPanel";
import { useEffect, useRef, useState } from "react";
import { api, type Service } from "~/lib/api";
import { prepareAgentServiceImport, type AgentImportSource, type AgentImportReview } from "~/lib/fused-agent-import";
import { webhookApplyNeedsStatus } from "~/lib/webhook-editor-import";
import { useFusedAgent } from "~/components/agent/FusedAgentContext";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { canEditWebhook } from "~/lib/webhook-editor-draft";
import { useToast } from "~/components/Toast";

/** Keeps the human-owned import diff and persistence actions visible in a compact review. */
export function AgentServiceImport({ service, version, onSaved }: { service: Service; version: string; onSaved: () => void }) {
  const agent = useFusedAgent();
  const register = agent?.registerServiceImport;
  const { access } = useCurrentActorAccess();
  const allowed = canEditWebhook(service.is_owner, access);
  const toast = useToast();
  const [review, setReview] = useState<AgentImportReview | null>(null);
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [uncertain, setUncertain] = useState(false);
  const [error, setError] = useState("");
  const locked = useRef(false);
  const mounted = useRef(true);
  // Route/version changes must not reveal or install results from a retired target.
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  useEffect(() => register?.({ name: service.name, version, available: allowed && !busy && !review,
    /** Produces a plan for this exact page without giving the agent a persistence action. */
    prepare: async (input: AgentImportSource, signal: AbortSignal) => {
      // Synchronous locking closes the gap before React publishes a busy bridge.
      if (!allowed) throw new Error("Only a service owner with catalogue import permission can prepare this import.");
      // Existing reviews stay under user control instead of being overwritten by a follow-up.
      if (locked.current || review) throw new Error("Finish or discard the current import review first.");
      locked.current = true; setBusy(true); setError("");
      try {
        const next = await prepareAgentServiceImport(service, version, input, signal);
        signal.throwIfAborted();
        // A completed fetch is not permission to update a detached service page.
        if (!mounted.current) throw new Error("The service page changed. Prepare a new review on the current page.");
        setReview(next);
        return { prepared: true, applied: false, service: service.name, version, target_type: next.plan.target_type, diff: next.plan.diff, reviewed_event_names: next.discovery?.webhooks?.map(event => event.path), next: "Review the import panel. The user must click Apply import to save it. Reviewed event names include preserved existing events. Documentation imports preserve verification settings; absence of a named scheme does not establish unsigned delivery. Check status appears only after an uncertain apply, not during normal review." };
      } finally { locked.current = false; /* An unmounted editor cannot publish busy state. */ if (mounted.current) setBusy(false); }
    },
  }), [register, service, version, allowed, busy, review]);

  /** Applies only the visible receipt after the user's ordinary confirmation prompt. */
  async function apply() {
    // Unknown outcomes must be resolved from the ledger before any retry.
    if (!allowed || !review || locked.current || uncertain) return;
    locked.current = true; setBusy(true); setError("");
    let submitted = false;
    try {
      const plan = review.plan;
      // The confirmation names destructive diff counts before the sole persistent action.
      setConfirming(true);
      const confirmed = await toast.confirm(`Import ${plan.target_type} into ${service.name} ${version}: ${plan.diff.added} added, ${plan.diff.changed} changed, ${plan.diff.removed} removed?`);
      setConfirming(false);
      // Dismissing the prompt never submits the reviewed receipt.
      if (!confirmed) return;
      submitted = true;
      const result = await api.integrations.applyImport(plan.plan_id, plan.review_hash);
      // A receipt for a different destination is not a successful update of this service.
      if (result.status !== "applied" || result.service_id !== service.id || result.version !== version || result.is_new_service) throw new Error("The import result could not be confirmed. Check its status.");
      setReview(null); onSaved(); toast.success("Service import applied.");
    } catch (cause) {
      // Network failures after submission can hide a commit; never enable blind reapplication.
      if (submitted && webhookApplyNeedsStatus(cause)) setUncertain(true);
      setError(cause instanceof Error ? cause.message : "Import failed.");
    } finally { locked.current = false; setBusy(false); setConfirming(false); }
  }

  /** Resolves an ambiguous apply through the existing durable import ledger without retrying it. */
  async function checkStatus() {
    // Only an unresolved receipt has an outcome to recover.
    if (!review || locked.current || !allowed) return;
    locked.current = true; setBusy(true);
    try {
      const status = await api.integrations.importStatus(review.plan.plan_id);
      // The ledger must confirm this destination before reporting success.
      if (status.status === "applied" && status.commit_state === "committed" && status.service_id === service.id && status.version === version) {
        setReview(null); setUncertain(false); setError(""); onSaved(); toast.success("Service import applied.");
      } else {
        setError(status.guidance || "The import outcome is not yet confirmed. Check again before making another import.");
        // Definitive no-commit outcomes allow discarding and preparing a fresh receipt, never silent retries.
        if (["not_committed", "impossible"].includes(status.commit_state)) { setUncertain(false); setReview(null); }
      }
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Could not check import status."); }
    finally { locked.current = false; setBusy(false); }
  }

  /** Discards only an unsubmitted review; unresolved commits retain their status recovery control. */
  function discard() {
    // A pending or uncertain apply cannot be mistaken for a cancelled mutation.
    if (locked.current || uncertain) return;
    setReview(null); setError("");
  }

  // Losing import permission removes both review and action controls immediately.
  if (!allowed) return null;
  // Idle pages keep their normal appearance until the assistant prepares a review.
  if (!review) return <>{busy && <p role="status">Preparing service import…</p>}{error && <p role="alert" className="text-sm text-red-700">{error}</p>}</>;
  return <>
    <div className="fixed inset-0 z-40 bg-slate-900/20" aria-hidden="true" />
    <aside data-fused-detail-sidebar data-fused-workspace-dialog role="dialog" aria-label="Review service import" aria-modal={!agent?.isOpen && !confirming} className="fixed inset-y-0 right-0 z-50 flex w-full max-w-lg flex-col border-l border-slate-200 bg-white shadow-xl">
      <header className="shrink-0 border-b border-slate-200 px-6 py-5">
        <div className="flex items-start justify-between gap-4">
          <div className="min-w-0"><h2 className="text-lg font-semibold tracking-tight text-slate-900">Review import</h2><p className="mt-1 break-words text-sm text-slate-500">{service.name} <span className="text-slate-300">/</span> {version}</p></div>
        </div>
        <div className="mt-4 flex flex-wrap gap-x-4 gap-y-1 text-sm" aria-label="Import changes">
          <span className="font-medium text-emerald-700">+{review.plan.diff.added} added</span>
          <span className="text-slate-500">{review.plan.diff.changed} changed</span>
          {/* Destructive changes deserve emphasis even when most of the import is additive. */}
          <span className={review.plan.diff.removed > 0 ? "font-medium text-red-700" : "text-slate-500"}>{review.plan.diff.removed} removed</span>
        </div>
      </header>
      {/* Narrow side gutters give long webhook names more room without moving the header or actions. */}
      <div className="min-h-0 flex-1 space-y-4 overflow-auto px-3 py-6 text-sm">
        {/* An interrupted response cannot prove the import did not commit. */}
        {uncertain && <p role="status" className="rounded-lg bg-amber-50 p-3 text-amber-900">The import may have been applied. Check its status before continuing.</p>}
        {/* Website imports expose the source-derived event catalogue before the user saves it. */}
        {review.discovery && <DiscoveryReviewSummaryPanel summary={review.discovery} surface="webhooks" embedded loading={false} error="" />}
        {/* Replacement semantics must remain visible even when a request was phrased as adding one operation. */}
        {review.plan.diff.removed > 0 && <p className="rounded-lg bg-amber-50 p-3 text-amber-900">This import removes existing definitions. To keep them, discard this plan and include them in the source.</p>}
        <p className="text-xs leading-relaxed text-slate-500">Updates this version’s {review.plan.target_type} definitions. Receiving URLs and credentials stay unchanged.</p>
        {/* Show authoritative names when provided without inventing detail from counts. */}
        {review.plan.diff.changed_names?.length ? <p>Changed: {review.plan.diff.changed_names.join(", ")}</p> : null}
        {review.plan.diff.removed_names?.length ? <p>Removed: {review.plan.diff.removed_names.join(", ")}</p> : null}
        {/* Preserve actionable failures beside the exact review they belong to. */}
        {error && <p role="alert" className="text-red-700">{error}</p>}
      </div>
      <footer className="flex shrink-0 justify-end gap-3 border-t border-slate-200 bg-white px-6 py-4 pb-[max(1rem,env(safe-area-inset-bottom))]">
        <button type="button" disabled={busy || uncertain} onClick={discard} className="rounded-lg border border-slate-200 px-3 py-2 text-sm disabled:opacity-50">Discard</button>
        {/* An unknown commit exposes only status recovery, never another apply. */}
        {uncertain ? <button type="button" disabled={busy} onClick={checkStatus} className="rounded-lg bg-slate-950 px-3 py-2 text-sm text-white disabled:opacity-50">Check status</button> : <button type="button" disabled={busy} onClick={apply} className="rounded-lg bg-slate-950 px-3 py-2 text-sm text-white disabled:opacity-50">{confirming ? "Awaiting confirmation…" : busy ? "Applying…" : "Apply import"}</button>}
      </footer>
    </aside>
  </>;
}
