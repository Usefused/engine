import { AlertTriangle, ArrowUpRight } from "lucide-react";
import type { AppCredentialReadiness } from "~/lib/app-builder-contract";
import { appCredentialBucketURL, appCredentialRequirementKey, missingAppCredentials } from "~/lib/app-credential-readiness";

/** Names missing schemes and their actual buckets without requesting or rendering secret values. */
export function AppCredentialWarning({ readiness, canManageBucket, created = false }: { readiness?: AppCredentialReadiness | null; canManageBucket: (bucketId: string) => boolean; created?: boolean }) {
  const missing = missingAppCredentials(readiness);
  // Anonymous, managed and ready selections must not show a speculative setup warning.
  if (!missing.length) return null;
  return <section aria-label="Missing credentials" className="space-y-3 rounded-lg border border-amber-200 bg-amber-50 p-4 text-sm">
    <div className="flex items-center gap-2 font-semibold text-amber-950"><AlertTriangle className="h-4 w-4 shrink-0" aria-hidden="true" />Credentials needed</div>
    {/* Deferred setup remains visible after publication without implying that the app still needs creation. */}
    <p className="text-amber-900">{created ? "The app is created." : "You can create the app now."} Calls using these credentials will fail until they’re added.</p>
    <ul className="divide-y divide-amber-200/70">
      {missing.map((item) => <li key={appCredentialRequirementKey(item)} className="flex flex-wrap items-center justify-between gap-3 py-3 first:pt-0 last:pb-0">
        <div className="min-w-0 space-y-1">
          <p className="break-words font-medium text-slate-900">{item.service || item.service_id}</p>
          <p className="break-words text-xs text-slate-600">{item.auth_name || item.auth_type} · Bucket: {item.bucket_name || item.bucket_id}</p>
        </div>
        {/* Bucket administration remains gated separately from permission to create an app. */}
        {canManageBucket(item.bucket_id)
          ? <a href={appCredentialBucketURL(item)} target="_blank" rel="noopener noreferrer" className="inline-flex shrink-0 items-center gap-1 text-xs font-semibold text-[var(--brand-violet)] hover:underline">Add credentials<ArrowUpRight className="h-3.5 w-3.5" aria-hidden="true" /><span className="sr-only"> in a new tab</span></a>
          : <span className="text-xs text-slate-600">Ask a bucket administrator to add these.</span>}
      </li>)}
    </ul>
  </section>;
}
