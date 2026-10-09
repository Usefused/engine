import { api, type Service, type ServiceWebhookEditorSource, type DiscoveryReviewSummary, type SpecificationImportPlan } from "./api";
import { assertWebhookPlanTarget } from "./webhook-editor-import";

export interface AgentImportSource { target_type: "endpoints" | "webhooks"; source_url?: string; source_content?: string; source_mode?: "spec" | "docs" }
export interface AgentImportReview { plan: SpecificationImportPlan; baseline?: ServiceWebhookEditorSource; discovery?: DiscoveryReviewSummary }

/** Plans against the open service identity; the model cannot redirect an import to another owner or version. */
export async function prepareAgentServiceImport(service: Pick<Service, "id" | "name" | "slug">, version: string, input: AgentImportSource, signal?: AbortSignal): Promise<AgentImportReview> {
  const sourceURL = input.source_url?.trim() || "";
  const source = input.source_content || "";
  // Exactly one bounded source prevents ambiguous fetches and accidental empty replacements.
  if (!version || !["endpoints", "webhooks"].includes(input.target_type) || Boolean(sourceURL) === Boolean(source) || sourceURL.length > 8192 || new TextEncoder().encode(source).length > 4 * 1024 * 1024) throw new Error("Choose endpoints or webhooks and supply one specification URL or document (up to 4 MiB).");
  // Credentials must stay in buckets, never in a URL sent to discovery or the conversation.
  if (sourceURL) {
    const url = new URL(sourceURL);
    // Registry performs network admission; browser validation rejects obvious credential-bearing URLs first.
    if (!["http:", "https:"].includes(url.protocol) || url.username || url.password) throw new Error("Use an HTTP(S) specification URL without embedded credentials.");
  }
  // Website extraction is an explicit mode, so a failed specification parse never silently changes workflows.
  if (input.source_mode && !["spec", "docs"].includes(input.source_mode)) throw new Error("Choose spec or docs source mode.");
  // Documentation discovery currently augments webhook catalogues only.
  if (input.source_mode === "docs" && (input.target_type !== "webhooks" || !sourceURL)) throw new Error("Webhook documentation discovery requires a website URL.");
  let baseline: ServiceWebhookEditorSource | undefined;
  // Webhook-only replacement binds to an owner-readable baseline, preserving the endpoint surface.
  if (input.target_type === "webhooks") {
    const result = await api.graphql<{ serviceWebhookEditor: ServiceWebhookEditorSource }>(`query AgentImportWebhookBaseline($service_id: String!, $version: String!) {
      serviceWebhookEditor(service_id: $service_id, version: $version) { service_id service_version_id revision }
    }`, { service_id: service.id, version }, { headers: { "Cache-Control": "no-cache" } });
    baseline = result.serviceWebhookEditor;
    // A stale or mismatched owner lookup must never become a new-service import.
    if (!baseline || baseline.service_id !== service.id) throw new Error("Reload the selected service before importing webhooks.");
  }
  // Use the same discovery sessions as CLI import discover, retaining the selected owner/version guard.
  if (input.source_mode === "docs" && baseline) return discoverAgentWebhooks(service, version, sourceURL, baseline, signal);
  // Webhooks use an explicit existing destination; endpoint source versions remain authoritative and are checked below.
  const plan = await api.integrations.planImport({ name: service.name, slug: service.slug, target_type: input.target_type,
    ...(baseline ? { destination_version: version, expected_target: baseline } : { version }),
    ...(sourceURL ? { source_url: sourceURL } : { source_content: source }),
  });
  // Source-declared versions and account-scoped slugs must match the user's selected destination.
  if (plan.is_new_service || plan.service_id !== service.id || plan.target_version !== version || plan.action !== "update_version" || plan.target_type !== input.target_type) throw new Error("This plan does not target the selected service, version and import type. Nothing was applied. Choose the intended version or correct the source.");
  // Require explicit optimistic-concurrency acknowledgement for webhook edits.
  if (baseline) assertWebhookPlanTarget(plan, baseline, version);
  return { plan, baseline };
}

/** Follows linked webhook references, allowing bounded extraction correction before preparing a review without applying it. */
async function discoverAgentWebhooks(service: Pick<Service, "id" | "name" | "slug">, version: string, sourceURL: string, baseline: ServiceWebhookEditorSource, signal?: AbortSignal): Promise<AgentImportReview> {
  signal?.throwIfAborted();
  let snapshot = await api.integrations.startDiscovery({ name: service.name, slug: service.slug || "", version,
    source_url: sourceURL, source_mode: "docs", target_type: "webhooks", destination_version: version,
    // Follow reference links from overview subpages while retaining the bounded interactive page budget.
    expected_target: baseline, requested_workers: 2, crawl: { max_pages: 8, max_depth: 2 },
  });
  const sessionID = snapshot.session_id;
  const deadline = Date.now() + 10 * 60_000;
  try {
    // Poll authoritative snapshots; the model does not receive or interpret transient SSE fragments.
    while (!["awaiting_review", "error", "cancelled"].includes(snapshot.state)) {
      signal?.throwIfAborted();
      // Bound interactive work while retaining the server's resumable session and cancellation path.
      if (Date.now() > deadline) throw new Error("Webhook discovery timed out. Narrow the documentation URL and try again.");
      await discoveryPause(signal);
      const next = await api.integrations.getDiscoverySession(sessionID);
      // A stale or cross-session response cannot redirect this import to another review.
      if (next.session_id !== sessionID || next.revision < snapshot.revision) throw new Error("Discovery returned an invalid session. Nothing was applied.");
      snapshot = next;
    }
    const receipt = snapshot.payload?.contract;
    // Failed or cancelled discovery has no source-grounded contract to plan.
    if (snapshot.state !== "awaiting_review" || !receipt) throw new Error(snapshot.payload?.diagnostics?.map(item => item.message).join(" ") || "Could not extract supported webhook schemas from this website.");
    const discovery = await api.integrations.getDiscoveryReviewSummary(sessionID, receipt);
    // Bind the visible event catalogue to the exact immutable draft being planned.
    if (discovery.session_id !== sessionID || discovery.draft_id !== receipt.draft_id || discovery.review_hash !== receipt.review_hash || discovery.draft_revision !== receipt.draft_revision) throw new Error("The discovery review changed. Prepare a fresh import.");
    signal?.throwIfAborted();
    snapshot = await api.integrations.actOnDiscovery(sessionID, { version: 1, session_id: sessionID, expected_revision: snapshot.revision, draft_revision: receipt.draft_revision, action: "request_plan" });
    const plan = snapshot.payload?.import_plan;
    // Only the authoritative reviewed plan supplies diff counts and the later apply receipt.
    if (snapshot.state !== "plan_ready" || !plan || plan.plan_id !== snapshot.payload?.plan?.plan_id || plan.review_hash !== snapshot.payload?.plan?.review_hash || plan.target_type !== "webhooks") throw new Error("The Registry did not return a reviewed webhook import plan.");
    assertWebhookPlanTarget(plan, baseline, version);
    return { plan, baseline, discovery };
  } catch (cause) {
    // Cancellation stops pending model work where possible; it never applies or deletes service data.
    if (!["plan_ready", "error", "cancelled"].includes(snapshot.state)) {
      try { await api.integrations.actOnDiscovery(sessionID, { version: 1, session_id: sessionID, expected_revision: snapshot.revision, draft_revision: snapshot.draft_revision, action: "cancel" }); } catch { /* A concurrent server transition leaves a resumable session rather than permitting a stale action. */ }
    }
    throw cause;
  }
}

/** Waits between snapshot reads and releases its abort listener after each interval. */
function discoveryPause(signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    // An already-cancelled conversation must not schedule another read.
    if (signal?.aborted) { reject(signal.reason); return; }
    /** Resolves one polling interval without retaining conversation listeners. */
    const finish = () => { signal?.removeEventListener("abort", abort); resolve(); };
    /** Cancels only this interval; the caller owns cancellation of the server session. */
    const abort = () => { clearTimeout(timer); signal?.removeEventListener("abort", abort); reject(signal?.reason); };
    const timer = setTimeout(finish, 1_000);
    signal?.addEventListener("abort", abort, { once: true });
  });
}
