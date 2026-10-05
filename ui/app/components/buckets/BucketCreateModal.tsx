import { FieldLabel } from "~/components/forms/FieldLabel";
import { type FormEvent, useEffect, useState } from "react";
import { Plus, X } from "lucide-react";
import { createPortal } from "react-dom";
import { api, type Bucket } from "~/lib/api";

type BucketCreateModalProps = {
  open: boolean;
  onClose: () => void;
  onCreated: (name: string, bucket: Bucket) => void | Promise<void>;
};

/** Creates a credential set in an app-styled dialog while preserving successful creation across selection retries. */
export function BucketCreateModal({ open, onClose, onCreated }: BucketCreateModalProps) {
  const [name, setName] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [created, setCreated] = useState<Bucket | null>(null);

  // Closing clears the name and completion receipt before another explicit creation.
  useEffect(() => {
    // A hidden dialog must not retain data from the previous creation.
    if (!open) {
      setCreated(null);
      setName("");
      setError("");
      setSaving(false);
    }
  }, [open]);

  // Portals avoid nesting this form inside the selector's parent form.
  if (!open) return null;

  /** Retries selection after a successful create without creating the credential set twice. */
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    event.stopPropagation();
    const trimmed = name.trim();
    // Invalid or duplicate submissions must not issue a mutation.
    if (!trimmed || saving) return;
    setSaving(true);
    setError("");
    try {
      // Keep the create receipt when refreshing an authorized selector fails.
      const bucket = created ?? await api.workspace.createBucket(trimmed);
      setCreated(bucket);
      await onCreated(bucket.name, bucket);
      onClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to create credential set");
    } finally {
      setSaving(false);
    }
  };

  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center px-4">
      <div className="absolute inset-0 bg-slate-900/40 backdrop-blur-sm" onClick={saving ? undefined : onClose} />
      <form
        onSubmit={submit}
        role="dialog"
        aria-modal="true"
        aria-labelledby="create-credential-set-title"
        className="relative w-full max-w-md rounded-xl border border-slate-200 bg-white shadow-xl"
      >
        <div className="flex items-center justify-between border-b border-slate-100 px-5 py-5 sm:px-6">
          <h2 id="create-credential-set-title" className="text-lg font-semibold text-slate-900">Create credential set</h2>
          <button
            type="button"
            onClick={onClose}
            disabled={saving}
            className="p-1.5 rounded-md text-slate-400 hover:bg-slate-100 hover:text-slate-600 disabled:opacity-50"
            aria-label="Close"
          >
            <X className="w-4 h-4" />
          </button>
        </div>

        <div className="px-5 py-5 sm:px-6 space-y-3">
          <label className="block text-sm font-medium text-slate-700" htmlFor="credential-set-name"><FieldLabel required>Name</FieldLabel></label>
          <input
            id="credential-set-name"
            required
            disabled={saving || Boolean(created)}
            value={name}
            onChange={(event) => setName(event.target.value)}
            autoFocus
            className="w-full rounded-lg border border-slate-200 px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-[var(--brand-violet)]"
          />
          {error && <p className="text-sm text-red-600">{error}</p>}
        </div>

        <div className="flex justify-end gap-2 border-t border-slate-100 px-5 py-5 sm:px-6">
          <button
            type="button"
            onClick={onClose}
            disabled={saving}
            className="px-4 py-2 rounded-lg border border-slate-200 text-sm font-medium text-slate-600 hover:bg-slate-50 disabled:opacity-50"
          >
            Cancel
          </button>
          <button
            type="submit"
            disabled={saving || !name.trim()}
            className="inline-flex items-center gap-2 px-4 py-2 rounded-lg bg-slate-950 text-sm font-medium text-white shadow-sm transition-colors hover:bg-slate-800 disabled:opacity-50"
          >
            <Plus className="w-4 h-4" />
            {/* A saved bucket needs only selection retry, never another create request. */}
            {saving ? "Saving…" : created ? "Retry selection" : "Create"}
          </button>
        </div>
      </form>
    </div>, document.body
  );
}
