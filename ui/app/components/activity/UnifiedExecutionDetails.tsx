import { unifiedAppTraceRows } from "~/lib/unified-app-trace";
import { unifiedAppOutcome } from "~/lib/unified-app-outcome";
import { UnifiedAppTrace } from "~/components/activity/UnifiedAppTrace";
import { CheckCircle2, ChevronRight, Loader2, Server, XCircle } from "lucide-react";
import type { EngineExecutionEventEntry, UnifiedExecutionStep } from "~/lib/api";
import { unifiedPhaseSummary, unifiedStepKey, unifiedStepStatus, type UnifiedStepRow } from "~/lib/unified-receipt";

interface UnifiedExecutionDetailsProps {
  event: EngineExecutionEventEntry;
  consumerName: string;
  rows: UnifiedStepRow[];
  loading: boolean;
  unavailable: boolean;
  onRetry: () => void;
  onSelect: (receipt: EngineExecutionEventEntry) => void;
}

// ReceiptField matches the existing inspector's typography without treating logical metadata as a provider response.
function ReceiptField({ label, value }: { label: string; value: string }) {
  return <div className="min-w-0"><dt className="text-[11px] text-slate-400">{label}</dt><dd className="mt-1 break-words text-xs text-slate-700">{value}</dd></div>;
}

// OperationReceiptMetadata shows useful provider metadata without depending on access to raw diagnostics.
function OperationReceiptMetadata({ receipt }: { receipt?: EngineExecutionEventEntry }) {
  // A missing child receipt must stay distinct from an operation that never ran.
  if (!receipt) return <p className="mt-1 text-xs text-slate-500">Physical receipt not available</p>;
  return <>
    <p className="mt-1 text-xs text-slate-500">{receipt.service_name || receipt.service_slug || "Service metadata unavailable"} · {receipt.latency_ms} ms</p>
    {/* HTTP status is ordinary execution metadata; provider response bodies remain private. */}
    {receipt.provider_http_status ? <p className="mt-1 text-xs text-slate-500">HTTP {receipt.provider_http_status} · {receipt.attempt_count} attempt(s)</p> : null}
  </>;
}

// UnifiedStep names dispatched operations from their receipt and keeps missing provider evidence explicit.
function UnifiedStep({ row, onSelect }: { row: UnifiedStepRow; onSelect: UnifiedExecutionDetailsProps["onSelect"] }) {
  const { step, receipt } = row;
  const state = unifiedStepStatus(step.status);
  // Only stored physical receipts are interactive; a skipped or mapping-failed target has no fabricated detail view.
  const content = <>
    <div className="min-w-0 flex-1">
      {/* Receipt operation names help ordinary readers identify the failed call without opening raw payloads. */}
      <div className="break-words font-mono text-xs font-medium text-slate-950">{receipt?.operation || step.target}</div>
      {/* A compensation is associated with its authored forward target, not a second logical success. */}
      {step.phase === "rollback" ? <p className="mt-1 text-xs text-slate-500">Compensates {step.target}</p> : null}
      <OperationReceiptMetadata receipt={receipt} />
    </div>
    <span className={`shrink-0 text-xs font-medium ${state.className}`}>{state.label}</span>
    {receipt ? <ChevronRight className="h-4 w-4 shrink-0 text-slate-400" /> : null}
  </>;
  // Keep the complete target row keyboard-accessible when authoritative detail is available.
  if (receipt) return <li><button type="button" aria-label={`Inspect ${step.phase} execution ${step.target}`} onClick={() => onSelect(receipt)} className="flex w-full items-start gap-3 px-3 py-4 text-left transition-colors hover:bg-slate-50 focus:bg-slate-50 focus:outline-none">{content}</button></li>;
  return <li className="flex items-start gap-3 px-3 py-4">{content}</li>;
}

// UnifiedPhase keeps authored order visible and reports forward and compensation outcomes separately.
function UnifiedPhase({ phase, rows, onSelect }: { phase: UnifiedExecutionStep["phase"]; rows: UnifiedStepRow[]; onSelect: UnifiedExecutionDetailsProps["onSelect"] }) {
  // These bounded in-memory rows are the already-authorized parent's metadata, not a fetched database filter.
  const phaseRows = rows.filter(({ step }) => step.phase === phase);
  // Absence of compensation is meaningful and must not look like missing provider telemetry.
  const title = phase === "forward" ? "Forward executions" : "Rollback executions";
  return <section className="mt-6" aria-label={title}>
    <h4 className="flex items-center gap-2 text-xs font-semibold text-slate-950"><Server className="h-3.5 w-3.5 text-slate-400" />{title}</h4>
    <p className="mt-2 text-xs text-slate-500">{unifiedPhaseSummary(rows, phase)}</p>
    {/* Empty rollback history is explicitly different from a successful compensation. */}
    {phaseRows.length === 0 ? <p className="mt-3 text-xs text-slate-500">No {phase} executions recorded.</p> : <ol className="mt-3 divide-y divide-slate-100 rounded-lg border border-slate-200">{phaseRows.map((row) => <UnifiedStep key={unifiedStepKey(row.step)} row={row} onSelect={onSelect} />)}</ol>}
  </section>;
}

// UnifiedExecutionDetails explains the ordinary outcome independently of permission-gated raw diagnostics.
export function UnifiedExecutionDetails({ event, consumerName, rows, loading, unavailable, onRetry, onSelect }: UnifiedExecutionDetailsProps) {
  // The parent's outcome remains failed even when all required compensation succeeds.
  const successful = event.status === "success";
  const hosted = unifiedAppTraceRows(event).length > 0;
  const outcome = unifiedAppOutcome(event);
  return <section className="border-y border-slate-200 py-6" aria-label="Unified execution receipt details">
    <div className="flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between">
      <div><div className="text-xs font-medium text-blue-700">Unified execution receipt</div><h3 className="mt-1 break-words font-mono text-sm font-semibold text-slate-950">{event.operation}</h3></div>
      <span className={`inline-flex items-center gap-1.5 text-xs font-medium ${successful ? "text-emerald-700" : "text-red-700"}`}>
        {/* Outcome colors and icons match ordinary execution receipts. */}
        {successful ? <CheckCircle2 className="h-3.5 w-3.5" /> : <XCircle className="h-3.5 w-3.5" />}{successful ? "Successful" : "Failed"}
      </span>
    </div>
    {/* Fixed explanations remain available to every authorized receipt reader without loading private details. */}
    <div className={`mt-4 rounded-lg border p-3 text-xs ${successful ? "border-emerald-200 bg-emerald-50 text-emerald-900" : "border-red-200 bg-red-50 text-red-900"}`}>
      <p className="font-semibold">{outcome.title}</p>
      <p className="mt-1">{outcome.message} {outcome.action}</p>
    </div>
    <dl className="mt-6 grid grid-cols-2 gap-4"><ReceiptField label="App" value={consumerName} /><ReceiptField label="Version" value={event.app_version || "Not recorded"} /><ReceiptField label="Access path" value={event.transport.toUpperCase()} /><ReceiptField label="Total elapsed" value={`${event.latency_ms} ms`} /></dl>
    <p className="mt-4 text-xs leading-5 text-slate-500">Operation results are shown below.</p>
    <UnifiedAppTrace event={event} consumerName={consumerName} />
    {/* Loading and unavailable states cannot silently imply that every child was skipped. */}
    {loading ? <p role="status" className="mt-4 flex items-center gap-2 text-xs text-slate-500"><Loader2 className="h-3.5 w-3.5 animate-spin" />Loading execution receipts…</p> : null}
    {unavailable ? <p role="status" className="mt-4 text-xs text-slate-500">Child receipts are unavailable.</p> : null}
    <p className="mt-3 text-xs text-slate-500">Receipt delivery may lag behind completion. <button type="button" disabled={loading} className="font-medium text-blue-700 hover:underline disabled:opacity-50" onClick={onRetry}>Refresh receipts</button></p>
    <UnifiedPhase phase="forward" rows={rows} onSelect={onSelect} />
    {/* Hosted TypeScript has no implicit rollback scheduler to report. */}
    {!hosted ? <UnifiedPhase phase="rollback" rows={rows} onSelect={onSelect} /> : null}
    <dl className="mt-6 grid gap-4 border-t border-slate-200 pt-5 sm:grid-cols-3"><ReceiptField label="Started" value={new Date(event.started_at).toLocaleString()} /><ReceiptField label="Trace ID" value={event.trace_id || "Not recorded"} /><ReceiptField label="Receipt ID" value={event.id} /></dl>
  </section>;
}
