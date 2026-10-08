import { useEffect, useRef, useState, type FormEvent, type KeyboardEvent } from "react";
import { useBeforeUnload, useBlocker } from "@remix-run/react";
import { api, type Service, type SpecificationImportPlan, type SpecificationImportStatus } from "~/lib/api";
import { webhookApplyNeedsStatus } from "~/lib/webhook-editor-import";

const fieldClass = "w-full rounded-md border border-slate-300 px-3 py-2 text-sm";
const buttonClass = "rounded-md bg-slate-900 px-4 py-2 text-sm font-medium text-white disabled:opacity-50";

interface ImportProps { service: Service; version: string; onClose: () => void; onSaved: () => void }

/** Keeps reviewed receipts tied to the existing destination before enabling a mutation. */
function matchesEndpointPlan(plan: SpecificationImportPlan, serviceID: string, version: string): boolean {
  return plan.service_id === serviceID && !plan.is_new_service && plan.target_version === version && plan.action === "update_version" && plan.target_type === "endpoints";
}

/** Only a matching durable commit can complete recovery without another write. */
function endpointImportCommitted(status: SpecificationImportStatus, serviceID: string, version: string): boolean {
  return status.status === "applied" && status.commit_state === "committed" && status.service_id === serviceID && status.version === version;
}

/** Owns import review, commit recovery, and navigation protection independently of presentation. */
export function useEndpointImport({ service, version, onClose, onSaved }: ImportProps) {
  const [source, setSource] = useState("");
  const [url, setURL] = useState("");
  const [plan, setPlan] = useState<SpecificationImportPlan | null>(null);
  const [busy, setBusy] = useState(false);
  const [uncertain, setUncertain] = useState(false);
  const [error, setError] = useState("");
  const [confirmClose, setConfirmClose] = useState(false);
  const dirty = !!(source || url || plan);
  const blocker = useBlocker(dirty || busy || uncertain);
  // Native navigation must not discard a reviewed draft or hide an unresolved commit.
  useBeforeUnload((event) => { if (dirty || busy || uncertain) { event.preventDefault(); event.returnValue = ""; } });

  /** Retains unsaved source until the owner deliberately confirms closing the drawer. */
  function close() {
    // A pending or unknown write must be resolved before losing its status receipt.
    if (busy || uncertain) return;
    if (dirty) { setConfirmClose(true); return; }
    onClose();
  }

  /** Reviews the complete source without authorizing creation of a different service or version. */
  async function review(event: FormEvent) {
    event.preventDefault();
    // A pinned service slug is necessary for the existing Registry import lookup.
    if (!service.slug || busy || uncertain) return;
    setBusy(true); setError("");
    try {
      // Exactly one input source avoids ambiguous URL-versus-upload precedence.
      const input = source.trim() ? { source_content: source } : { source_url: url.trim() };
      const next = await api.integrations.planImport({ name: service.name, slug: service.slug, version, target_type: "endpoints", ...input });
      // An older or incompatible planner cannot redirect an existing-service action into creation.
      if (!matchesEndpointPlan(next, service.id, version)) throw new Error("The import does not target this service's endpoints and selected version. Reload before importing.");
      setPlan(next);
    } catch (failure) { setError(String((failure as Error).message)); }
    finally { setBusy(false); }
  }

  /** Applies only the reviewed receipt and preserves uncertain outcomes for explicit status recovery. */
  async function apply() {
    // A write cannot be retried while its durable result is unknown.
    if (!plan || busy || uncertain) return;
    setBusy(true); setError("");
    try {
      const result = await api.integrations.applyImport(plan.plan_id, plan.review_hash);
      // HTTP success must still identify the selected service and version before closing the drawer.
      if (result.status !== "applied" || result.service_id !== service.id || result.version !== version || result.is_new_service) throw new Error("The result did not confirm this service version. Check import status.");
      onSaved();
    } catch (failure) {
      setError(String((failure as Error).message));
      // Network failures may follow a committed write; never offer another apply blindly.
      setUncertain(webhookApplyNeedsStatus(failure));
    } finally { setBusy(false); }
  }

  /** Resolves an interrupted apply using its original operation receipt without replaying a mutation. */
  async function checkStatus() {
    // Recovery belongs to one reviewed plan and cannot overlap another request.
    if (!plan || busy || !uncertain) return;
    setBusy(true);
    try {
      const result = await api.integrations.importStatus(plan.plan_id);
      // Only matching committed state confirms success; other outcomes keep the recovery receipt visible.
      if (endpointImportCommitted(result, service.id, version)) { onSaved(); return; }
      // A definitive rollback permits a fresh review; an ongoing or unknown outcome stays locked.
      if (result.commit_state === "not_committed" || result.commit_state === "impossible") { setUncertain(false); setPlan(null); }
      setError(result.guidance || "The import has not confirmed completion. Check its status again.");
    } catch (failure) { setError(String((failure as Error).message)); }
    finally { setBusy(false); }
  }

  /** Bounds file reads and makes each new source invalidate the earlier review. */
  async function loadFile(file?: File) {
    // Cancelling the picker is not an instruction to discard the current source.
    if (!file || busy || uncertain) return;
    setBusy(true); setError(""); setPlan(null);
    try {
      // Match the existing specification upload limit before allocating the file contents.
      if (file.size > 5 * 1024 * 1024) throw new Error("File is too large. Maximum size is 5 MB.");
      setSource(await file.text()); setURL("");
    } catch (failure) { setError(String((failure as Error).message)); }
    finally { setBusy(false); }
  }

  return { source, setSource, url, setURL, plan, setPlan, busy, uncertain, error, confirmClose, setConfirmClose, blocker, close, review, apply, checkStatus, loadFile };
}

type Editor = ReturnType<typeof useEndpointImport>;

/** Reuses Registry's reviewed endpoint import contract while pinning this existing service and version. */
export function ServiceEndpointImport(props: ImportProps) {
  const editor = useEndpointImport(props);
  const { service, version } = props;
  const panel = useRef<HTMLElement>(null);
  // The modal receives focus on open and returns it to the invoking control when closed.
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null;
    panel.current?.focus();
    return () => previous?.focus();
  }, []);
  return <>
    <div className="fixed inset-0 z-40 bg-slate-900/30" aria-hidden="true" onClick={editor.close} />
    <section ref={panel} tabIndex={-1} onKeyDown={(event) => importKeyboard(event, panel.current, editor.close)} data-fused-detail-sidebar role="dialog" aria-modal="true" aria-labelledby="endpoint-import-title" className="fixed inset-y-0 right-0 z-50 flex w-full max-w-2xl flex-col overflow-y-auto bg-white p-6 shadow-xl">
      <header className="mb-6 flex items-start justify-between gap-4"><div><h2 id="endpoint-import-title" className="text-lg font-semibold">Import endpoints</h2><p className="mt-1 text-sm text-slate-500">{service.name} · {version}</p></div><button type="button" aria-label="Close endpoint import" disabled={editor.busy || editor.uncertain} onClick={editor.close}>✕</button></header>
      <p className="mb-5 text-sm text-slate-600">Import a complete specification for this version. Review additions, changes, and removals before applying.</p>
      {/* Errors preserve the source and receipt so retry or status recovery remains deliberate. */}
      {editor.error && <p role="alert" className="mb-4 rounded-md bg-red-50 p-3 text-sm text-red-700">{editor.error}</p>}
      <EndpointImportSource editor={editor} service={service} />
      {/* Unknown commits can be inspected, but neither replayed nor silently dismissed. */}
      {editor.uncertain && <button type="button" className="mt-4 text-sm font-medium text-blue-600" disabled={editor.busy} onClick={editor.checkStatus}>Check import status</button>}
      {editor.busy && <p role="status" className="mt-4 text-sm text-slate-500">Processing import…</p>}
      <EndpointImportDiscard editor={editor} onClose={props.onClose} />
    </section>
  </>;
}

/** Source editing and reviewed receipts are mutually exclusive to prevent stale-source submission. */
function EndpointImportSource({ editor, service }: { editor: Editor; service: Service }) {
  const { plan, busy, uncertain } = editor;
  // A reviewed plan locks source editing until the owner explicitly returns to authoring.
  if (plan) return <div className="space-y-5"><p className="text-sm">{plan.diff.added} added · {plan.diff.changed} changed · {plan.diff.removed} removed</p><div className="flex gap-3"><button className={buttonClass} disabled={busy || uncertain} onClick={editor.apply}>Import endpoints</button><button type="button" disabled={busy || uncertain} onClick={() => editor.setPlan(null)} className="text-sm text-slate-600">Change source</button></div></div>;
  return <form onSubmit={editor.review} className="space-y-4"><fieldset disabled={busy || uncertain} className="space-y-4">
    <label className="block space-y-1 text-sm"><span>Specification URL</span><input type="url" className={fieldClass} value={editor.url} disabled={!!editor.source} onChange={(event) => editor.setURL(event.target.value)} placeholder="https://api.example.com/openapi.json" /></label>
    <label className="block space-y-1 text-sm"><span>Upload specification</span><input type="file" accept=".json,.yaml,.yml,.graphql" className={fieldClass} onChange={(event) => void editor.loadFile(event.target.files?.[0])} /></label>
    <label className="block space-y-1 text-sm"><span>Or paste a specification</span><textarea className={`${fieldClass} font-mono`} rows={10} value={editor.source} onChange={(event) => { editor.setSource(event.target.value); editor.setURL(""); }} /></label>
    <button className={buttonClass} type="submit" disabled={busy || (!editor.source.trim() && !editor.url.trim()) || !service.slug}>Review import</button>
  </fieldset></form>;
}

/** Both close and route navigation require deliberate discard of local drafts. */
function EndpointImportDiscard({ editor, onClose }: { editor: Editor; onClose: () => void }) {
  // No confirmation is mounted until an actual close or navigation attempts to discard work.
  if (!editor.confirmClose && editor.blocker.state !== "blocked") return null;
  /** The router owns blocked navigation; ordinary close only dismisses this panel. */
  function discard() {
    if (editor.blocker.state === "blocked") editor.blocker.proceed();
    else onClose();
  }
  /** Returning to the draft cancels either kind of pending dismissal. */
  function keep() { editor.setConfirmClose(false); editor.blocker.reset?.(); }
  return <div className="mt-5 rounded-md border border-amber-200 bg-amber-50 p-4 text-sm"><p>Discard the unsaved endpoint import?</p><div className="mt-3 flex gap-4"><button disabled={editor.busy || editor.uncertain} onClick={keep}>Keep editing</button><button disabled={editor.busy || editor.uncertain} onClick={discard}>Discard and continue</button></div></div>;
}

/** Keeps keyboard navigation inside the import dialog and routes Escape through its discard safeguards. */
function importKeyboard(event: KeyboardEvent, panel: HTMLElement | null, close: () => void) {
  // Escape must respect pending and uncertain writes rather than dismissing the modal directly.
  if (event.key === "Escape") { event.stopPropagation(); close(); return; }
  // Ordinary typing remains owned by each input.
  if (event.key !== "Tab" || !panel) return;
  const controls = Array.from(panel.querySelectorAll<HTMLElement>('button:not(:disabled), input:not(:disabled), textarea:not(:disabled), [tabindex="0"]')).filter((node) => node.getClientRects().length > 0);
  const first = controls[0];
  const last = controls[controls.length - 1];
  // Wrapping prevents keyboard access to background service actions while a draft is open.
  if (event.shiftKey && [first, panel].includes(document.activeElement as HTMLElement)) { event.preventDefault(); last?.focus(); }
  else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first?.focus(); }
}
