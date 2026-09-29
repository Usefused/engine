import { useEffect, useState } from "react";
import { ChevronDown, ChevronUp, Loader2, LockKeyhole } from "lucide-react";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasResourcePermission } from "~/lib/current-actor-access";
import { api, type EngineExecutionEventEntry, type PrivateExecutionDiagnostics } from "~/lib/api";

// Readable JSON preserves the captured values, including non-JSON exceptions and truncated responses.
function diagnosticText(value: unknown): string {
  // A missing output is distinct from an authored null result.
  if (value === undefined) return "No response captured.";
  if (typeof value === "string") {
    // Provider recordings are JSON text where possible, but errors can be arbitrary strings.
    try { return JSON.stringify(JSON.parse(value), null, 2); } catch { return value; }
  }
  return JSON.stringify(value, null, 2);
}

// PayloadPanel keeps large bodies inside the drawer without widening the mobile viewport.
function PayloadPanel({ title, value }: { title: string; value: unknown }) {
  return <details className="group rounded-lg border border-slate-200 bg-white" open>
    <summary className="cursor-pointer px-3 py-2 text-xs font-medium text-slate-700">{title}</summary>
    <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-all border-t border-slate-100 bg-slate-50 p-3 text-xs leading-relaxed text-slate-700">{diagnosticText(value)}</pre>
  </details>;
}

// DiagnosticContent presents explicitly privileged bodies without inserting authored HTML.
function DiagnosticContent({ detail }: { detail: PrivateExecutionDiagnostics }) {
  const { error, calls, incomplete, truncated } = detail.diagnostics;
  return <div className="space-y-3">
    {/* Partial evidence is labelled so a missing provider response is never mistaken for success. */}
    {incomplete || truncated || error?.truncated ? <p className="text-xs text-amber-700">Some details are incomplete or exceeded the capture limit.</p> : null}
    {error ? <div className="space-y-2 rounded-lg border border-red-200 bg-red-50 p-3">
      <p className="text-xs font-semibold capitalize text-red-800">{error.phase.replaceAll("_", " ")}</p>
      <pre className="max-h-48 overflow-auto whitespace-pre-wrap break-all text-xs leading-relaxed text-red-900">{error.message}</pre>
      {/* Exceptions without a stack still retain their original thrown value. */}
      {error.stack ? <PayloadPanel title="Stack trace" value={error.stack} /> : null}
    </div> : null}
    <PayloadPanel title="Request body" value={detail.request} />
    {/* Invalid authored output is useful even though it cannot become the public typed response. */}
    <PayloadPanel title="Response body" value={error?.response ?? detail.response} />
    {calls.map((call) => <div key={call.ordinal} className="space-y-2 border-t border-slate-200 pt-3">
      <p className="text-xs font-semibold text-slate-700">Provider call {call.ordinal}</p>
      {call.truncated ? <p className="text-xs text-amber-700">This call exceeded the capture limit.</p> : null}
      <PayloadPanel title="Operation request" value={call.request} />
      <PayloadPanel title="Operation response" value={call.response} />
      {call.error ? <PayloadPanel title="Provider error" value={call.error} /> : null}
    </div>)}
  </div>;
}

// AuthorizedDiagnostics fetches only on demand and discards private data when collapsed or unmounted.
function AuthorizedDiagnostics({ event }: { event: EngineExecutionEventEntry }) {
  const [open, setOpen] = useState(false);
  const [detail, setDetail] = useState<PrivateExecutionDiagnostics | null>(null);
  const [failed, setFailed] = useState(false);
  // The permission-gated parent unmounts this component when access or execution changes.
  useEffect(() => {
    setDetail(null);
    setFailed(false);
    if (!open) return;
    let active = true;
    api.appConfig.diagnostics(event.app_id!, event.id).then((result) => {
      // A closed panel cannot regain private data from an obsolete network response.
      if (active) setDetail(result);
    }).catch(() => {
      // Authorization failures never echo an upstream response body into the ordinary inspector.
      if (active) setFailed(true);
    });
    return () => { active = false; };
  }, [event.app_id, event.id, open]);
  // Collapsing releases the private payload immediately rather than retaining it in a hidden view.
  const toggle = () => { setDetail(null); setOpen((value) => !value); };
  return <section className="mt-6 min-w-0 border-t border-slate-200 pt-5">
    <button type="button" onClick={toggle} aria-expanded={open} className="flex w-full items-center gap-2 text-left text-sm font-semibold text-slate-800">
      <LockKeyhole size={15} className="text-slate-400" /> Private diagnostics
      {open ? <ChevronUp size={15} className="ml-auto" /> : <ChevronDown size={15} className="ml-auto" />}
    </button>
    <p className="mb-3 mt-1 text-xs text-slate-500">Request, response, and error details · restricted access</p>
    {/* Opening this section performs a fresh server-side permission check. */}
    {open && !detail && !failed ? <p role="status" className="flex items-center gap-2 py-3 text-xs text-slate-500"><Loader2 size={14} className="animate-spin" /> Loading diagnostics…</p> : null}
    {open && failed ? <p role="status" className="rounded-lg bg-slate-50 p-3 text-xs text-slate-600">Diagnostics are unavailable. They may have expired, were not captured, or your access has changed.</p> : null}
    {open && detail ? <DiagnosticContent detail={detail} /> : null}
  </section>;
}

// ExecutionDiagnostics keeps private payloads outside the ordinary audit permission boundary.
export function ExecutionDiagnostics({ event }: { event: EngineExecutionEventEntry }) {
  const { access } = useCurrentActorAccess();
  // Family grants cover immutable versions; the API checks the exact account and version again.
  if (!event.app_id || !event.app_family_id || !hasResourcePermission(access, "app.unified_app.diagnostics.read", "APP", event.app_family_id)) return null;
  return <AuthorizedDiagnostics key={event.id} event={event} />;
}
