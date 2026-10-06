import { useEffect, useState, type FormEvent } from "react";
import type { SecretMeta } from "~/lib/api";
import type { BucketSecretFormPayload } from "~/lib/buckets";
import { readNamedBucketSecret } from "~/lib/bucket-secret-target";
import { FieldLabel } from "~/components/forms/FieldLabel";

/** Opens an exact existing bucket variable while keeping its value write-only and its usual permissions intact. */
export function FocusedBucketSecret({bucketID,name,canManage,saving,onSave,onClose}:{bucketID:string;name:string;canManage:boolean;saving:boolean;onSave:(payload:BucketSecretFormPayload)=>Promise<void>;onClose:()=>void}){
 const [entry,setEntry]=useState<SecretMeta|null>(null),[loading,setLoading]=useState(true),[error,setError]=useState(""),[value,setValue]=useState(""),[saved,setSaved]=useState(false);
 useEffect(()=>{
  let active=true;setLoading(true);setEntry(null);setError("");setValue("");setSaved(false);
  readNamedBucketSecret(bucketID,name).then((result)=>{
   // A response for a previous bucket must not make the current form writable.
   if(active)setEntry(result);
  }).catch((cause)=>{if(active)setError(String(cause));}).finally(()=>{if(active)setLoading(false);});
  return ()=>{active=false;};
 },[bucketID,name]);
 /** Uses the existing credential save action and preserves the variable's expiry. */
 async function save(event:FormEvent<HTMLFormElement>){
  event.preventDefault();
  // A deep link cannot create a missing variable or bypass credential management authority.
  if(!entry || !canManage || saving || !value)return;
  setError("");
  try{await onSave({keyName:name,value,expiresAt:entry.expires_at||undefined});setValue("");setSaved(true);}
  catch(cause){setError(String(cause));}
 }
 return <section className="m-4 space-y-3 rounded-lg border border-violet-200 bg-violet-50/40 p-4"><div className="flex items-center justify-between gap-3"><h3 className="break-all font-mono text-sm font-semibold text-slate-900">{name}</h3><button type="button" onClick={onClose} className="text-xs text-slate-500">Close</button></div>
  {/* Metadata access is checked by the parent and independently by Engine; values stay masked. */}
  {loading ? <p role="status" className="text-xs text-slate-500">Finding secret…</p> : error ? <p role="alert" className="text-xs text-red-700">{error}</p> : !entry ? <p className="text-xs text-slate-500">This secret no longer exists in this bucket.</p> : <><p className="text-xs text-slate-500">Value: ••••••••</p>{canManage && <form onSubmit={save} className="space-y-3"><label className="block space-y-1 text-xs font-medium"><FieldLabel required>New value</FieldLabel><input required type="password" data-fused-visible="false" autoComplete="new-password" value={value} disabled={saving} onChange={(event)=>{setValue(event.target.value);setSaved(false);}} className="w-full rounded-lg border border-slate-300 bg-white px-3 py-2 text-sm" /></label><button type="submit" disabled={saving||!value} className="rounded-md bg-[var(--brand-violet)] px-2.5 py-1.5 text-xs font-semibold text-white disabled:opacity-50">{saving ? "Saving…" : "Save value"}</button></form>}{saved && <p role="status" className="text-xs text-emerald-700">Secret updated.</p>}</>}
 </section>;
}
