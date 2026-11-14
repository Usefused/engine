import { useEffect, useState } from "react";
import { ArrowUpRight, Check, CircleHelp, KeyRound, RefreshCw, TriangleAlert } from "lucide-react";
import { api } from "~/lib/api";
import type { TryReadiness, TryRequirement } from "~/lib/app-try-contract";

const credentialLabels = { managed: "Managed authentication", ready: "Credentials configured", missing: "Credentials missing", unknown: "Credential status unavailable" };
const connectionLabels = { not_required: "", user_required: "Connected user needed", ready: "User connected", missing: "User not connected", reconnect_required: "Reconnect required", unknown: "Connection status unavailable" };

/** Refreshes exact operation/bucket/user readiness without exposing provider credentials or starting consent. */
export function AppTryReadiness({ versionID, operation, bucketID, userRef, unified }: { versionID: string; operation: string; bucketID: string; userRef: string; unified: boolean }) {
  const [state, setState] = useState<{ key: string; data?: TryReadiness; error?: string }>();
  const [revision, setRevision] = useState(0);
  const key = JSON.stringify([versionID, operation, bucketID, userRef, revision]);
  // Debounce user-reference edits while immediately hiding readiness for the previous identity.
  useEffect(() => {
    const controller = new AbortController();
    const timer = window.setTimeout(/** Inspects only the latest settled selection. */ () => {
      void api.appConfig.tryReadiness(versionID, operation, bucketID, userRef, controller.signal).then(/** Publishes metadata only to its live session. */ data => {
        // A later selection owns the visible readiness state.
        if (!controller.signal.aborted) setState({ key, data });
      }).catch(/** Reports failure without guessing credential state. */ () => {
        // Failure is not evidence that a connection is absent or ready.
        if (!controller.signal.aborted) setState({ key, error: "Readiness could not be checked. Verify your bucket access and refresh. Engine still checks credentials when you run." });
      });
    }, 250);
    return /** Invalidates scheduled and pending inspection. */ () => { window.clearTimeout(timer); controller.abort(); };
  }, [key, versionID, operation, bucketID, userRef]);
  // Only an exact selection match can display a green credential state.
  const current = state?.key === key ? state : undefined;
  return <section aria-label="Connection readiness" className="overflow-hidden rounded-xl border border-slate-200 bg-white">
    <div className="flex items-center justify-between gap-3 px-4 py-3">
      <h3 className="text-xs font-semibold text-slate-800">Connections</h3>
      <button type="button" onClick={/** Rechecks metadata after bucket setup. */ () => setRevision(/** Invalidates earlier readiness. */ value => value + 1)} aria-label="Refresh readiness" className="rounded-md p-1.5 text-slate-400 transition hover:bg-slate-100 hover:text-slate-700"><RefreshCw className={`h-3.5 w-3.5 ${!current ? "animate-spin" : ""}`} aria-hidden="true" /></button>
    </div>
    <div aria-live="polite" className="text-xs">
      {/* Pending, failed, and anonymous operations must not look like configured credentials. */}
      {!current ? <p className="px-4 pb-4 text-slate-500">Checking connections…</p> : current.error ? <p className="px-4 pb-4 leading-5 text-amber-800">{current.error}</p> : current.data && <>
        {/* No declared requirements means anonymous access, not missing metadata. */}
        {current.data.requirements.length === 0 && <p className="px-4 pb-4 text-slate-500">No provider credentials required.</p>}
        {current.data.requirements.map(/** Keeps each service, auth scheme, and bucket distinguishable. */ (item, index) => <ConnectionRow key={`${item.bucket_id}:${item.service_id}:${item.auth_name}:${index}`} item={item} unified={unified} />)}
      </>}
    </div>
    {/* Inspection limits remain available without overwhelming the run form with implementation detail. */}
    <ReadinessDetails data={current?.data} />
  </section>;
}

/** Gives connection problems precedence over configured credentials so a green label cannot hide a required action. */
function connectionStatus(item: TryRequirement, unified: boolean): { label: string; tone: "ready" | "attention" | "unknown" } {
  // Missing or unavailable credentials cannot become ready because a user happens to be connected.
  if (item.credentials === "missing") return { label: credentialLabels.missing, tone: "attention" };
  if (item.credentials === "unknown") return { label: credentialLabels.unknown, tone: "unknown" };
  // Authored apps choose their own users; a static preview must not demand an unrelated selector.
  if (unified && item.connection === "user_required") return { label: "User selected at runtime", tone: "unknown" };
  // Configured credentials alone do not resolve missing consent or a stale connection.
  if (item.connection !== "not_required" && item.connection !== "ready") return { label: connectionLabels[item.connection], tone: item.connection === "unknown" ? "unknown" : "attention" };
  return { label: credentialLabels[item.credentials], tone: "ready" };
}

/** Keeps provider identity, status, and bucket navigation readable in one responsive row. */
function ConnectionRow({ item, unified }: { item: TryRequirement; unified: boolean }) {
  const status = connectionStatus(item, unified);
  // Icons mirror text rather than serving as the only indication of missing or unverified access.
  const Icon = status.tone === "ready" ? Check : status.tone === "attention" ? TriangleAlert : CircleHelp;
  const tones = { ready: "bg-emerald-50 text-emerald-700", attention: "bg-amber-50 text-amber-800", unknown: "bg-slate-100 text-slate-600" };
  // Credential setup opens secrets; consent and connected-user setup opens connections.
  const tab = item.connection === "not_required" || item.credentials === "missing" ? "secrets" : "connections";
  return <div className="flex items-start gap-3 border-t border-slate-100 px-4 py-3">
    <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg border border-slate-200 text-slate-400"><KeyRound className="h-3.5 w-3.5" aria-hidden="true" /></div>
    <div className="min-w-0 flex-1 space-y-2">
      <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-2">
        <p className="min-w-0 break-words font-semibold text-slate-800">{item.service} <span className="font-normal text-slate-400">· {item.auth_name}</span></p>
        <span className={`inline-flex max-w-full items-center gap-1.5 rounded-md px-2 py-1 text-[10px] font-medium ${tones[status.tone]}`}><Icon className="h-3 w-3 shrink-0" aria-hidden="true" />{status.label}</span>
      </div>
      <a href={`/integrations/buckets?${new URLSearchParams({ bucket: item.bucket_id, tab })}`} target="_blank" rel="noreferrer" aria-label={`Open ${item.bucket_name} bucket setup for ${item.service}`} className="inline-flex max-w-full items-center gap-1 text-[11px] text-slate-500 underline-offset-2 hover:text-slate-900 hover:underline"><span className="truncate">Bucket: {item.bucket_name}</span><ArrowUpRight className="h-3 w-3 shrink-0" aria-hidden="true" /></a>
    </div>
  </div>;
}

/** Keeps optional inspection details accessible without repeating them in every connection row. */
function ReadinessDetails({ data }: { data?: TryReadiness }) {
  // Loading or failed inspection cannot supply trustworthy scope details.
  if (!data) return null;
  return <details className="border-t border-slate-100 bg-slate-50/60 px-4 py-2.5 text-xs text-slate-500">
      <summary className="cursor-pointer text-[11px] marker:text-slate-400">What this checks</summary>
      <div className="mt-2 space-y-2 pb-1 leading-5">
        <p>Checks saved credentials. Provider access is verified when you run.</p>
        {/* Authored routing notes explain why runtime-selected users cannot be checked here. */}
        {data.note && <p>{data.note}</p>}
      </div>
    </details>;
}
