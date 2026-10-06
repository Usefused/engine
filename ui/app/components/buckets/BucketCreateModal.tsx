import { FieldLabel } from "~/components/forms/FieldLabel";
import { type FormEvent, type RefObject, useEffect, useLayoutEffect, useRef, useState } from "react";
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
  const form = useRef<HTMLFormElement>(null);
  const [position, setPosition] = useState({ top: 0, left: 0, width: 384 });

  /** Returns keyboard focus to the action after an explicit dismissal. */
  function dismiss() {
    onClose();
    anchor?.current?.querySelector<HTMLButtonElement>('button')?.focus();
  }

  // Body portals avoid clipping; measured placement follows the trigger as its pane scrolls.
  useLayoutEffect(() => {
    // Existing selector dialogs keep their centered layout.
    if (!open || !anchor?.current) return;
    const trigger = anchor.current;
    /** Fits the name field within narrow screens and flips above a low trigger. */
    function place() {
      const rect = trigger.getBoundingClientRect();
      const width = Math.min(384, window.innerWidth - 32);
      const height = form.current?.offsetHeight ?? 180;
      // Prefer below the action, unless the viewport cannot fit the complete form.
      const top = rect.bottom + 10 + height <= window.innerHeight - 16 ? rect.bottom + 10 : Math.max(16, rect.top - height - 10);
      setPosition({ top, left: Math.max(16, Math.min(rect.right - width, window.innerWidth - width - 16)), width });
    }
    place();
    const resize = new ResizeObserver(place);
    resize.observe(trigger);
    // Error and loading content can change the form height while it is open.
    if (form.current) resize.observe(form.current);
    window.addEventListener('resize', place);
    document.addEventListener('scroll', place, true);
    return () => { resize.disconnect(); window.removeEventListener('resize', place); document.removeEventListener('scroll', place, true); };
  }, [open, anchor]);

  // A non-modal popover allows the page to remain usable without a dimmed backdrop.
  useEffect(() => {
    // Do not dismiss an in-flight creation and lose its completion receipt.
    if (!open || !anchor || saving) return;
    /** Dismisses on outside interaction without stealing focus from the clicked control. */
    function outside(event: PointerEvent) {
      const target = event.target as Node;
      // Assistant interaction keeps the draft available for requested form edits.
      if (form.current?.contains(target) || anchor?.current?.contains(target) || (target instanceof Element && target.closest('[data-fused-agent]'))) return;
      onClose();
    }
    /** Escape closes only this form and restores its trigger focus. */
    function escape(event: KeyboardEvent) {
      // Escape inside the assistant belongs to the conversation panel.
      if (event.key !== 'Escape' || (event.target instanceof Element && event.target.closest('[data-fused-agent]'))) return;
      event.preventDefault(); event.stopPropagation(); dismiss();
    }
    document.addEventListener('pointerdown', outside);
    document.addEventListener('keydown', escape, true);
    return () => { document.removeEventListener('pointerdown', outside); document.removeEventListener('keydown', escape, true); };
  }, [open, anchor, saving, onClose]);

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

  // The transformed workspace pane must not change the dialog's viewport positioning.
  return createPortal(
    <div data-fused-workspace-dialog className={anchor ? "fixed z-50" : "fixed inset-0 z-50 flex items-center justify-center px-4"} style={anchor ? position : undefined}>
      {/* Anchored creation stays lightweight; selector dialogs retain their existing backdrop. */}
      {!anchor && <div className="absolute inset-0 bg-slate-900/40 backdrop-blur-sm" onClick={saving ? undefined : onClose} />}
      {/* An open assistant remains an accessible sibling rather than being hidden by modal semantics. */}
      <form
        onSubmit={submit}
        ref={form}
        id={anchor ? "create-bucket-popover" : undefined}
        role="dialog"
        aria-modal={anchor ? false : !agent?.isOpen}
        aria-labelledby={anchor ? "create-bucket-label" : "create-credential-set-title"}
        className={anchor ? "relative w-full rounded-xl border border-slate-200 bg-white p-5 shadow-lg" : "relative w-full max-w-md rounded-xl border border-slate-200 bg-white shadow-xl"}
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
    </div>, document.body
  );
}
