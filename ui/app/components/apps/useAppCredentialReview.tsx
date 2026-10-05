import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Loader2, X } from "lucide-react";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasResourcePermission } from "~/lib/current-actor-access";
import type { AppPlanResponse, AppPlanReviewer } from "~/lib/app-builder-contract";
import { missingAppCredentials } from "~/lib/app-credential-readiness";
import { AppCredentialWarning } from "./AppCredentialWarning";

interface Review {
  plan: AppPlanResponse;
  recheck: () => Promise<AppPlanResponse>;
  resolve: (plan: AppPlanResponse | null) => void;
}

/** Suspends creation at the reviewed plan while the user chooses setup, rechecking or explicit deferral. */
export function useAppCredentialReview() {
  const { access } = useCurrentActorAccess();
  const [pending, setPending] = useState<Review | null>(null);
  const current = useRef<Review | null>(null);
  const checking = useRef(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [accepted, setAccepted] = useState<AppPlanResponse | null>(null);

  // Leaving the builder cancels approval and prevents a late recheck from applying an app.
  useEffect(() => () => { current.current?.resolve(null); current.current = null; }, []);

  /** Credential management and bucket visibility are independent from app creation authority. */
  function canManageBucket(bucketId: string): boolean {
    return hasResourcePermission(access, "bucket.read", "BUCKET", bucketId) && hasResourcePermission(access, "credentials.manage", "BUCKET", bucketId);
  }

  /** Ready plans continue immediately; missing material always requires a new explicit choice. */
  const review: AppPlanReviewer = (plan, recheck) => {
    setAccepted(null);
    // A previous prompt cannot authorize a newer draft or different missing scheme.
    current.current?.resolve(null);
    current.current = null;
    setPending(null); setError(""); setBusy(false); checking.current = false;
    if (!missingAppCredentials(plan.credential_readiness).length) return Promise.resolve(plan);
    return new Promise((resolve) => {
      const request = { plan, recheck, resolve };
      current.current = request; setPending(request);
    });
  };

  /** Settles only the active review; cancel never reaches apply and proceed preserves its exact receipt. */
  function finish(proceed: boolean) {
    const request = current.current;
    // A recheck in flight or failure cannot approve stale readiness as if it were current.
    if (!request || (proceed && (checking.current || Boolean(error)))) return;
    current.current = null; setPending(null);
    setAccepted(proceed ? request.plan : null);
    request.resolve(proceed ? request.plan : null);
  }

  /** Replans against current bucket material after setup without invoking any provider operation. */
  async function recheck() {
    const request = current.current;
    // Repeated clicks share one check and cannot enqueue multiple app publications.
    if (!request || checking.current) return;
    checking.current = true; setBusy(true); setError("");
    try {
      const plan = await request.recheck();
      // A cancelled or replaced dialog must ignore its old network response.
      if (current.current !== request) return;
      request.plan = plan;
      if (!missingAppCredentials(plan.credential_readiness).length) {
        current.current = null; setPending(null); request.resolve(plan);
      } else setPending({ ...request });
    } catch (cause) {
      // Keep the draft and dialog open so a temporary check failure can be retried.
      if (current.current === request) setError(cause instanceof Error ? cause.message : "Could not check credentials.");
    } finally {
      // A newer review owns its own loading state.
      if (current.current === request || current.current === null) { checking.current = false; setBusy(false); }
    }
  }

  return {
    review, canManageBucket,
    warning: <AppCredentialWarning readiness={accepted?.credential_readiness} canManageBucket={canManageBucket} created />,
    dialog: pending ? <CredentialReviewDialog plan={pending.plan} canManageBucket={canManageBucket} busy={busy} error={error} onCancel={() => finish(false)} onProceed={() => finish(true)} onRecheck={recheck} /> : null,
  };
}

/** Uses the native modal focus boundary so keyboard and screen-reader users can safely review before creation. */
function CredentialReviewDialog({ plan, canManageBucket, busy, error, onCancel, onProceed, onRecheck }: {
  plan: AppPlanResponse; canManageBucket: (id: string) => boolean; busy: boolean; error: string;
  onCancel: () => void; onProceed: () => void; onRecheck: () => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  // Native modal presentation traps focus, restores it on close and supports Escape cancellation.
  useEffect(() => { const element = dialog.current!; element.showModal(); return () => element.close(); }, []);
  return createPortal(<dialog ref={dialog} aria-labelledby="credential-review-title" onCancel={(event) => { event.preventDefault(); onCancel(); }} className="m-auto max-h-[90dvh] w-[calc(100%-2rem)] max-w-lg overflow-y-auto rounded-xl border border-slate-200 bg-white p-0 shadow-xl backdrop:bg-slate-900/40">
    <header className="flex items-center justify-between gap-3 border-b border-slate-100 px-5 py-4">
      <h2 id="credential-review-title" className="text-lg font-semibold text-slate-900">Set up credentials?</h2>
      <button type="button" onClick={onCancel} aria-label="Close credential review" className="rounded-md p-1.5 text-slate-500 hover:bg-slate-100"><X className="h-4 w-4" /></button>
    </header>
    <div className="space-y-3 p-5">
      <AppCredentialWarning readiness={plan.credential_readiness} canManageBucket={canManageBucket} />
      <p className="text-xs text-slate-500">Bucket setup opens in a new tab. Your app draft stays here.</p>
      {error && <p role="alert" className="text-sm text-red-700">{error}</p>}
    </div>
    <footer className="flex flex-wrap justify-end gap-2 border-t border-slate-100 px-5 py-4">
      <button type="button" onClick={onCancel} className="rounded-lg px-3 py-2 text-sm text-slate-600 hover:bg-slate-50">Cancel</button>
      <button type="button" onClick={onProceed} disabled={busy || Boolean(error)} className="rounded-lg border border-slate-200 px-3 py-2 text-sm font-medium text-slate-700 disabled:opacity-50">Proceed anyway</button>
      <button type="button" onClick={onRecheck} disabled={busy} className="inline-flex items-center gap-2 rounded-lg bg-slate-950 px-3 py-2 text-sm font-medium text-white disabled:opacity-50">{busy && <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" />}{busy ? "Checking…" : "Recheck and continue"}</button>
    </footer>
  </dialog>, document.body);
}
