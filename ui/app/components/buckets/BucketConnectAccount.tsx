import { useState, type FormEvent } from "react";
import { ArrowUpRight, Loader2 } from "lucide-react";
import { api, type ActivatedService, type BucketSummary } from "~/lib/api";
import { errorMessage } from "~/lib/buckets";
import { bucketAuthorizeURL, bucketConnectOptions, bucketConnectVariables, selectedConnectOption, canConnectBucketService } from "~/lib/bucket-connect";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";

const START_CONNECT_SESSION = `mutation BucketStartConnectSession($bucketId: String!, $serviceId: String!, $endUserRef: String!, $authType: String, $authName: String, $authRef: String, $scopes: [String!], $returnUrl: String) {
  startConnectSession(bucket_id: $bucketId, service_id: $serviceId, end_user_ref: $endUserRef, auth_type: $authType, auth_name: $authName, auth_ref: $authRef, scopes: $scopes, return_url: $returnUrl) {
    authorize_url expires_at
  }
}`;
const fieldClass = "mt-1 w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 focus:border-slate-500 focus:outline-none focus:ring-1 focus:ring-slate-500";

/** Starts the same bucket-scoped consent mutation as the CLI without handling provider tokens in the browser. */
export function BucketConnectAccount({ bucket, service }: { bucket: BucketSummary; service?: ActivatedService }) {
  const { access } = useCurrentActorAccess();
  const [open, setOpen] = useState(false);
  const [authId, setAuthId] = useState("");
  const [endUserRef, setEndUserRef] = useState("");
  const [scopes, setScopes] = useState("");
  const [authRef, setAuthRef] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const canConnect = canConnectBucketService(access, bucket.id, service?.service_id);
  const options = bucketConnectOptions(service);
  // A sole declared scheme is unambiguous; multiple schemes require an explicit choice.
  const auth = selectedConnectOption(options, authId);

  /** Keeps local errors in the form and redirects only after Engine has admitted the exact requested session. */
  async function connect(event: FormEvent) {
    event.preventDefault();
    // UI permission and selection checks supplement, rather than replace, Engine authorization.
    if (!canConnect || !service || !auth) { setError("Choose an available service and authentication scheme."); return; }
    setError("");
    setPending(true);
    try {
      const variables = bucketConnectVariables({ bucketId: bucket.id, serviceId: service.service_id, endUserRef, auth, authRef, scopes }, window.location.origin);
      const result = await api.mcpGraphql<{ startConnectSession: { authorize_url: string; expires_at: string } }>(START_CONNECT_SESSION, variables);
      window.location.assign(bucketAuthorizeURL(result.startConnectSession.authorize_url));
    } catch (failure) {
      // Admission errors preserve the form and never silently retry consent or switch credentials.
      setError(errorMessage(failure, "Could not start account connection."));
      setPending(false);
    }
  }

  // Read-only bucket access must not expose a control that creates connected credentials.
  if (!canConnect || !options.length) return null;
  return <div className="mt-3">
    <button type="button" aria-expanded={open} onClick={() => { /* Toggle only local form visibility; consent starts on explicit submission. */ setOpen(!open); }} className="rounded-md border border-slate-200 px-3 py-2 text-sm font-medium text-slate-800 hover:bg-slate-50">Connect account</button>
    {/* Keep consent fields local to the service whose action was expanded. */}
    {open && <form onSubmit={connect} className="mt-4 space-y-4">
      <fieldset disabled={pending} className="space-y-4 disabled:opacity-60">
        <ConnectScheme options={options} selected={auth?.id ?? ""} onChange={setAuthId} />
        <label className="block text-xs font-medium text-slate-600">User reference
          <input required value={endUserRef} onChange={(event) => { /* Preserve the caller's stable account identity across reconnects. */ setEndUserRef(event.target.value); }} placeholder="e.g. team-account" className={fieldClass} />
          <span className="mt-1 block font-normal text-slate-500">Identifies this connected account in {bucket.name}.</span>
        </label>
        <details className="text-xs text-slate-600"><summary className="cursor-pointer font-medium">Connection options</summary>
          <div className="mt-3 space-y-3">
            <label className="block">Scopes (optional)<textarea value={scopes} onChange={(event) => { /* Engine validates these against the selected scheme's declared scopes. */ setScopes(event.target.value); }} className={fieldClass} rows={2} placeholder="Leave empty for the service's default scopes" /><span className="mt-1 block text-slate-500">Separate scopes with spaces or newlines.</span></label>
            <label className="block">Application reference (optional)<input value={authRef} onChange={(event) => { /* Forward an explicit local or Fused Managed App reference without resolving credentials here. */ setAuthRef(event.target.value); }} className={fieldClass} placeholder="Use this service's credentials in this bucket" /><span className="mt-1 block text-slate-500">For a shared application or an available Fused Managed App. Leave empty to use the client credentials stored for this service.</span></label>
          </div>
        </details>
        <button type="submit" disabled={!auth} className="inline-flex items-center gap-2 rounded-md bg-slate-900 px-3 py-2 text-sm font-medium text-white disabled:opacity-50">{pending ? <Loader2 className="h-4 w-4 animate-spin" /> : <ArrowUpRight className="h-4 w-4" />}Continue to provider</button>
      </fieldset>
      {/* Admission failures remain visible without clearing the requested account. */}
      {error && <p role="alert" className="break-words text-sm text-red-700">{error}</p>}
    </form>}
  </div>;
}

/** Shows the exact named schemes so services with multiple OAuth registrations never choose one arbitrarily. */
function ConnectScheme({ options, selected, onChange }: { options: ReturnType<typeof bucketConnectOptions>; selected: string; onChange: (id: string) => void }) {
  // The service must be selected before a meaningful scheme can be displayed.
  if (!options.length) return null;
  return <label className="block text-xs font-medium text-slate-600">Authentication
    <select required value={selected} onChange={(event) => { /* Selection pins both auth type and name in the request. */ onChange(event.target.value); }} className={fieldClass}>
      <option value="" disabled>Choose authentication</option>
      {options.map((option) => <option key={option.id} value={option.id}>{option.label} · {option.key_prefix}</option>)}
    </select>
  </label>;
}

/** Treats callback markers as navigation feedback; only the connection list confirms a stored account. */
export function ConnectReturnStatus({ result }: { result: string | null }) {
  // Callback errors should invite a deliberate retry rather than automatically replaying consent.
  if (result === "error") return <p role="alert" className="mb-3 text-sm text-red-700">Account connection was not completed. Try connecting again.</p>;
  // A URL marker alone cannot assert that credentials were persisted for this user.
  if (result === "success") return <p role="status" className="mb-3 text-sm text-slate-600">Returned from the provider. Check Connected users for the account status.</p>;
  return null;
}
