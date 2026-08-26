import { useEffect, useState } from "react";
import { api, type Service, type ServiceWebhookEditorSource, type SpecificationImportPlan } from "~/lib/api";
import { APIRequestError } from "~/lib/authorization-error";
import { readWebhookDraft, serializeWebhookDraft, webhookEditorMaxBytes, type WebhookEditorDraft } from "~/lib/webhook-editor-draft";
import { assertWebhookApplyResult, assertWebhookPlanTarget, assertWebhookRecoverySource, webhookApplyNeedsStatus, webhookEditorError, webhookImportInput, webhookStatusCommitted, webhookStatusError, webhookTargetChanged } from "~/lib/webhook-editor-import";

const WEBHOOK_EDITOR_QUERY = `query ServiceWebhookEditor($service_id: String!, $version: String!) {
  serviceWebhookEditor(service_id: $service_id, version: $version) {
    service_id service_version_id revision source_content
  }
}`;

interface LatestWebhookDraft { source: ServiceWebhookEditorSource; draft: WebhookEditorDraft; referenceSource: string }

// Owner edits need a fresh revision rather than the Engine's ordinary short-lived catalogue cache.
async function loadWebhookEditorSource(serviceID: string, version: string): Promise<ServiceWebhookEditorSource> {
  const result = await api.graphql<{ serviceWebhookEditor: ServiceWebhookEditorSource }>(WEBHOOK_EDITOR_QUERY, { service_id: serviceID, version }, { headers: { "Cache-Control": "no-cache" } });
  return result.serviceWebhookEditor;
}

// Lazy owner-only reads and writes stay on the existing GraphQL/import transports and audit path.
export function useWebhookEditor(service: Service, version: string, onSaved: () => void) {
  const [baseline, setBaseline] = useState<ServiceWebhookEditorSource | null>(null);
  const [draft, setDraft] = useState<WebhookEditorDraft | null>(null);
  const [plan, setPlan] = useState<SpecificationImportPlan | null>(null);
  const [pendingFile, setPendingFile] = useState<{ draft: WebhookEditorDraft; plan: SpecificationImportPlan } | null>(null);
  const [busy, setBusy] = useState(true);
  const [dirty, setDirty] = useState(false);
  const [error, setError] = useState("");
  const [uncertain, setUncertain] = useState(false);
  const [staleTarget, setStaleTarget] = useState(false);
  const [latest, setLatest] = useState<LatestWebhookDraft | null>(null);
  const [retainedSource, setRetainedSource] = useState("");

  useEffect(() => {
    let active = true;
    loadWebhookEditorSource(service.id, version)
      .then((source) => {
        // Late responses cannot revive a closed drawer or a different version.
        if (!active) return;
        setDraft(readWebhookDraft(source.source_content));
        setBaseline(source);
      })
      // A retired load cannot overwrite the current editor's failure or busy state.
      .catch((failure) => { if (active) setError(webhookEditorError(failure)); })
      .finally(() => { if (active) setBusy(false); });
    return () => { active = false; };
  }, [service.id, version]);

  // Every explicit edit invalidates the reviewed receipt; applying stale source is never offered.
  function change(next: WebhookEditorDraft) {
    // Locked drafts must survive both a pending commit and a stale-target reconciliation unchanged.
    if (busy || uncertain || staleTarget) return;
    setDraft(next);
    setDirty(true);
    setPlan(null);
    setPendingFile(null);
    setError("");
  }

  // Invalid text is still an unsaved user edit and cannot retain an older apply receipt.
  function markDirty() {
    // A disabled editor cannot discard the receipt needed to recover an unknown commit.
    if (busy || uncertain || staleTarget) return;
    setDirty(true); setPlan(null);
  }

  // Stale receipts can never become valid again, so only an explicit latest-baseline choice unlocks editing.
  function markTargetChanged() { setStaleTarget(true); setLatest(null); setPlan(null); setPendingFile(null); }

  // Pre-mutation errors can offer reconciliation, while uncertain apply errors must retain their status receipt.
  function recordFailure(failure: unknown, canReconcile = true) {
    setError(webhookEditorError(failure));
    // Recognize both planner and apply codes without guessing from human-readable wording.
    if (canReconcile && failure instanceof APIRequestError && webhookTargetChanged(failure.code)) markTargetChanged();
  }

  // Loading is read-only and keeps both the original baseline and edited draft until the owner chooses otherwise.
  async function loadLatest() {
    // Never read/rebase away the receipt of an import whose commit may still be in progress.
    if (!staleTarget || !baseline || !draft || busy || uncertain) return;
    setBusy(true);
    try {
      const referenceSource = serializeWebhookDraft(draft);
      const source = await loadWebhookEditorSource(service.id, version);
      assertWebhookRecoverySource(source, baseline);
      setLatest({ source, draft: readWebhookDraft(source.source_content), referenceSource });
    } catch (failure) { setError(webhookEditorError(failure)); }
    finally { setBusy(false); }
  }

  // Deliberate replacement starts from others' current changes; the old full source is reference-only, never merged.
  function startFromLatest() {
    // A candidate must come from the guarded read and cannot bypass an unresolved mutation.
    if (!latest || !staleTarget || busy || uncertain) return;
    setRetainedSource(latest.referenceSource);
    setBaseline(latest.source); setDraft(latest.draft);
    setDirty(false); setPlan(null); setPendingFile(null); setError(""); setStaleTarget(false); setLatest(null);
  }

  // One planning function serves the builder and uploaded files, leaving parsing to Registry.
  async function requestPlan(source: string) {
    // A failed initial read cannot accidentally become an empty-catalogue replacement.
    if (!baseline) throw new Error("Reload the complete webhook draft before reviewing.");
    const reviewed = await api.integrations.planImport(webhookImportInput(service, version, baseline, source));
    assertWebhookPlanTarget(reviewed, baseline, version);
    return reviewed;
  }

  // Reviewing serializes only the preserved draft and never starts a mutation.
  async function review() {
    // A stale or unresolved import must not be replaced with another review that hides its outcome.
    if (!draft || busy || uncertain || staleTarget) return;
    setBusy(true); setError("");
    try { setPlan(await requestPlan(serializeWebhookDraft(draft))); }
    catch (failure) { recordFailure(failure); }
    finally { setBusy(false); }
  }

  // Uploaded YAML/JSON is previewed by the existing parser, never interpreted as OpenAPI in React.
  async function importFile(file: File) {
    // File preview is also planning, so it cannot bypass the same conflict/outcome lock as the builder.
    if (busy || uncertain || staleTarget) return;
    setBusy(true); setError(""); setPlan(null);
    try {
      // File admission happens before reading bytes to bound browser memory.
      if (file.size > webhookEditorMaxBytes) throw new Error("OpenAPI uploads in this editor are limited to 4 MiB.");
      const reviewed = await requestPlan(await file.text());
      // An older server cannot provide a lossless builder preview; do not infer from its diff.
      if (!reviewed.webhook_draft) throw new Error("This Engine cannot preview imported webhook definitions yet. Update it before importing here.");
      setPendingFile({ draft: readWebhookDraft(reviewed.webhook_draft.source_content), plan: reviewed });
    } catch (failure) { recordFailure(failure); }
    finally { setBusy(false); }
  }

  // Accepting replacement does not reuse its original receipt: serialization must receive a fresh review.
  function acceptFile() {
    // A pending preview cannot replace a draft while another operation owns its recovery state.
    if (!pendingFile || busy || uncertain || staleTarget) return;
    change(pendingFile.draft);
  }

  // Complete the UI only once a normal apply or matching durable status confirms the commit.
  function finish() { setDirty(false); setUncertain(false); onSaved(); }

  // Status lookup is read-only and never auto-retries a possibly committed write.
  async function checkStatus() {
    // Recovery is tied to the interrupted apply and cannot issue duplicate concurrent status reads.
    if (!plan || !baseline || !uncertain || busy) return;
    setBusy(true);
    try {
      const status = await api.integrations.importStatus(plan.plan_id);
      // Matching complete receipt is the only success condition.
      if (webhookStatusCommitted(status, baseline, version)) { finish(); return; }
      setError(webhookStatusError(status));
      // Pending or unknown states may still commit; require another status read rather than retrying.
      if (status.status === "failed" && status.commit_state === "not_committed") {
        setUncertain(false); setPlan(null);
        // A durably rejected stale target uses the same explicit reconciliation as an immediate conflict.
        if (webhookTargetChanged(status.code)) markTargetChanged();
      }
    } catch (failure) { setError(webhookEditorError(failure)); }
    finally { setBusy(false); }
  }

  // Saving uses exactly the reviewed plan/hash, with no hidden re-plan or workspace setup mutation.
  async function save() {
    // Uncertain writes must be recovered before another mutation is allowed.
    if (!plan || !baseline || uncertain || staleTarget || busy) return;
    setBusy(true); setError("");
    try { const result = await api.integrations.applyImport(plan.plan_id, plan.review_hash); assertWebhookApplyResult(result, baseline, version); finish(); }
    catch (failure) {
      const needsStatus = webhookApplyNeedsStatus(failure);
      recordFailure(failure, !needsStatus);
      setUncertain(needsStatus);
      // Never replay a failed review after a definitive precondition/permission denial.
      if (!needsStatus) setPlan(null);
    } finally { setBusy(false); }
  }

  return { draft, plan, pendingFile, busy, dirty, error, uncertain, staleTarget, latest, retainedSource, baselineRevision: baseline?.revision, change, markDirty, review, importFile, acceptFile, cancelFile: () => setPendingFile(null), save, checkStatus, loadLatest, startFromLatest };
}
