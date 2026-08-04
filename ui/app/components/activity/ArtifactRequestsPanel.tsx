import { useEffect, useState } from "react";
import { ChevronLeft, ChevronRight, Clock, Loader2 } from "lucide-react";
import { api, type EngineExecutionEventEntry } from "~/lib/api";
import { artifactActivityIssue, type ArtifactActivityIssue } from "~/lib/artifact-activity-error";

type RequestStatus = "all" | "success" | "failed";

interface ArtifactRequestsPanelProps {
  artifactId: string;
  transport: "sdk" | "mcp";
}

const PAGE_SIZE = 10;

function requestLabel(event: EngineExecutionEventEntry): string {
  return event.operation || event.event_name || event.request_path || "Request";
}

function RequestRow({ event }: { event: EngineExecutionEventEntry }) {
  const failed = event.status === "failed";
  return (
    <tr className="border-t border-slate-100 align-top">
      <td className="px-4 py-3">
        <div className="font-medium text-slate-800">{requestLabel(event)}</div>
        <div className="mt-1 font-mono text-xs text-slate-500">
          {[event.http_method, event.request_path].filter(Boolean).join(" ") || event.direction}
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
    </tr>
  );
}

export function ArtifactRequestsPanel({ artifactId, transport }: ArtifactRequestsPanelProps) {
  const [status, setStatus] = useState<RequestStatus>("all");
  const [page, setPage] = useState(1);
  const [items, setItems] = useState<EngineExecutionEventEntry[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [issue, setIssue] = useState<ArtifactActivityIssue | null>(null);

  useEffect(() => {
    setLoading(true);
    setIssue(null);
    api.workspace.listArtifactExecutionEvents({
      artifactId,
      transport,
      status: status === "all" ? undefined : status,
      limit: PAGE_SIZE,
      offset: (page - 1) * PAGE_SIZE,
    }).then((result) => {
      setItems(result.items);
      setTotal(result.total);
    }).catch((cause) => {
      setIssue(artifactActivityIssue(cause, transport));
    }).finally(() => setLoading(false));
  }, [artifactId, page, status, transport]);

  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));

  const selectStatus = (next: RequestStatus) => {
    setStatus(next);
    setPage(1);
  };

  return (
    <div className="overflow-hidden rounded-xl border border-slate-200 bg-white">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-slate-100 px-4 py-3">
        <div>
          <h3 className="text-sm font-semibold text-slate-900">Execution receipts</h3>
          <p className="mt-0.5 text-xs text-slate-500">Actual requests executed through this {transport === "mcp" ? "MCP server" : "app"}.</p>
        </div>
        <div className="flex rounded-lg bg-slate-100 p-1">
          {(["all", "success", "failed"] as RequestStatus[]).map((value) => (
            <button
              key={value}
              type="button"
              onClick={() => selectStatus(value)}
              className={`rounded-md px-3 py-1 text-xs font-medium capitalize ${status === value ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700"}`}
            >
              {value}
            </button>
          ))}
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
        <div className="overflow-x-auto">
          <table className="w-full min-w-[760px] text-left">
            <thead className="bg-slate-50 text-xs uppercase tracking-wide text-slate-500">
              <tr><th className="px-4 py-3">Request</th><th className="px-4 py-3">Provider</th><th className="px-4 py-3">Result</th><th className="px-4 py-3">Latency</th><th className="px-4 py-3 text-right">Started</th></tr>
            </thead>
            <tbody>{items.map((event) => <RequestRow key={event.id} event={event} />)}</tbody>
          </table>
        </div>
      )}

      {total > PAGE_SIZE ? (
        <div className="flex items-center justify-between border-t border-slate-100 px-4 py-3 text-xs text-slate-500">
          <span>Page {page} of {totalPages} · {total} requests</span>
          <div className="flex gap-1">
            <button type="button" aria-label="Previous page" disabled={page === 1} onClick={() => setPage((value) => Math.max(1, value - 1))} className="rounded p-1.5 hover:bg-slate-100 disabled:opacity-40"><ChevronLeft className="h-4 w-4" /></button>
            <button type="button" aria-label="Next page" disabled={page === totalPages} onClick={() => setPage((value) => Math.min(totalPages, value + 1))} className="rounded p-1.5 hover:bg-slate-100 disabled:opacity-40"><ChevronRight className="h-4 w-4" /></button>
          </div>
        </div>
      ) : null}
    </div>
  );
}
