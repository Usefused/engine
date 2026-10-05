import { useEffect, useId, useState, type FormEvent } from "react";
import { createPortal } from "react-dom";
import { FieldLabel } from "~/components/forms/FieldLabel";
import { Select } from "~/components/forms/Select";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasAnyPermission, hasResourcePermission, hasWorkspacePermission, loadCurrentActorAccess, type CurrentActorAccess } from "~/lib/current-actor-access";
import { api, type Bucket, type SecretMeta, type GraphQLPage } from "~/lib/api";
import { readBuckets } from "~/lib/buckets";
import { readAllBoundedPages } from "~/lib/bounded-pages";
import { createReferencedSecret } from "~/lib/secret-creation";
import { CreateCredentialButton } from "./CreateCredentialButton";
import { BucketCreateModal } from "./BucketCreateModal";

const fieldClass = "w-full rounded-lg border border-slate-300 bg-white px-3 py-2 text-sm font-normal text-slate-900 focus:outline-none focus:ring-2 focus:ring-[var(--brand-violet)] disabled:bg-slate-50";
const actionClass = "text-xs font-semibold text-blue-600 hover:underline disabled:opacity-50";

/** Offers secret creation beside a reference without navigating away or passing secret values to the parent. */
export function SecretReferenceField({ value, onChange, required = false, disabled = false, label = "Signing secret reference" }: {
  value: string; onChange: (value: string) => void; required?: boolean; disabled?: boolean; label?: string;
}) {
  const id = useId();
  const { access } = useCurrentActorAccess();
  const [open, setOpen] = useState(false);
  const canCreate = hasAnyPermission(access, "credentials.manage") || hasWorkspacePermission(access, "bucket.manage");
  /** A successful save replaces only the reference, allowing the caller to invalidate its reviewed plan. */
  function created(reference: string) { onChange(reference); setOpen(false); }
  return <div className="space-y-2 text-sm font-medium">
    <div className="flex flex-wrap items-center justify-between gap-3">
      <label htmlFor={id}><FieldLabel required={required}>{label}</FieldLabel></label>
      {/* Scoped read access does not imply permission to write a secret. */}
      {canCreate && <CreateCredentialButton disabled={disabled} onClick={() => setOpen(true)}>Create secret</CreateCredentialButton>}
    </div>
    <input id={id} required={required} disabled={disabled} autoComplete="off" value={value} onChange={(event) => onChange(event.target.value)} placeholder="${bucket.default.secret.signing_key}" className={fieldClass} />
    {/* Unmounting the dialog discards plaintext values on both cancel and success. */}
    {open && <CreateSecretDialog onClose={() => setOpen(false)} onCreated={created} />}
  </div>;
}

/** Loads writable sets on demand and stores a write-only value through the existing credentials endpoint. */
function CreateSecretDialog({ onClose, onCreated }: { onClose: () => void; onCreated: (reference: string) => void }) {
  const id = useId();
  const [access, setAccess] = useState<CurrentActorAccess | null>(null);
  const [buckets, setBuckets] = useState<Bucket[]>([]);
  const [bucketId, setBucketId] = useState("");
  const [keyName, setKeyName] = useState("");
  const [value, setValue] = useState("");
  const [busy, setBusy] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState("");
  const [createBucket, setCreateBucket] = useState(false);
  // Opening loads choices automatically; the retry button is reserved for lookup failures.
  useEffect(() => { void load(); }, []);
  /** Fresh grants include creator access after making a credential set from this dialog. */
  async function load(preferredId = "") {
    setBusy(true); setError("");
    try {
      const [freshAccess, all] = await Promise.all([
        loadCurrentActorAccess(),
        readAllBoundedPages(async (limit, offset) => {
          const page = await readBuckets(limit, offset);
          return { items: page.bucketSummaries, total: page.total ?? page.bucketSummaries.length };
        }, 100, 100),
      ]);
      const writable = all.filter((bucket) => hasResourcePermission(freshAccess, "credentials.manage", "BUCKET", bucket.id));
      setAccess(freshAccess); setBuckets(writable); setLoaded(true);
      // Creating a set does not bypass a missing secret-write grant.
      if (preferredId && !writable.some((bucket) => bucket.id === preferredId)) throw new Error("The new credential set is not available for storing secrets.");
      // Refresh never substitutes another bucket for an explicit existing choice.
      setBucketId((current) => preferredId || current || writable[0]?.id || "");
    } catch {
      setError("Could not load credential sets. Try again.");
      // Keep the create receipt open so a failed selection can be retried without a duplicate set.
      if (preferredId) throw new Error("Credential set created, but it could not be selected. Check your access, then retry selection.");
    }
    finally { setBusy(false); }
  }
  /** Secret writes happen only on explicit submit, after metadata checks and form validation. */
  async function save(event: FormEvent) {
    event.preventDefault(); event.stopPropagation();
    const bucket = buckets.find((item) => item.id === bucketId);
    // A missing or inaccessible set must not become an implicit default destination.
    if (busy || !bucket) return;
    setBusy(true); setError("");
    try {
      const reference = await createReferencedSecret({ bucket, keyName, value }, {
        // Read metadata only; stored plaintext is never requested or rendered.
        read: async (bucketId, limit, offset) => {
          const result = await api.mcpGraphql<{ secretMetaPage: GraphQLPage<SecretMeta> }>(`query NewSecretMetadata($bucketId: String!, $limit: Int!, $offset: Int!) {
            secretMetaPage(bucket_id: $bucketId, limit: $limit, offset: $offset) { total items { key_name key_names service_id } }
          }`, { bucketId, limit, offset });
          return result.secretMetaPage;
        },
        save: (payload) => api.workspace.upsertBucketSecret(payload),
      });
      setValue(""); onCreated(reference);
    } catch (cause) {
      // Known validation errors contain no values; transport errors might echo request data.
      setError(cause instanceof Error && /^(Choose a credential|Enter a secret|A secret with)/.test(cause.message) ? cause.message : "Could not save the secret. Check Credentials before retrying.");
    } finally { setBusy(false); }
  }
  return createPortal(<div className="fixed inset-0 z-50 flex items-center justify-center bg-slate-900/40 px-4">
    <form onSubmit={save} role="dialog" aria-modal="true" aria-labelledby={`${id}-title`} className="w-full max-w-lg space-y-5 rounded-xl border border-slate-200 bg-white p-5 shadow-xl sm:p-6">
      <h2 id={`${id}-title`} className="text-lg font-semibold text-slate-900">Create secret</h2>
      {/* Loading is explicit and retryable without discarding the user's input. */}
      {!loaded && <button type="button" disabled={busy} onClick={() => void load()} className={actionClass}>{busy ? "Loading…" : "Load credential sets"}</button>}
      {loaded && <div className="space-y-2">
        <div className="flex flex-wrap items-center justify-between gap-3"><label htmlFor={`${id}-bucket`}><FieldLabel required>Credential set</FieldLabel></label>
          {/* New sets require workspace management rather than only scoped secret write access. */}
          {hasWorkspacePermission(access, "bucket.manage") && <CreateCredentialButton disabled={busy} onClick={() => setCreateBucket(true)} />}
        </div>
        <Select id={`${id}-bucket`} required disabled={busy} className={fieldClass} value={bucketId} onChange={(event) => setBucketId(event.target.value)}><option value="">Choose a credential set</option>{buckets.map((bucket) => <option key={bucket.id} value={bucket.id}>{bucket.name}</option>)}</Select>
        {/* Empty results distinguish a lack of write access from a failed request. */}
        {buckets.length === 0 && <p className="text-sm text-slate-500">No credential sets are available for storing secrets.</p>}
      </div>}
      <label className="block space-y-2"><FieldLabel required>Secret name</FieldLabel><input required autoFocus disabled={busy} className={fieldClass} value={keyName} onChange={(event) => setKeyName(event.target.value)} placeholder="stripe_signing_key" /></label>
      <label className="block space-y-2"><FieldLabel required>Secret value</FieldLabel><input required disabled={busy} type="password" autoComplete="new-password" className={fieldClass} value={value} onChange={(event) => setValue(event.target.value)} /></label>
      {/* Only the reference is passed back to webhook planning, never this value. */}
      <p className="text-xs text-slate-500">Paste the secret supplied by your provider. Saving fills in the reference on your form.</p>
      {error && <p role="alert" className="text-sm text-red-700">{error}</p>}
      <div className="flex justify-end gap-3 border-t border-slate-100 pt-4"><button type="button" disabled={busy} onClick={onClose} className="rounded-lg border border-slate-200 px-4 py-2 text-sm font-medium text-slate-600 hover:bg-slate-50 disabled:opacity-50">Cancel</button><button type="submit" disabled={busy || !bucketId || !value || !keyName.trim()} className="rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white shadow-sm hover:bg-slate-800 disabled:opacity-50">{busy ? "Saving…" : "Save secret"}</button></div>
    </form>
    <BucketCreateModal open={createBucket} onClose={() => setCreateBucket(false)} onCreated={async (_name, bucket) => { await load(bucket.id); }} />
  </div>, document.body);
}
