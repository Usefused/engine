import { useRef, useState, type RefObject, type ChangeEvent, type FormEvent } from "react";
import { AnchoredPopover, focusPopoverAnchor } from "~/components/forms/AnchoredPopover";
import { Loader2, X } from "lucide-react";
import { api, type Service } from "~/lib/api";
import { useToast } from "~/components/Toast";

/** Keeps owner-only display edits in the shared anchored form surface. */
export function ServiceDetailsEditor({ service, anchor, onClose, onSaved }: { service: Service; anchor: RefObject<HTMLElement>; onClose: () => void; onSaved: () => void }) {
  const [name, setName] = useState(service.name);
  const [description, setDescription] = useState(service.description || "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const locked = useRef(false);
  const toast = useToast();
  /** Explicit dismissal returns keyboard focus to the action that opened the form. */
  function dismiss() { onClose(); focusPopoverAnchor(anchor); }

  /** Update only the unsaved display label. */
  function changeName(event: ChangeEvent<HTMLInputElement>) { setName(event.target.value); }

  /** Keep description edits local until the user saves. */
  function changeDescription(event: ChangeEvent<HTMLTextAreaElement>) { setDescription(event.target.value); }

  /** Avoid duplicate writes while preserving a rejected draft for correction. */
  async function save(event: FormEvent) {
    event.preventDefault();
    // The server enforces ownership as well; this guard avoids invalid client submissions.
    if (!service.is_owner || locked.current || !name.trim()) return;
    locked.current = true; setBusy(true); setError("");
    try {
      await api.integrations.updateDetails(service.id, { name: name.trim(), description });
      toast.success("Service details updated."); onSaved();
    } catch (cause) {
      // Keep a useful API error when provided, with a fallback for unexpected failures.
      setError(cause instanceof Error ? cause.message : "Could not update service details.");
    } finally { locked.current = false; setBusy(false); }
  }

  // Non-owners must never see an editable draft, even if callers accidentally mount it.
  if (!service.is_owner) return null;
  return (
    <AnchoredPopover anchor={anchor} busy={busy} onClose={onClose}>
    <form onSubmit={save} id="service-details-popover" role="dialog" aria-modal={false} aria-label="Edit service details" className="p-5">
      <div className="mb-4 flex items-center justify-between gap-3">
        <h2 className="text-sm font-semibold text-slate-900">Edit details</h2>
        <button type="button" onClick={dismiss} disabled={busy} aria-label="Close service details" className="rounded-md p-1 text-slate-400 hover:bg-slate-100 hover:text-slate-700"><X className="h-4 w-4" /></button>
      </div>
      <div className="space-y-4">
        <label className="block text-sm font-medium text-slate-700">Name
          <input aria-label="Name" autoFocus required maxLength={255} value={name} onChange={changeName} disabled={busy} className="mt-1.5 block w-full rounded-lg border border-slate-200 px-3 py-2 text-sm font-normal text-slate-900 focus:border-slate-400 focus:outline-none focus:ring-2 focus:ring-slate-100" />
        </label>
        <label className="block text-sm font-medium text-slate-700">Description
          <textarea aria-label="Description" placeholder="What does this service do?" rows={3} maxLength={10000} value={description} onChange={changeDescription} disabled={busy} className="mt-1.5 block w-full resize-y rounded-lg border border-slate-200 px-3 py-2 text-sm font-normal text-slate-900 placeholder:text-slate-400 focus:border-slate-400 focus:outline-none focus:ring-2 focus:ring-slate-100" />
        </label>
        {/* Errors remain alongside the draft so a rejected save can be corrected. */}
        {error && <p role="alert" className="text-sm text-red-700">{error}</p>}
      </div>
      <div className="mt-5 flex justify-end gap-2">
        <button type="button" onClick={dismiss} disabled={busy} className="rounded-lg border border-slate-200 px-3 py-2 text-sm text-slate-700 hover:bg-slate-50">Cancel</button>
        <button type="submit" disabled={busy || !name.trim()} className="inline-flex items-center gap-2 rounded-lg bg-slate-950 px-3 py-2 text-sm font-medium text-white disabled:opacity-50">
          {/* Busy feedback distinguishes a pending request from validation disabling. */}
          {busy && <Loader2 className="h-4 w-4 animate-spin" />}Save changes
        </button>
      </div>
    </form>
    </AnchoredPopover>
  );
}
