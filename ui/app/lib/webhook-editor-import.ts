import type { Service, ServiceWebhookEditorSource, SpecificationImportApplyResult, SpecificationImportPlan, SpecificationImportStatus } from "./api";
import { APIRequestError } from "./authorization-error.ts";

// Both input modes bind to the exact baseline and use the ordinary OpenAPI import contract.
export function webhookImportInput(service: Pick<Service, "name" | "slug">, version: string, baseline: ServiceWebhookEditorSource, source: string) {
  return {
    name: service.name,
    slug: service.slug,
    destination_version: version,
    target_type: "webhooks" as const,
    expected_target: { service_id: baseline.service_id, service_version_id: baseline.service_version_id, revision: baseline.revision },
    source_content: source,
    include_webhook_draft: true,
  };
}

// A server receipt must describe this existing destination before a save button can appear.
export function assertWebhookPlanTarget(plan: SpecificationImportPlan, baseline: ServiceWebhookEditorSource, version: string): void {
  // Reject creation or a mismatched destination even if an older server ignores the new precondition.
  if (plan.is_new_service || plan.service_id !== baseline.service_id || plan.target_version !== version || plan.action !== "update_version") {
    throw new Error("The reviewed import does not target this existing service version. Reload before editing.");
  }
  const expected = plan.expected_target;
  // A matching destination alone does not prove an older server enforced optimistic concurrency.
  if (!expected || expected.service_id !== baseline.service_id || expected.service_version_id !== baseline.service_version_id || expected.revision !== baseline.revision) {
    throw new Error("The server did not confirm this exact service version and baseline revision. Update the Engine and reload before editing.");
  }
}

// HTTP success alone cannot prove that the expected existing destination was updated.
export function assertWebhookApplyResult(result: SpecificationImportApplyResult, baseline: ServiceWebhookEditorSource, version: string): void {
  // Incomplete or mismatched success responses must go through durable status recovery.
  if (result.status !== "applied" || result.version !== version || result.is_new_service || !receiptAdvancesBaseline(result, baseline)) {
    throw new Error("The apply response did not confirm this service version. Check import status before saving again.");
  }
}

// Unknown transport results need ledger recovery; confirmed pre-mutation failures may be re-reviewed.
export function webhookApplyNeedsStatus(error: unknown): boolean {
  // A network exception provides no evidence that the server rolled back.
  if (!(error instanceof APIRequestError)) return true;
  return error.commitState !== "not_committed" && error.commitState !== "impossible";
}

// Only a complete matching durable receipt can turn recovery into a success message.
export function webhookStatusCommitted(status: SpecificationImportStatus, baseline: ServiceWebhookEditorSource, version: string): boolean {
  return status.status === "applied" && status.commit_state === "committed" && status.version === version && receiptAdvancesBaseline(status, baseline);
}

// Apply and recovery share exact version identity plus a strictly advancing safe-integer revision contract.
function receiptAdvancesBaseline(receipt: { service_id?: string; service_version_id?: string; revision?: number }, baseline: ServiceWebhookEditorSource): boolean {
  return receipt.service_id === baseline.service_id && receipt.service_version_id === baseline.service_version_id && Number.isSafeInteger(receipt.revision) && Number(receipt.revision) > baseline.revision;
}

// Plan validation and durable apply status use different casing for the same optimistic-concurrency conflict.
export function webhookTargetChanged(code: string | undefined): boolean {
  return code?.toUpperCase() === "IMPORT_TARGET_CHANGED";
}

// Only an exact, advancing snapshot can become the replacement baseline after an explicit owner decision.
export function assertWebhookRecoverySource(source: ServiceWebhookEditorSource, baseline: ServiceWebhookEditorSource): void {
  // Recreated versions and cached snapshots cannot silently acquire the original draft's destination identity.
  if (source.service_id !== baseline.service_id || source.service_version_id !== baseline.service_version_id || !Number.isSafeInteger(source.revision) || source.revision <= baseline.revision) {
    throw new Error("The latest snapshot did not confirm a newer revision of this exact service version. Your draft is unchanged. Try loading again; if the version was replaced, close and reopen its details.");
  }
}

// Keep meaningful server diagnostics while replacing duplicated CLI advice with the editor's explicit recovery controls.
export function webhookEditorError(error: unknown): string {
  // Typed transport metadata provides the existing slim recovery contract.
  if (error instanceof APIRequestError) {
    const message = webhookFailureMessage(error.message, error.code, error.recovery, error.remediation);
    return [message, error.code, error.phase, error.commitState, error.operationId, error.requestId].filter(Boolean).join(" · ");
  }
  return error instanceof Error ? error.message : "The webhook operation failed. Your draft has been kept.";
}

// Status and immediate failures present the same slim diagnostics without changing the shared API error envelope.
export function webhookStatusError(status: SpecificationImportStatus): string {
  const message = webhookFailureMessage(status.guidance ?? "", status.code, status.recovery);
  return [message, status.code, status.phase, status.commit_state, status.operation_id].filter(Boolean).join(" · ");
}

// Browser conflict recovery is a read-and-reconcile action, never an instruction to blindly replay a CLI command.
function webhookFailureMessage(message: string, code?: string, recovery?: string, remediation?: string): string {
  // Keep non-conflict help once, including servers whose message already contains its recovery field.
  if (!webhookTargetChanged(code)) return [message, recovery && !message.includes(recovery) ? recovery : ""].filter(Boolean).join(" ");
  for (const advice of [recovery, remediation]) {
    // Only exact, separately supplied CLI advice is removed; server explanations and diagnostics remain intact.
    if (advice?.includes("fused-cli")) message = message.replaceAll(advice, "").trim();
  }
  return message;
}
