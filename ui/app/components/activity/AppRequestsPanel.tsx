import { useEffect, useState } from "react";
import { ChevronLeft, ChevronRight, Clock, Loader2 } from "lucide-react";
import { AppExecutionInspector } from "~/components/activity/AppExecutionInspector";
import { activateReceiptRow } from "~/components/activity/receiptRow";
import { api, type EngineExecutionEventEntry } from "~/lib/api";
import { appActivityIssue, type AppActivityIssue } from "~/lib/app-activity-error";
import { receiptRequestLabel } from "~/lib/unified-receipt";

type RequestStatus = "all" | "success" | "failed";

interface AppRequestsPanelProps {
  appId: string;
  consumerName: string;
  transport: "sdk" | "mcp";
}

const PAGE_SIZE = 10;
// requestLabel selects the most specific bounded identity recorded on a receipt.
function requestLabel(event: EngineExecutionEventEntry): string {
  return event.operation || event.event_name || event.request_path || "Request";
}

// RequestRow makes the complete table row an accessible receipt-inspection control.
function RequestRow({ event, onSelect }: { event: EngineExecutionEventEntry; onSelect: (event: EngineExecutionEventEntry) => void }) {
  const failed = event.status === "failed";
  return (
    <tr role="button" tabIndex={0} aria-haspopup="dialog" aria-label={`Inspect ${requestLabel(event)}`} onClick={() => onSelect(event)} onKeyDown={(keyboardEvent) => activateReceiptRow(keyboardEvent, () => onSelect(event))} className="cursor-pointer border-t border-slate-100 align-top transition-colors hover:bg-slate-50 focus:bg-slate-50 focus:outline-none">
      <td className="px-4 py-3">
        <div className="font-medium text-slate-800">{requestLabel(event)}</div>
        <div className="mt-1 font-mono text-xs text-slate-500">
          {receiptRequestLabel(event)}
        </div>
      </td>
      <td className="px-4 py-3 text-sm text-slate-600">
        <div>{event.provider_host || "Engine"}</div>
        {event.provider_http_status ? <div className="mt-1 text-xs text-slate-400">HTTP {event.provider_http_status}</div> : null}
      </td>
      <td className="px-4 py-3">
        <span className={`inline-flex rounded-full px-2 py-0.5 text-xs font-medium ${failed ? "bg-red-50 text-red-700" : "bg-emerald-50 text-emerald-700"}`}>
          {event.status}
        </span>
        {event.failure_reason ? <div className="mt-1 max-w-xs text-xs text-red-600">{event.failure_reason}</div> : null}
      </td>
      <td className="px-4 py-3 text-sm text-slate-600">{event.latency_ms} ms</td>
      <td className="px-4 py-3 text-right text-xs text-slate-500">
        <div>{new Date(event.started_at).toLocaleString()}</div>
        {event.trace_id ? <div className="mt-1 font-mono" title={event.trace_id}>trace {event.trace_id.slice(0, 8)}</div> : null}
      </td>
      <td className="px-3 py-3 text-slate-400"><ChevronRight className="h-4 w-4" /></td>
    </tr>
  );
}

// RequestCard presents the same receipt identity as the desktop table without
// forcing a wide column layout onto touch-sized screens.
function RequestCard({ event, onSelect }: { event: EngineExecutionEventEntry; onSelect: (event: EngineExecutionEventEntry) => void }) {
  const failed = event.status === "failed";
  const providerRequest = receiptRequestLabel(event);
  return (
    <button type="button" aria-haspopup="dialog" aria-label={`Inspect ${requestLabel(event)}`} onClick={() => onSelect(event)} className="block w-full p-4 text-left transition-colors hover:bg-slate-50 focus:bg-slate-50 focus:outline-none">
      <div className="flex min-w-0 items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="break-words font-medium text-slate-800">{requestLabel(event)}</div>
          <div className="mt-1 break-all font-mono text-[11px] text-slate-500">{providerRequest}</div>
        </div>
        <ChevronRight className="mt-0.5 h-4 w-4 shrink-0 text-slate-400" />
      </div>
      <div className="mt-3 flex flex-wrap items-center gap-2 text-xs">
        <span className={`inline-flex rounded-full px-2 py-0.5 font-medium ${failed ? "bg-red-50 text-red-700" : "bg-emerald-50 text-emerald-700"}`}>{event.status}</span>
        <span className="text-slate-500">{event.latency_ms} ms</span>
        {event.provider_http_status ? <span className="text-slate-400">HTTP {event.provider_http_status}</span> : null}
      </div>
      <div className="mt-3 flex min-w-0 items-end justify-between gap-3 text-xs text-slate-500">
        <span className="min-w-0 truncate" title={event.provider_host || "Engine"}>{event.provider_host || "Engine"}</span>
        <span className="shrink-0 text-right">{new Date(event.started_at).toLocaleString()}</span>
      </div>
      {event.failure_reason ? <div className="mt-2 break-words text-xs text-red-600">{event.failure_reason}</div> : null}
    </button>
  );
}

// AppRequestsPanel owns one page of app-scoped receipts and the shared detail drawer.
export function AppRequestsPanel({ appId, consumerName, transport }: AppRequestsPanelProps) {
  const [status, setStatus] = useState<RequestStatus>("all");
  const [page, setPage] = useState(1);
  const [items, setItems] = useState<EngineExecutionEventEntry[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
	const [issue, setIssue] = useState<AppActivityIssue | null>(null);
	const [includeAllVersions, setIncludeAllVersions] = useState(false);
  const [selectedEvent, setSelectedEvent] = useState<EngineExecutionEventEntry | null>(null);

  useEffect(() => {
    setLoading(true);
    setIssue(null);
	// Changing a receipt query closes detail that may no longer exist on its page.
	setSelectedEvent(null);
	api.workspace.listAppExecutionEvents({
		appId,
		includeAllVersions,
		// MCP remains a distinct runtime. SDK-family pages intentionally omit the
		// transport filter so direct REST and generated-client calls appear together.
		transport: transport === "mcp" ? "mcp" : undefined,
      status: status === "all" ? undefined : status,
      limit: PAGE_SIZE,
      offset: (page - 1) * PAGE_SIZE,
    }).then((result) => {
      setItems(result.items);
      setTotal(result.total);
    }).catch((cause) => {
      setIssue(appActivityIssue(cause, transport));
    }).finally(() => setLoading(false));
	}, [appId, includeAllVersions, page, status, transport]);

  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));

  // A status change restarts pagination so the first filtered receipt is visible.
  const selectStatus = (next: RequestStatus) => {
    setStatus(next);
    setPage(1);
  };

  return (
    <div className="overflow-hidden rounded-xl border border-slate-200 bg-white">
      <div className="flex flex-col gap-3 border-b border-slate-100 px-4 py-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="min-w-0">
          <h3 className="text-sm font-semibold text-slate-900">Execution receipts</h3>
          <p className="mt-0.5 text-xs text-slate-500">Individual and Unified calls through this {transport === "mcp" ? "MCP server" : "app"}. Open a Unified call to inspect its executions.</p>
        </div>
		<div className="grid w-full grid-cols-1 gap-2 sm:flex sm:w-auto sm:flex-wrap sm:items-center">
			<select
				aria-label="Activity version"
				value={includeAllVersions ? "all" : "current"}
				onChange={(event) => { setIncludeAllVersions(event.target.value === "all"); setPage(1); }}
				className="w-full rounded-lg border border-slate-200 bg-white px-3 py-2 text-xs font-medium text-slate-700 sm:w-auto sm:py-1.5"
			>
				<option value="current">This version</option>
				<option value="all">All versions</option>
			</select>
			<div className="grid grid-cols-3 rounded-lg bg-slate-100 p-1">
          {(["all", "success", "failed"] as RequestStatus[]).map((value) => (
            <button
              key={value}
              type="button"
              onClick={() => selectStatus(value)}
              className={`rounded-md px-3 py-1.5 text-xs font-medium capitalize ${status === value ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700"}`}
            >
              {value}
            </button>
			))}
			</div>
		</div>
      </div>

      {loading ? (
        <div className="flex items-center justify-center gap-2 py-14 text-sm text-slate-500"><Loader2 className="h-4 w-4 animate-spin" /> Loading requests...</div>
      ) : issue ? (
        <div className={`px-4 py-12 text-center text-sm ${issue.tone === "neutral" ? "text-slate-500" : "text-red-600"}`}>{issue.message}</div>
      ) : items.length === 0 ? (
        <div className="flex flex-col items-center py-14 text-center text-slate-500">
          <Clock className="mb-2 h-5 w-5 text-slate-400" />
          <p className="text-sm">No {status === "all" ? "" : `${status} `}requests recorded yet.</p>
        </div>
      ) : (
        <>
        <div className="divide-y divide-slate-100 md:hidden">
          {items.map((event) => <RequestCard key={event.id} event={event} onSelect={setSelectedEvent} />)}
        </div>
        <div className="hidden overflow-x-auto md:block">
          <table className="w-full min-w-[760px] text-left">
            <thead className="bg-slate-50 text-xs uppercase tracking-wide text-slate-500">
              <tr><th className="px-4 py-3">Request</th><th className="px-4 py-3">Provider</th><th className="px-4 py-3">Result</th><th className="px-4 py-3">Latency</th><th className="px-4 py-3 text-right">Started</th><th className="px-3 py-3"><span className="sr-only">Inspect</span></th></tr>
            </thead>
            <tbody>{items.map((event) => <RequestRow key={event.id} event={event} onSelect={setSelectedEvent} />)}</tbody>
          </table>
        </div>
        </>
      )}

      {total > PAGE_SIZE ? (
        <div className="flex flex-wrap items-center justify-between gap-2 border-t border-slate-100 px-4 py-3 text-xs text-slate-500">
          <span className="min-w-0">Page {page} of {totalPages} · {total} requests</span>
          <div className="flex gap-1">
            <button type="button" aria-label="Previous page" disabled={page === 1} onClick={() => setPage((value) => Math.max(1, value - 1))} className="rounded p-1.5 hover:bg-slate-100 disabled:opacity-40"><ChevronLeft className="h-4 w-4" /></button>
            <button type="button" aria-label="Next page" disabled={page === totalPages} onClick={() => setPage((value) => Math.min(totalPages, value + 1))} className="rounded p-1.5 hover:bg-slate-100 disabled:opacity-40"><ChevronRight className="h-4 w-4" /></button>
          </div>
        </div>
      ) : null}
      {/* One inspector owns the parent/child stack, keeping service receipt rendering shared. */}
      {selectedEvent ? <AppExecutionInspector key={selectedEvent.id} event={selectedEvent} appId={appId} consumerName={consumerName} onClose={() => setSelectedEvent(null)} /> : null}
    </div>
  );
}
