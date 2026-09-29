import { Activity } from "lucide-react";
import type { EngineExecutionEventEntry } from "~/lib/api";
import { unifiedAppTraceRows } from "~/lib/unified-app-trace";

// UnifiedAppTrace displays the same phase measurements emitted to OTEL using authorized durable receipt data.
export function UnifiedAppTrace({ event, consumerName }: { event: EngineExecutionEventEntry; consumerName: string }) {
  const phases = unifiedAppTraceRows(event);
  // Historical receipts cannot supply an invented validation or authored-code breakdown.
  if (phases.length === 0) return null;
  return <section className="mt-6 min-w-0" aria-label="App execution trace">
    <h4 className="flex items-center gap-2 text-xs font-semibold text-slate-950"><Activity size={14} className="text-slate-400" />Execution trace</h4>
    <div className="mt-3 overflow-hidden rounded-lg border border-slate-200">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-slate-200 bg-slate-50 px-3 py-3">
        <span className="min-w-0 break-words text-xs font-semibold text-slate-800">{consumerName} · {event.app_version}</span>
        <span className="text-xs tabular-nums text-slate-600">{event.latency_ms} ms total</span>
      </div>
      <ol className="divide-y divide-slate-100">
        {phases.map((phase) => <li key={phase.name} className="px-3 py-3">
          <div className="flex items-center justify-between gap-3 text-xs"><span className="text-slate-700">{phase.label}</span><span className="shrink-0 tabular-nums text-slate-500">{phase.duration_ms.toLocaleString(undefined, { maximumFractionDigits: 2 })} ms</span></div>
          <div className="mt-2 h-1.5 overflow-hidden rounded-full bg-slate-100" aria-hidden="true"><div className="h-full rounded-full bg-violet-500" style={{ width: `${phase.percent}%`, minWidth: "2px" }} /></div>
        </li>)}
      </ol>
    </div>
    <p className="mt-2 text-[11px] leading-5 text-slate-500">TypeScript execution includes awaited provider calls. Total time also includes startup, queueing, and saving the result.</p>
  </section>;
}
