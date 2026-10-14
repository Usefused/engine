import { useEffect, useRef, useState } from "react";
import { api, type SecretMeta } from "~/lib/api";

type Props = { secret: SecretMeta; canManage: boolean; canReveal: boolean; onChanged?: () => void };

/** Keeps plaintext out of row metadata and mounts the sensitive dialog only after explicit interaction. */
export function BucketSecretAccess(props: Props) {
  const [open, setOpen] = useState(false);
  // Older UI-created keys retain OpenAPI spelling in metadata.
  const type = props.secret.credential_type.toLowerCase().replaceAll("-", "_");
  const eligible = type === "api_key" || type === "apikey" || type === "bucket_secret";
  // Unsupported families and callers without either capability see only the existing masked placeholder.
  if (!eligible || (!props.canManage && !props.canReveal)) return <span className="text-xs text-slate-500">********</span>;
  return <>
    <button type="button" className="rounded border border-slate-200 px-2 py-1 text-xs text-slate-600" onClick={/** Opening a dialog never fetches plaintext. */ () => setOpen(true)} aria-label={`Secret access for ${props.secret.key_name}`}>
      {props.secret.exportable ? "Exportable" : "Private"}
    </button>
    {/* Unmounting on close discards all sensitive component state. */}
    {open && <SecretAccessDialog {...props} onClose={/** Close removes plaintext from the UI. */ () => setOpen(false)} />}
  </>;
}

/** Uses the native modal focus trap and keeps retrieved values transient and excluded from agent-visible context. */
function SecretAccessDialog({ secret, canManage, canReveal, onChanged, onClose }: Props & { onClose: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  const request = useRef<AbortController | null>(null);
  const alive = useRef(true);
  const [value, setValue] = useState<string | null>(null);
  const [exportable, setExportable] = useState(secret.exportable === true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  // Blur and unmount cancel pending reads as well as clearing already displayed values.
  useEffect(() => {
    alive.current = true;
    dialog.current?.showModal();
    /** Leaving the window must not let a late response restore a hidden value. */
    const hide = () => { request.current?.abort(); setValue(null); };
    window.addEventListener("blur", hide);
    // Close and permission-driven remounts release pending requests and listener ownership.
    return () => { alive.current = false; request.current?.abort(); window.removeEventListener("blur", hide); };
  }, []);

  // Auto-hide bounds accidental on-screen exposure while requiring another audited request to reveal again.
  useEffect(() => {
    // No timer is needed before the user retrieves a value.
    if (value === null) return;
    const timer = window.setTimeout(/** Clear without retaining a recoverable client cache. */ () => setValue(null), 30_000);
    return /** Unmount or a subsequent hide releases this timer. */ () => window.clearTimeout(timer);
  }, [value]);

  /** Fetches only after an explicit reveal action and suppresses stale or cancelled responses. */
  async function reveal() {
    // UI checks complement Engine's authoritative grant and export-policy checks.
    if (!canReveal || !exportable || busy) return;
    setBusy(true); setError("");
    const controller = new AbortController(); request.current = controller;
    try {
      const result = await api.workspace.revealBucketSecret(secret.bucket_id, secret.id, controller.signal);
      // Closing, losing focus, or losing authority prevents late plaintext from entering state.
      if (alive.current && !controller.signal.aborted) setValue(result.value);
    } catch {
      // Only fixed copy is displayed so exception payloads cannot echo secret material.
      if (alive.current && !controller.signal.aborted) setError("Could not reveal this secret. Check your permission and its current export policy.");
    } finally {
      // Unmounted components must not retain asynchronous state updates.
      if (alive.current) setBusy(false);
    }
  }

  /** Saves explicit retrieval consent separately from the secret value. */
  async function savePolicy() {
    // Reveal authority alone cannot change a secret's export policy.
    if (!canManage || busy) return;
    setBusy(true); setError(""); setValue(null);
    try {
      await api.workspace.setSecretExportable(secret.bucket_id, secret.id, exportable);
      // Ignore acknowledgements after the dialog has been dismissed.
      if (alive.current) { onChanged?.(); onClose(); }
    } catch {
      // A failed write leaves the user's proposed change available for inspection.
      if (alive.current) setError("Could not update the export policy. Refresh before retrying.");
    } finally {
      // Closing discards all pending presentation state.
      if (alive.current) setBusy(false);
    }
  }

  return <dialog ref={dialog} aria-label="Secret access" onCancel={onClose} onClose={onClose} className="m-auto w-[calc(100%-2rem)] max-w-md rounded-xl border border-slate-200 bg-white p-5 shadow-xl backdrop:bg-slate-900/30">
    <div className="flex items-start justify-between gap-3"><h2 className="break-all text-base font-semibold">{secret.key_name}</h2><button type="button" onClick={onClose} className="text-sm">Close</button></div>
    <p className="my-3 text-sm text-slate-600">Allow authorized callers to retrieve this secret and use it outside Fused. Disabling retrieval does not invalidate copies already retrieved.</p>
    {/* Credential managers can change policy; reveal-only callers see the saved state. */}
    {canManage ? <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={exportable} disabled={busy} onChange={/** Editing policy immediately hides previously retrieved material. */ (event) => { setValue(null); setExportable(event.target.checked); }} />Allow secret retrieval</label> : <p className="text-sm">{exportable ? "Retrieval enabled" : "Retrieval disabled"}</p>}
    <div className="my-3 flex flex-wrap gap-2">
      {/* Unsaved changes must never enable a reveal button against a private stored secret. */}
      {canReveal && secret.exportable && exportable && <button type="button" disabled={busy} onClick={reveal} className="rounded border px-3 py-1.5 text-sm disabled:opacity-50">Reveal secret</button>}
      {canManage && <button type="button" disabled={busy || exportable === (secret.exportable === true)} onClick={savePolicy} className="rounded border px-3 py-1.5 text-sm disabled:opacity-50">Save policy</button>}
    </div>
    {error && <p role="alert" className="text-sm text-red-700">{error}</p>}
    {/* Plaintext is never placed in metadata, browser storage, telemetry, or the assistant context. */}
    {value !== null && <div data-fused-visible="false"><textarea aria-label="Revealed secret" readOnly value={value} spellCheck={false} autoComplete="off" className="w-full resize-y rounded border p-2 font-mono text-xs" /><button type="button" onClick={/** Hiding drops the value instead of retaining a toggle cache. */ () => setValue(null)} className="text-xs text-slate-600">Hide now · automatically hidden after 30 seconds</button></div>}
  </dialog>;
}
