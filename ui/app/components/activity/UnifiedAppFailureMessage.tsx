import { useEffect, useState } from "react";
import { api, type EngineExecutionEventEntry } from "~/lib/api";

// UnifiedAppFailureMessage keeps retained exception text out of activity queries and private diagnostic requests.
export function UnifiedAppFailureMessage({ event, fallback }: { event: EngineExecutionEventEntry; fallback: string }) {
  const [failure, setFailure] = useState<{ id: string; message: string } | null>(null);
  const [loading, setLoading] = useState(false);
  // A changed receipt owns a new request; late responses cannot display another execution's error.
  useEffect(() => {
    // Successful receipts and legacy entries without an app identity have no retained failure to fetch.
    if (event.status === "success" || !event.app_id) return;
    let active = true;
    setLoading(true);
    api.appConfig.executionFailure(event.app_id, event.id).then((result) => {
      // Closed drawers and newer selections revoke ownership of this response.
      if (active) setFailure({ id: event.id, message: result.message });
    }).catch(() => {
      // Expired results leave the existing receipt explanation available without exposing transport errors.
      if (active) setFailure(null);
    }).finally(() => {
      // A stale request must not clear the next receipt's loading state.
      if (active) setLoading(false);
    });
    return () => { active = false; };
  }, [event.app_id, event.id, event.status]);
  // Pre-upgrade generic results retain the useful stage explanation instead of replacing it with another generic sentence.
  const message = failure?.id === event.id && failure.message !== "unified app did not complete successfully" ? failure.message : "";
  return <>
    <p className="mt-1 whitespace-pre-wrap break-words [overflow-wrap:anywhere]">{message || fallback}</p>
    {/* The receipt remains readable while its retained message is loading. */}
    {loading ? <p role="status" className="mt-2 text-xs">Loading error details…</p> : null}
  </>;
}
