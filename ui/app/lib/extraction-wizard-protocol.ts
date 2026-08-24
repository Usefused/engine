import type {
  DiscoveryActionRequest,
  DiscoveryContractReceipt,
  DiscoveryEnrichmentProposal,
  DiscoveryEventEnvelope,
  DiscoveryOperationSelection,
  DiscoveryReviewSummary,
  DiscoverySnapshot,
  DiscoveryState,
  DiscoveredOperation,
} from "./api";

const discoveryStates = new Set<DiscoveryState>([
  "resolve_source",
  "fetch_spec",
  "crawl_docs",
  "discover_operations",
  "awaiting_selection",
  "extract_contract",
  "enrich_contract",
  "awaiting_review",
  "plan_ready",
  "error",
  "cancelled",
]);

const discoveryEvents = new Set<DiscoveryEventEnvelope["type"]>([
  "state_changed",
  "source_candidate",
  "source_resolved",
  "crawl_progress",
  "operations_discovered",
  "selection_required",
  "extraction_progress",
  "draft_ready",
  "enrichment_proposed",
  "review_required",
  "plan_ready",
  "failed",
  "cancelled",
]);

const discoveryEventStates: Partial<Record<DiscoveryEventEnvelope["type"], DiscoveryState>> = {
  source_candidate: "resolve_source",
  source_resolved: "fetch_spec",
  crawl_progress: "crawl_docs",
  operations_discovered: "discover_operations",
  selection_required: "awaiting_selection",
  extraction_progress: "extract_contract",
  draft_ready: "enrich_contract",
  enrichment_proposed: "awaiting_review",
  review_required: "awaiting_review",
  plan_ready: "plan_ready",
  failed: "error",
  cancelled: "cancelled",
};

const discoveryStateMessages: Record<DiscoveryState, string> = {
  resolve_source: "Looking for an authoritative API specification...",
  fetch_spec: "Validating and sealing the discovered specification...",
  crawl_docs: "Reading the admitted API documentation pages...",
  discover_operations: "Finding exact operation identities...",
  awaiting_selection: "Choose the operations to include.",
  extract_contract: "Building the selected operation contracts...",
  enrich_contract: "Checking optional Fused configuration...",
  awaiting_review: "Review the immutable contract draft.",
  plan_ready: "The reviewed import plan is ready to apply.",
  error: "Discovery failed.",
  cancelled: "Discovery cancelled.",
};

// visibleDiscoveryError keeps durable workflow failures authoritative while still surfacing transport failures.
export function visibleDiscoveryError(sessionError: string, transportError: string): string {
  return sessionError || transportError;
}

// operationSelectionID renders the exact method/path identity admitted by typed Registry actions.
export function operationSelectionID(operation: DiscoveryOperationSelection): string {
  return `${operation.method}|${operation.path}`;
}

// initialOperationSelectionIDs starts with at most the explicit Registry-owned selection ceiling.
export function initialOperationSelectionIDs(operations: DiscoveredOperation[], maxSelections: number): Set<string> {
  if (!Number.isInteger(maxSelections) || maxSelections <= 0) return new Set();
  return new Set(operations.slice(0, maxSelections).map(operationSelectionID));
}

// initialProposalSelectionIDs selects proposals that explicitly require user confirmation.
export function initialProposalSelectionIDs(proposals: DiscoveryEnrichmentProposal[]): Set<string> {
  return new Set(proposals.filter((proposal) => proposal.requires_confirmation).map((proposal) => proposal.id));
}

// discoveryStateMessage maps the closed state vocabulary to stable progress copy.
export function discoveryStateMessage(state: DiscoveryState): string {
  return discoveryStateMessages[state];
}

// reviewableDiscoveryReceipt returns the exact public draft receipt only in states that permit summary reads.
export function reviewableDiscoveryReceipt(snapshot: DiscoverySnapshot | null): DiscoveryContractReceipt | null {
  if (!snapshot || (snapshot.state !== "awaiting_review" && snapshot.state !== "plan_ready")) return null;
  return snapshot.payload?.contract || null;
}

// reviewSummaryMatchesSnapshot rejects any response that is not bound to the currently displayed session and draft.
export function reviewSummaryMatchesSnapshot(summary: DiscoveryReviewSummary, snapshot: DiscoverySnapshot): boolean {
  const receipt = reviewableDiscoveryReceipt(snapshot);
  return summary.schema_version === 1
    && summary.session_id === snapshot.session_id
    && Boolean(receipt)
    && summary.draft_id === receipt?.draft_id
    && summary.draft_revision === receipt?.draft_revision
    && summary.review_hash === receipt?.review_hash;
}

// discoveryReviewReceiptsMatch identifies a 409 reload that retained the same stale receipt.
export function discoveryReviewReceiptsMatch(left: DiscoveryContractReceipt | null, right: DiscoveryContractReceipt | null): boolean {
  if (!left || !right) return false;
  return discoveryReviewReceiptKey(left) === discoveryReviewReceiptKey(right);
}

// discoveryReviewReceiptKey creates one UI-local equality key without weakening the server receipt.
function discoveryReviewReceiptKey(receipt: DiscoveryContractReceipt): string {
  return `${receipt.draft_id}:${receipt.draft_revision}:${receipt.review_hash}`;
}

// parseDiscoveryEnvelope rejects stale, cross-session, or unknown SSE values before they trigger a snapshot reload.
export function parseDiscoveryEnvelope(raw: string, sessionID: string, revision: number): DiscoveryEventEnvelope | null {
  const event = decodeDiscoveryEvent(raw);
  if (!event || !validDiscoveryEventIdentity(event, sessionID, revision)) return null;
  if (!validDiscoveryEventVocabulary(event)) return null;
  if (!discoveryEventMatchesState(event as DiscoveryEventEnvelope)) return null;
  if (!validDiscoveryEventPayload(event)) return null;
  return event as DiscoveryEventEnvelope;
}

// decodeDiscoveryEvent admits only a JSON object before field-level validation.
function decodeDiscoveryEvent(raw: string): Partial<DiscoveryEventEnvelope> | null {
  try {
    const candidate: unknown = JSON.parse(raw);
    if (!candidate || typeof candidate !== "object") return null;
    return candidate as Partial<DiscoveryEventEnvelope>;
  } catch {
    return null;
  }
}

// validDiscoveryEventIdentity enforces protocol, session, and monotonic revision binding.
function validDiscoveryEventIdentity(event: Partial<DiscoveryEventEnvelope>, sessionID: string, revision: number): boolean {
  return event.version === 1
    && event.session_id === sessionID
    && Number.isInteger(event.revision)
    && Number(event.revision) > 0
    && Number(event.revision) >= revision;
}

// validDiscoveryEventVocabulary rejects unknown states and event discriminators.
function validDiscoveryEventVocabulary(event: Partial<DiscoveryEventEnvelope>): boolean {
  return discoveryStates.has(event.state as DiscoveryState)
    && discoveryEvents.has(event.type as DiscoveryEventEnvelope["type"]);
}

// validDiscoveryEventPayload permits omission or one JSON object projection.
function validDiscoveryEventPayload(event: Partial<DiscoveryEventEnvelope>): boolean {
  return event.payload === undefined || Boolean(event.payload && typeof event.payload === "object");
}

// discoveryEventMatchesState enforces the same event/state truth table as Registry.
function discoveryEventMatchesState(event: DiscoveryEventEnvelope): boolean {
  return event.type === "state_changed" || discoveryEventStates[event.type] === event.state;
}

// preferNewerSnapshot prevents a slower GET from overwriting a revision already observed by the UI.
export function preferNewerSnapshot(current: DiscoverySnapshot | null, next: DiscoverySnapshot): DiscoverySnapshot {
  if (!current || next.revision >= current.revision) return next;
  return current;
}

// selectOperationsAction binds an exact allowlist to the currently displayed snapshot revision.
export function selectOperationsAction(snapshot: DiscoverySnapshot, operations: DiscoveryOperationSelection[]): DiscoveryActionRequest {
  return {
    version: 1,
    session_id: snapshot.session_id,
    expected_revision: snapshot.revision,
    action: "select_operations",
    payload: { operations },
  };
}

// enrichmentDecisionAction binds proposal decisions to both session and immutable draft revisions.
export function enrichmentDecisionAction(
  snapshot: DiscoverySnapshot,
  action: "accept_enrichment" | "reject_enrichment",
  proposalIDs: string[],
): DiscoveryActionRequest {
  return {
    version: 1,
    session_id: snapshot.session_id,
    expected_revision: snapshot.revision,
    draft_revision: requiredDraftRevision(snapshot),
    action,
    payload: { proposal_ids: proposalIDs },
  };
}

// updateOverlayAction submits only a structured object and leaves x-fused validation to Registry.
export function updateOverlayAction(snapshot: DiscoverySnapshot, overlay: Record<string, unknown>): DiscoveryActionRequest {
  return {
    version: 1,
    session_id: snapshot.session_id,
    expected_revision: snapshot.revision,
    draft_revision: requiredDraftRevision(snapshot),
    action: "update_overlay",
    payload: { overlay },
  };
}

// requestPlanAction asks Registry to create an immutable plan without applying it.
export function requestPlanAction(snapshot: DiscoverySnapshot): DiscoveryActionRequest {
  return {
    version: 1,
    session_id: snapshot.session_id,
    expected_revision: snapshot.revision,
    draft_revision: requiredDraftRevision(snapshot),
    action: "request_plan",
  };
}

// cancelDiscoveryAction uses the same typed action boundary as every other session mutation.
export function cancelDiscoveryAction(snapshot: DiscoverySnapshot): DiscoveryActionRequest {
  const request: DiscoveryActionRequest = {
    version: 1,
    session_id: snapshot.session_id,
    expected_revision: snapshot.revision,
    action: "cancel",
  };
  if (snapshot.draft_revision) request.draft_revision = snapshot.draft_revision;
  return request;
}

// parseOverlayObject rejects arrays and scalar JSON before Registry receives an overlay action.
export function parseOverlayObject(raw: string): Record<string, unknown> {
  const parsed: unknown = JSON.parse(raw);
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error("The Fused overlay must be a JSON object.");
  }
  return parsed as Record<string, unknown>;
}

// requiredDraftRevision fails closed rather than inventing a review identity for draft-bound actions.
function requiredDraftRevision(snapshot: DiscoverySnapshot): number {
  if (!snapshot.draft_revision) throw new Error("The discovery snapshot has no reviewable draft revision.");
  return snapshot.draft_revision;
}
