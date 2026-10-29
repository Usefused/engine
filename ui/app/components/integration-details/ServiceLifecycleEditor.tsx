import { useEffect, useRef, useState, type RefObject, type FormEvent, type ChangeEvent } from "react";
import { useNavigate } from "@remix-run/react";
import { X } from "lucide-react";
import { AnchoredPopover } from "~/components/forms/AnchoredPopover";
import { useToast } from "~/components/Toast";
import { api, type Service, type ServiceLifecycle } from "~/lib/api";

/** Formats an absolute deadline for the browser's local-time date control without changing the instant. */
export function localDeletionDate(value: string): string {
  const date = new Date(value);
  return new Date(date.getTime() - date.getTimezoneOffset() * 60_000).toISOString().slice(0, 16);
}

/** Presents owner removal options under the shared action label while explaining their Registry-wide effect. */
export function ServiceLifecycleEditor({ service, anchor, onClose, onSaved }: { service: Service; anchor: RefObject<HTMLElement>; onClose: () => void; onSaved: () => void }) {
  const [state, setState] = useState<ServiceLifecycle | null>(null);
  const [date, setDate] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const locked = useRef(false);
  const toast = useToast();
  const navigate = useNavigate();

  // Load authoritative schedule state each time the editor opens; avoid updating an unmounted editor.
  useEffect(() => {
    let active = true;
    // Only the current request may initialize the visible draft.
    async function load() {
      try {
        const result = await api.integrations.lifecycle(service.id);
        // Closing or switching services invalidates this response.
        if (!active) return;
        setState(result);
        // Unscheduled services start with an empty date rather than an implied destructive default.
        setDate(result.delete_at ? localDeletionDate(result.delete_at) : "");
      } catch { if (active) setError("Could not load the Registry lifecycle. Close and retry."); }
    }
    void load();
    // Abandoned requests cannot overwrite a subsequent editor's draft.
    return () => { active = false; };
  }, [service.id]);

  /** Change only the local draft until the owner explicitly saves. */
  function changeDate(event: ChangeEvent<HTMLInputElement>) { setDate(event.target.value); }
  /** Keep the editor open while its mutation is pending. */
  function close() { if (!locked.current) onClose(); }

  /** Serialize owner mutations and preserve the draft on server rejection. */
  async function mutate(action: "schedule" | "cancel" | "delete") {
    // Stale permissions or an unresolved lifecycle must not invite a destructive request.
    if (!service.is_owner || locked.current || !state || state.deleted_at) return;
    let instant: string | undefined;
    // Client validation avoids requests for invalid local dates; Registry validates again against its clock.
    if (action === "schedule") {
      const parsed = new Date(date);
      if (!Number.isFinite(parsed.getTime()) || parsed.getTime() <= Date.now()) { setError("Choose a future deletion date and time."); return; }
      instant = parsed.toISOString();
    }
    locked.current = true; setBusy(true); setError("");
    try {
      // Deleting a Registry service affects every workspace, unlike the adjacent membership action.
      if (action === "delete") {
        const confirmed = await toast.confirm(`Permanently delete ${service.name} from the Registry for everyone? Apps using it will stop working as Engines sync. Only service, version and operation names and IDs will remain for history.`);
        // Cancelling confirmation leaves the definition and its schedule untouched.
        if (!confirmed) return;
        await api.integrations.deleteFromRegistry(service.id);
        toast.success("Service deleted from Registry.");
        navigate("/integrations");
      } else {
        // A missing deadline means explicit cancellation, never immediate deletion.
        await api.integrations.scheduleDeletion(service.id, instant);
        toast.success(action === "cancel" ? "Deletion cancelled." : "Service deprecated and deletion scheduled.");
        onSaved();
      }
    } catch (cause) {
      // Keep the specific API denial when available without discarding the owner's draft.
      setError(cause instanceof Error ? cause.message : "Could not update the service lifecycle.");
    } finally { locked.current = false; setBusy(false); }
  }

  /** Submitting the date form always schedules; immediate deletion has a separate confirmed action. */
  function schedule(event: FormEvent) { event.preventDefault(); void mutate("schedule"); }
  /** Cancels only the future removal, preserving any independently deprecated versions. */
  function cancel() { void mutate("cancel"); }
  /** Opens an explicit all-workspaces confirmation before deleting Registry content. */
  function deleteNow() { void mutate("delete"); }

  // Ownership is checked again on the server; hide the editor immediately if the UI loses it.
  if (!service.is_owner) return null;
  return <AnchoredPopover anchor={anchor} busy={busy} onClose={close}>
    <div role="dialog" aria-label="Remove from Workspace" className="space-y-5 p-5 text-sm">
      <div className="flex items-center justify-between gap-3"><h2 className="font-semibold text-slate-900">Remove from Workspace</h2><button type="button" aria-label="Close Remove from Workspace" onClick={close} disabled={busy} className="rounded p-1 text-slate-500 hover:bg-slate-100"><X className="h-4 w-4" /></button></div>
      <p className="text-slate-600">These actions affect every workspace using <strong className="font-medium text-slate-900">{service.name}</strong>.</p>
      <form onSubmit={schedule} className="space-y-3">
        <h3 className="font-medium text-slate-900">Deprecate and schedule deletion</h3>
        <p className="text-slate-500">Warn consumers now and remove the service on your chosen date. You can change or cancel the date before deletion.</p>
        <label className="block text-slate-700">Delete on <span className="text-slate-500">(your local time)</span><input type="datetime-local" required aria-label="Deletion date" value={date} onChange={changeDate} disabled={busy || !state} className="mt-1.5 w-full rounded-md border border-slate-200 bg-white px-3 py-2" /></label>
        <div className="flex flex-wrap gap-2"><button type="submit" disabled={busy || !state || !date} className="rounded-md bg-slate-950 px-3 py-2 font-medium text-white disabled:opacity-50">{state?.delete_at ? "Update deletion date" : "Schedule deletion"}</button>
          {/* Cancellation appears only for an existing schedule. */}
          {state?.delete_at && <button type="button" disabled={busy} onClick={cancel} className="rounded-md px-3 py-2 text-slate-700 hover:bg-slate-100">Cancel deletion</button>}
        </div>
      </form>
      <section className="space-y-2 rounded-lg bg-red-50 p-3"><h3 className="font-medium text-red-900">Delete immediately</h3><p className="text-red-800">Permanently remove all versions and definitions. Keep only names, IDs and deletion history.</p><button type="button" onClick={deleteNow} disabled={busy || !state} className="rounded-md bg-red-700 px-3 py-2 font-medium text-white disabled:opacity-50">Delete from Registry</button></section>
      {/* A failed read or mutation remains visible alongside the controls. */}
      {error && <p role="alert" className="text-red-700">{error}</p>}
    </div>
  </AnchoredPopover>;
}

/** Shows a consumer-facing deadline without exposing any owner controls. */
export function ServiceLifecycleNotice({ serviceId }: { serviceId: string }) {
  const [state, setState] = useState<ServiceLifecycle | null>(null);
  // Refresh on navigation; this notice is advisory while execution uses durable Engine authority.
  useEffect(() => {
    let active = true;
    // Ignore responses from an abandoned service page.
    async function load() { try { const result = await api.integrations.lifecycle(serviceId); if (active) setState(result); } catch { /* Other details remain usable when lifecycle reads are unavailable. */ } }
    void load();
    // Prevent a prior service's deadline leaking into the next page.
    return () => { active = false; };
  }, [serviceId]);
  // Unscheduled services need no lifecycle banner.
  if (!state?.delete_at) return null;
  return <p role="status" className="mb-4 rounded-lg bg-amber-50 px-4 py-3 text-sm text-amber-900">This service is deprecated and will be deleted on {new Date(state.delete_at).toLocaleString()}. Update apps that use it before then.</p>;
}
