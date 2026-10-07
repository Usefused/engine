import { useEffect, useId, useRef, useState, type FormEvent } from "react";
import { KeyRound, Loader2, Plus } from "lucide-react";
import { api } from "~/lib/api";
import { hasResourcePermission } from "~/lib/current-actor-access";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { CopyValue } from "~/components/CopyValue";
import { FieldLabel } from "~/components/forms/FieldLabel";
import { Select } from "~/components/forms/Select";

type TokenAppKind = "sdk" | "api" | "mcp" | "unified_app";
const secondary = "inline-flex items-center justify-center gap-2 rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50 disabled:opacity-50";

/** Gates issuance on the exact family and app type; losing access unmounts any one-time secret. */
export function AppExecutionTokens({ familyID, kind }: { familyID: string; kind: TokenAppKind }) {
  const { access, loading, failed } = useCurrentActorAccess();
  const canManage = hasResourcePermission(access, `app.${kind}.tokens.manage`, "APP", familyID);
  return <section aria-label="Execution tokens" className="min-w-0 rounded-xl border border-slate-200 bg-white p-5">
    {/* The keyed form discards plaintext on identity changes, including navigation between app families. */}
    {canManage ? <TokenGenerator key={`${access?.subject_id}:${kind}:${familyID}`} familyID={familyID} /> : <>
      <h2 className="text-sm font-semibold text-slate-900">Execution tokens</h2>
      <p className="mt-2 text-sm text-slate-500">{loading ? "Checking token permissions…" : failed ? "Could not check token permissions. Refresh to try again." : "You need permission to manage execution tokens for this app."}</p>
    </>}
  </section>;
}

/** Creates tokens only on submission and retains the returned secret in this mounted form's memory. */
function TokenGenerator({ familyID }: { familyID: string }) {
  const id = useId();
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [expiry, setExpiry] = useState("2592000");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [issued, setIssued] = useState<{ token: string; name: string; expires_at: string | null } | null>(null);
  const pending = useRef(false);
  const mounted = useRef(true);
  // Responses for a closed page must not reveal credentials in a subsequently mounted app.
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);

  /** Prevents duplicate issuance and displays authoritative Engine failures without automatic retries. */
  async function generate(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    // Native validation handles ordinary entry; this guard also covers rapid repeated submissions.
    if (pending.current || !name.trim()) return;
    pending.current = true; setBusy(true); setError("");
    try {
      const result = await api.appTokens.generate(familyID, { name: name.trim(), allow: ["*"], ...(expiry === "never" ? {} : { expires_in: Number(expiry) }) });
      // Only this still-visible authorized form may receive its one-time value.
      if (mounted.current) { setIssued(result); setOpen(false); setName(""); }
    } catch (cause) {
      // Keep the draft editable on failure without guessing whether a retry is safe.
      if (mounted.current) setError(cause instanceof Error ? cause.message : "Could not generate an execution token.");
    } finally {
      pending.current = false;
      // Unmounted forms cannot publish stale loading state.
      if (mounted.current) setBusy(false);
    }
  }

  /** Dismisses the one-time plaintext without revoking the token stored by Engine. */
  function dismissToken() { setIssued(null); }

  return <>
    <div className="flex flex-wrap items-start justify-between gap-3">
      <div><h2 className="flex items-center gap-2 text-sm font-semibold text-slate-900"><KeyRound className="h-4 w-4 text-slate-400" aria-hidden="true" />Execution tokens</h2><p className="mt-1.5 text-sm text-slate-500">Authenticate calls to this app across its versions.</p></div>
      {/* Finish copying the current credential before creating another one-time value. */}
      {!open && !issued && <button type="button" onClick={() => { setError(""); setOpen(true); }} className={secondary}><Plus className="h-4 w-4" aria-hidden="true" />Generate token</button>}
    </div>
    {open && <form onSubmit={generate} className="mt-4 space-y-4 border-t border-slate-100 pt-4">
      <div className="grid gap-4 sm:grid-cols-2">
        <label htmlFor={`${id}-name`} className="space-y-1 text-sm font-medium text-slate-700"><FieldLabel required>Token name</FieldLabel><input id={`${id}-name`} name="tokenName" autoFocus required maxLength={128} disabled={busy} value={name} onChange={event => setName(event.target.value)} placeholder="e.g. backend-production" className="w-full rounded-lg border border-slate-300 px-3 py-2 text-sm font-normal text-slate-900 placeholder:text-slate-400" /></label>
        <label htmlFor={`${id}-expiry`} className="space-y-1 text-sm font-medium text-slate-700"><FieldLabel>Expires after</FieldLabel><Select id={`${id}-expiry`} disabled={busy} value={expiry} onChange={event => setExpiry(event.target.value)}><option value="86400">1 day</option><option value="2592000">30 days</option><option value="7776000">90 days</option><option value="31536000">1 year</option><option value="never">Never</option></Select></label>
      </div>
      <p className="text-xs text-slate-500">Allows all operations exposed by this app, including operations added in future versions.</p>
      {error && <p role="alert" className="text-sm text-red-700">{error}</p>}
      <div className="flex justify-end gap-2"><button type="button" disabled={busy} onClick={() => setOpen(false)} className={secondary}>Cancel</button><button type="submit" disabled={busy || !name.trim()} className="inline-flex items-center gap-2 rounded-lg bg-violet-700 px-3 py-2 text-sm font-medium text-white hover:bg-violet-800 disabled:opacity-50">{busy && <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" />}{busy ? "Generating…" : "Generate token"}</button></div>
    </form>}
    {/* One-time credentials and their controls are excluded from assistant page snapshots. */}
    {issued && <div data-fused-visible="false" className="mt-4 border-t border-slate-100 pt-4">
      <p role="status" className="text-sm font-medium text-slate-900">Token “{issued.name}” created</p>
      <p className="mt-1 text-xs text-slate-500">Copy it now. You won’t be able to view it again after leaving or dismissing this message.</p>
      <CopyValue value={issued.token} label="execution token" sensitive className="mt-3" />
      <div className="mt-3 flex justify-end"><button type="button" onClick={dismissToken} className={secondary}>Done</button></div>
    </div>}
  </>;
}
