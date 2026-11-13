import { useEffect, useState } from "react";
import { RefreshCw } from "lucide-react";
import { api } from "~/lib/api";
import type { TryReadiness } from "~/lib/app-try-contract";

const credentialLabels = { managed: "Uses Fused managed authentication", ready: "Credentials configured", missing: "Credentials missing", unknown: "Credential status unavailable" };
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
  return <section aria-label="Connection readiness" className="rounded-lg bg-slate-50 p-4">
    <div className="flex items-center justify-between gap-3"><h3 className="text-sm font-semibold">Connection readiness</h3><button type="button" onClick={/** Rechecks metadata after bucket setup. */ () => setRevision(/** Invalidates earlier readiness. */ value => value + 1)} aria-label="Refresh readiness" className="rounded-md p-1.5 text-slate-500 hover:bg-slate-200"><RefreshCw className="h-4 w-4" /></button></div>
    <div aria-live="polite" className="mt-3 space-y-3 text-xs">
      {/* Unknown, failed, anonymous and configured states remain visibly distinct. */}
      {!current ? <p className="text-slate-500">Checking this operation’s connections…</p> : current.error ? <p className="text-amber-800">{current.error}</p> : current.data && <>
        {/* Empty requirements mean anonymous direct operations, not missing metadata. */}
        {current.data.requirements.length === 0 && <p className="text-slate-600">No direct provider credentials required.</p>}
        {current.data.requirements.map(/** Preserves each exact service, authentication, and bucket tuple. */ (item, index) => <div key={`${item.bucket_id}:${item.service_id}:${item.auth_name}:${index}`} className="space-y-1 border-t border-slate-200 pt-3 first:border-0 first:pt-0">
          <p className="font-semibold text-slate-800">{item.service} <span className="font-normal text-slate-500">· {item.auth_name}</span></p>
          <p className="text-slate-500">Bucket: {item.bucket_name}</p>
          {/* Missing credentials need attention while unknown and managed remain separate labels. */}
          <p className={item.credentials === "missing" ? "text-amber-800" : "text-slate-600"}>{credentialLabels[item.credentials]}</p>
          {/* Authored apps determine their connected user at runtime. */}
          {item.connection !== "not_required" && <p className="text-slate-600">{unified && item.connection === "user_required" ? "The app selects its connected user when it runs." : connectionLabels[item.connection]}</p>}
          {/* Missing application credentials open secrets; consent setup opens connections. */}
          <a href={`/integrations/buckets?${new URLSearchParams({ bucket: item.bucket_id, tab: item.connection === "not_required" || item.credentials === "missing" ? "secrets" : "connections" })}`} target="_blank" rel="noreferrer" className="inline-block pt-1 font-medium text-slate-950 hover:underline">Open bucket setup ↗</a>
        </div>)}
        {/* Show static-inspection limits only when provided by Engine. */}
        {current.data.note && <p className="border-t border-slate-200 pt-3 leading-5 text-slate-500">{current.data.note}</p>}
        <p className="leading-5 text-slate-500">Readiness reflects stored configuration. Provider access and resource selection are checked during execution.</p>
      </>}
    </div>
  </section>;
}
