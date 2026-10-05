import { useState, type FormEvent } from "react";
import { SecretReferenceField } from "~/components/buckets/SecretReferenceField";
import { api } from "~/lib/api";
import { createWebhook } from "~/lib/webhook-discovery-api";
import type { AppPlanResponse } from "~/lib/app-builder-contract";

/** Reviews a write-only credential reference; Engine never returns the stored signing secret to this form. */
export function WebhookSigningSecret({ slug, onSaved }: { slug: string; onSaved: () => void }) {
  const [open,setOpen]=useState(false), [secret,setSecret]=useState("");
  const [plan,setPlan]=useState<AppPlanResponse|null>(null), [busy,setBusy]=useState(false), [error,setError]=useState(""), [uncertain,setUncertain]=useState(false);
  /** Review preserves the entire server-owned configuration instead of rebuilding it from list cards. */
  async function review(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    // A committed-but-unconfirmed update must be inspected before another mutation is offered.
    if(busy || uncertain) return;
    setBusy(true);setError("");
    try {setPlan(await api.appConfig.webhookSecretPlan<AppPlanResponse>(slug,secret.trim()));}
    catch(cause){setError(String(cause));}
    finally{setBusy(false);}
  }
  /** Apply uses the ordinary webhook receipt path with fresh Engine authorization and revision validation. */
  async function save() {
    // Only the reviewed reference can be applied; edits below invalidate this receipt.
    if(!plan || busy || uncertain) return;
    setBusy(true);setError("");
    try{await createWebhook(plan);setOpen(false);setSecret("");setPlan(null);onSaved();}
    catch(cause){setError(`${String(cause)} Refresh the list before trying again.`);setUncertain(true);}
    finally{setBusy(false);}
  }
  // Keep credential controls out of the URL row until a manager explicitly opens them.
  if(!open) return <button type="button" onClick={()=>setOpen(true)} className="text-[11px] font-medium text-slate-500 hover:text-[var(--brand-violet)]">Change secret reference</button>;
  return <form onSubmit={review} className="w-full space-y-3 border-t border-slate-100 pt-4"><fieldset disabled={busy || uncertain} className="space-y-3">
    <SecretReferenceField required label="New secret reference" disabled={busy || uncertain} value={secret} onChange={(reference) => { setSecret(reference); setPlan(null); setError(""); }} />
    <p className="text-xs text-slate-500">Choose an existing reference or create a secret here. Your receiving URL stays the same.</p>
    {error && <p role="alert" className="text-sm text-red-700">{error}</p>}
    {/* Review and save are distinct; no request retries are hidden behind the button. */}
    <div className="flex flex-wrap items-center gap-3">{plan ? <button type="button" onClick={save} className="rounded-lg bg-[var(--brand-violet)] px-2.5 py-1.5 text-xs font-semibold text-white">{busy ? "Saving…" : "Save reference"}</button> : <button type="submit" className="rounded-lg bg-[var(--brand-violet)] px-2.5 py-1.5 text-xs font-semibold text-white">{busy ? "Reviewing…" : "Review change"}</button>}<button type="button" onClick={()=>{setOpen(false);setPlan(null);setSecret("");setError("");}} className="px-2 py-1.5 text-xs text-slate-500">Cancel</button></div>
  </fieldset></form>;
}
