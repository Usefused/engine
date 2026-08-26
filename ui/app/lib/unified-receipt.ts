import type { EngineExecutionEventEntry, UnifiedExecutionStep } from "./api";

export interface UnifiedStepRow {
  step: UnifiedExecutionStep;
  receipt?: EngineExecutionEventEntry;
}

// receiptRequestLabel distinguishes an orchestration from a provider request without inventing a service.
export function receiptRequestLabel(event: EngineExecutionEventEntry): string {
  // Logical receipts represent several operations and have no single provider route.
  if (event.execution_kind === "unified") return "Unified operation";
  // Historical physical receipts may predate provider-request metadata.
  return [event.http_method, event.request_path].filter(Boolean).join(" ") || event.direction;
}

// unifiedStepKey separates compensation from the forward call to the same authored target.
export function unifiedStepKey(step: Pick<UnifiedExecutionStep, "target" | "phase">): string {
  return JSON.stringify([step.phase, step.target]);
}

// unifiedStepRows joins the one bounded child page without another request per target.
export function unifiedStepRows(parent: EngineExecutionEventEntry, children: EngineExecutionEventEntry[]): UnifiedStepRow[] {
  const indexed = new Map<string, EngineExecutionEventEntry>();
  // Correlation is server-owned; unrelated or legacy receipts cannot become clickable target evidence.
  for (const child of children) {
    if (child.parent_execution_id === parent.id && child.unified_target && child.execution_phase) {
      indexed.set(unifiedStepKey({ target: child.unified_target, phase: child.execution_phase }), child);
    }
  }
  // Absent historical metadata stays empty rather than inferring an execution graph from provider calls.
  return (parent.unified_steps ?? []).map((step) => ({ step, receipt: indexed.get(unifiedStepKey(step)) }));
}

// unifiedPhaseSummary counts logical outcomes, not provider attempts or billable executions.
export function unifiedPhaseSummary(rows: UnifiedStepRow[], phase: UnifiedExecutionStep["phase"]): string {
  const counts = { success: 0, error: 0, skipped: 0 };
  // A rollback cannot erase the failed forward step, so each phase has independent counts.
  for (const { step } of rows) {
    if (step.phase === phase) counts[step.status] += 1;
  }
  // Compensation has no scheduler-skipped outcome in the canonical receipt contract.
  if (phase === "rollback") return `${counts.success} succeeded · ${counts.error} failed`;
  return `${counts.success} succeeded · ${counts.error} failed · ${counts.skipped} skipped`;
}

// unifiedStepStatus uses the existing receipt vocabulary while keeping scheduler skips distinct.
export function unifiedStepStatus(status: UnifiedExecutionStep["status"]): { label: string; className: string } {
  const states = {
    success: { label: "Successful", className: "text-emerald-700" },
    error: { label: "Failed", className: "text-red-700" },
    skipped: { label: "Skipped", className: "text-slate-500" },
  };
  return states[status];
}
