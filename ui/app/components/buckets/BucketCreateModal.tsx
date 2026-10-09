import { AnchoredPopover, focusPopoverAnchor } from "~/components/forms/AnchoredPopover";
import { FieldLabel } from "~/components/forms/FieldLabel";
import { type FormEvent, type RefObject, useEffect, useState } from "react";
import { Loader2, Plus, X } from "lucide-react";
import { createPortal } from "react-dom";
import { api, type Bucket } from "~/lib/api";
import { useFusedAgent } from "~/components/agent/FusedAgentContext";

type BucketCreateModalProps = {
  open: boolean;
  onClose: () => void;
  onCreated: (name: string, bucket: Bucket) => void | Promise<void>;
  anchor?: RefObject<HTMLElement>;
};

/** Shares the creation receipt between an anchored page popover and existing selector dialogs. */
export function BucketCreateModal({ open, onClose, onCreated, anchor }: BucketCreateModalProps) {
  const agent = useFusedAgent();
  const [name, setName] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [created, setCreated] = useState<Bucket | null>(null);
  /** Returns keyboard focus to the action after an explicit dismissal. */
  function dismiss() {
    onClose();
    // Centered selector dialogs do not provide an external anchor.
    if (anchor) focusPopoverAnchor(anchor);
  }

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

  /** Retries selection after a successful create without creating the bucket twice. */
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
      setError(err instanceof Error ? err.message : "Failed to create bucket");
    } finally {
      setSaving(false);
    }
  };

  // Both presentations use the same form and creation receipt.
  const content = (
      <form
        onSubmit={submit}
        id={anchor ? "create-bucket-popover" : undefined}
        role="dialog"
        aria-modal={anchor ? false : !agent?.isOpen}
        aria-labelledby={anchor ? "create-bucket-label" : "create-credential-set-title"}
        className={anchor ? "relative w-full p-5" : "relative w-full max-w-md rounded-xl border border-slate-200 bg-white shadow-xl"}
      >
        {/* The label alone identifies the compact form; a second title would repeat it. */}
        {!anchor && <div className="flex items-center justify-between border-b border-slate-100 px-5 py-5 sm:px-6">
          <h2 id="create-credential-set-title" className="text-lg font-semibold text-slate-900">Create bucket</h2>
          <button
            type="button"
            onClick={onClose}
            disabled={saving}
            className="p-1.5 rounded-md text-slate-400 hover:bg-slate-100 hover:text-slate-600 disabled:opacity-50"
            aria-label="Close"
          >
            <X className="w-4 h-4" />
          </button>
        </div>}

        <div className={anchor ? "space-y-2.5" : "px-5 py-5 sm:px-6 space-y-3"}>
          <label id="create-bucket-label" className="block text-sm font-medium text-slate-700" htmlFor="credential-set-name">{anchor ? "Bucket name" : <FieldLabel required>Name</FieldLabel>}</label>
          <input
            id="credential-set-name"
            required
            disabled={saving || Boolean(created)}
            value={name}
            onChange={(event) => setName(event.target.value)}
            autoFocus
            placeholder={anchor ? "e.g. production" : undefined}
            aria-invalid={Boolean(error)}
            aria-describedby={error ? "create-bucket-error" : undefined}
            className="w-full rounded-lg border border-slate-200 px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-[var(--brand-violet)]"
          />
          {error && <p id="create-bucket-error" role="alert" className="text-sm text-red-600">{error}</p>}
        </div>

        <div className={anchor ? "mt-4 flex justify-end gap-2" : "flex justify-end gap-2 border-t border-slate-100 px-5 py-5 sm:px-6"}>
          <button
            type="button"
            onClick={dismiss}
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
            {/* Loading remains visible while the creation request is in flight. */}
            {saving ? <Loader2 className="w-4 h-4 animate-spin" /> : <Plus className="w-4 h-4" />}
            {/* A saved bucket needs only selection retry, never another create request. */}
            {saving ? "Saving…" : created ? "Retry selection" : "Create"}
          </button>
        </div>
      </form>
  );
  // Page actions use the shared popover; embedded selectors retain their centered dialog.
  if (anchor) return <AnchoredPopover anchor={anchor} busy={saving} onClose={onClose}>{content}</AnchoredPopover>;
  return createPortal(
    <div data-fused-workspace-dialog className="fixed inset-0 z-50 flex items-center justify-center px-4">
      {/* An in-flight creation must finish before the backdrop can dismiss its receipt. */}
      <div className="absolute inset-0 bg-slate-900/40 backdrop-blur-sm" onClick={saving ? undefined : onClose} />
      {content}
    </div>, document.body
  );
}
