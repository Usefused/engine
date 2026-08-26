import { useEffect, useRef, useState } from "react";
import { useBeforeUnload, useBlocker } from "@remix-run/react";
import type { Service, SpecificationImportPlan } from "~/lib/api";
import { useWebhookEditor } from "./useWebhookEditor";
import { WebhookEventEditor } from "./WebhookEventEditor";
import { WebhookSettingsEditor } from "./WebhookSettingsEditor";

type Editor = ReturnType<typeof useWebhookEditor>;
interface Props { service: Service; version: string; onClose: () => void; onSaved: () => void }

// Mounting is the explicit owner click boundary; no source or credentials are loaded by the normal view.
export function WebhookDefinitionEditor({ service, version, onClose, onSaved }: Props) {
  const editor = useWebhookEditor(service, version, onSaved);
  const [invalid, setInvalid] = useState(false);
  const [confirmClose, setConfirmClose] = useState(false);
  const panel = useRef<HTMLDivElement>(null);
  const protectedDraft = hasUnsavedWebhookWork(editor);
  const blocker = useBlocker(protectedDraft || editor.busy);
  useBeforeUnload((event) => {
    // Native refresh must not silently discard a draft or hide an in-flight apply.
    if (protectedDraft || editor.busy) { event.preventDefault(); event.returnValue = ""; }
  });
  useEffect(() => { panel.current?.focus(); }, []);
  // Invalid composite text must invalidate review even when it cannot enter the JSON document.
  function invalidChanged(value: boolean) { setInvalid(value); if (value) editor.markDirty(); }
  // Deliberately selecting a fresh baseline also resets validation left by the old form's local text.
  function startLatest() { editor.startFromLatest(); setInvalid(false); }
  // Closing never submits or silently throws away an owner-authored draft.
  function close() {
    // In-flight work must settle before the drawer can disappear.
    if (editor.busy) return;
    // Explicit confirmation distinguishes intentional discard from an accidental backdrop click.
    if (protectedDraft) { setConfirmClose(true); return; }
    onClose();
  }
  return <>
    <div className="fixed inset-0 z-40 bg-slate-900/30" aria-hidden="true" onClick={close} />
    <div ref={panel} tabIndex={-1} role="dialog" aria-modal="true" aria-labelledby="webhook-editor-title" className="fixed inset-y-0 right-0 z-50 flex w-full max-w-3xl flex-col bg-white shadow-2xl outline-none" onKeyDown={(event) => handleEditorKeyboard(event, panel.current, close)}>
      <header className="flex items-start justify-between gap-4 border-b border-slate-200 p-4 sm:p-6"><div><h2 id="webhook-editor-title" className="text-lg font-semibold">Edit webhook</h2><p className="mt-1 break-all text-sm text-slate-500">{service.name} · {version}</p></div><button type="button" aria-label="Close webhook editor" disabled={editor.busy} onClick={close} className="p-2 text-slate-600 disabled:opacity-40">✕</button></header>
      <div className="min-h-0 flex-1 space-y-5 overflow-y-auto p-4 sm:p-6">
        {confirmClose && <DiscardNotice busy={editor.busy} onKeep={() => setConfirmClose(false)} onDiscard={onClose} />}
        <EditorError editor={editor} />
        <WebhookConflictRecovery editor={editor} onStartLatest={startLatest} />
        {editor.retainedSource && <RetainedWebhookDraft source={editor.retainedSource} label="Your retained draft before reloading" />}
        {editor.busy && <p role="status" className="text-sm text-slate-500">Processing webhook definition…</p>}
        <EditorDraftForm editor={editor} onInvalid={invalidChanged} />
        {editor.pendingFile && <section className="space-y-3 rounded-lg border border-amber-200 bg-amber-50 p-4"><h3 className="font-semibold">Replace draft with imported webhooks?</h3><p className="text-sm">This replaces the complete event catalogue and shared webhook settings. Nothing has been saved.</p><WebhookPlanReview plan={editor.pendingFile.plan} /><div className="flex gap-4"><button type="button" disabled={editor.busy} onClick={() => { editor.acceptFile(); setInvalid(false); }} className="text-sm font-semibold text-indigo-700">Use imported definition</button><button type="button" onClick={editor.cancelFile} className="text-sm">Keep current draft</button></div></section>}
        {editor.plan && <WebhookPlanReview plan={editor.plan} />}
        {blocker.state === "blocked" && <DiscardNotice busy={editor.busy} onKeep={() => blocker.reset?.()} onDiscard={() => blocker.proceed?.()} />}
      </div>
      <EditorFooter editor={editor} invalid={invalid} onClose={close} />
    </div>
  </>;
}

// A reference copy and unresolved outcome deserve the same deliberate discard boundary as editable changes.
function hasUnsavedWebhookWork(editor: Editor): boolean {
  return editor.dirty || editor.uncertain || editor.staleTarget || Boolean(editor.retainedSource);
}

// Remounting at a new baseline clears only old form-local validation after the owner's explicit choice.
function EditorDraftForm({ editor, onInvalid }: { editor: Editor; onInvalid: (invalid: boolean) => void }) {
  // The initial read must finish before a complete replacement can be edited.
  if (!editor.draft) return null;
  return <fieldset key={editor.baselineRevision} disabled={editor.busy || editor.uncertain || editor.staleTarget} className="min-w-0 space-y-6 disabled:opacity-70">
    <OpenAPIInput editor={editor} />
    <WebhookEventEditor draft={editor.draft} onChange={editor.change} />
    <WebhookSettingsEditor draft={editor.draft} onChange={editor.change} onInvalid={onInvalid} />
  </fieldset>;
}

// Conflict recovery separates a read-only comparison from the intentional discard of the stale editing baseline.
function WebhookConflictRecovery({ editor, onStartLatest }: { editor: Editor; onStartLatest: () => void }) {
  // Unknown commits never reach this flow; they must use the durable status action instead.
  if (!editor.staleTarget) return null;
  return <section aria-label="Webhook conflict recovery" className="space-y-3 rounded-lg border border-amber-200 bg-amber-50 p-4 text-sm">
    <h3 className="font-semibold">This service version changed</h3>
    <p>Your draft is kept below. Load the latest definition for comparison, then deliberately start from it and reapply your changes. Nothing is saved by either recovery action.</p>
    <button type="button" disabled={editor.busy || editor.uncertain} onClick={() => void editor.loadLatest()} className="font-semibold text-indigo-700 disabled:opacity-50">Load latest for comparison</button>
    {editor.latest && <>
      <p>Latest revision: {editor.latest.source.revision} · {editor.latest.draft.events.length} event(s). Your editor still contains the original draft.</p>
      <RetainedWebhookDraft source={editor.latest.source.source_content} label="Latest saved definition" />
      <RetainedWebhookDraft source={editor.latest.referenceSource} label="Your current unsaved draft" />
      {editor.retainedSource && <p>Starting from latest replaces the earlier reference copy below with your current draft. Copy the earlier one first if you still need it.</p>}
      <button type="button" disabled={editor.busy || editor.uncertain} onClick={onStartLatest} className="font-semibold text-indigo-700 disabled:opacity-50">Start from latest; keep my draft for reference</button>
    </>}
  </section>;
}

// Exact source stays selectable without browser storage, clipboard permissions, or automatic full-document replay.
function RetainedWebhookDraft({ source, label }: { source: string; label: string }) {
  return <details className="space-y-2 rounded-lg border border-slate-200 bg-white p-3 text-sm">
    <summary className="cursor-pointer font-medium">{label}</summary>
    <p className="text-xs text-slate-600">Copy only the changes you intend to reapply. Importing an old full definition would overwrite other people's newer changes.</p>
    <textarea aria-label={label} readOnly value={source} rows={10} className="w-full rounded border border-slate-300 p-2 font-mono text-xs" />
  </details>;
}

// Reuse one explicit confirmation for closing and navigation without relying on browser-native prompts.
function DiscardNotice({ busy, onKeep, onDiscard }: { busy: boolean; onKeep: () => void; onDiscard: () => void }) {
  return <section role="alert" className="space-y-3 rounded-lg border border-amber-200 bg-amber-50 p-4"><p className="text-sm">Leaving or switching versions discards unsaved webhook changes and any retained reference copy.</p><div className="flex gap-4"><button type="button" onClick={onKeep} className="text-sm font-semibold">Keep editing</button><button type="button" disabled={busy} onClick={onDiscard} className="text-sm text-red-700">Discard and continue</button></div></section>;
}

// Keep keyboard navigation inside the active modal while allowing a deliberate Escape close.
function handleEditorKeyboard(event: React.KeyboardEvent, panel: HTMLDivElement | null, close: () => void) {
  // Escape follows the same dirty/busy guard as every other close gesture.
  if (event.key === "Escape") { event.stopPropagation(); close(); return; }
  // Only Tab needs a focus boundary; other editing keys retain native behaviour.
  if (event.key !== "Tab" || !panel) return;
  const controls = Array.from(panel.querySelectorAll<HTMLElement>('button:not(:disabled), input:not(:disabled), select:not(:disabled), textarea:not(:disabled), summary, [tabindex="0"]')).filter((node) => node.getClientRects().length > 0);
  const first = controls[0];
  const last = controls[controls.length - 1];
  // Wrapping at either edge prevents focus from reaching the dimmed read-only page.
  if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last?.focus(); }
  else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first?.focus(); }
}

// File selection starts only a server-side preview and does not modify the current draft yet.
function OpenAPIInput({ editor }: { editor: Editor }) {
  return <section className="rounded-lg border border-slate-200 p-3"><label className="block text-sm font-medium">Import OpenAPI<input type="file" accept=".json,.yaml,.yml,application/json,application/yaml" className="mt-2 block w-full text-sm" onChange={(event) => { const file = event.target.files?.[0]; if (file) void editor.importFile(file); event.target.value = ""; }} /></label><p className="mt-2 text-xs text-slate-500">Optional. Preview and confirm replacement first; uploading never saves automatically. Maximum 4 MiB.</p></section>;
}

// The review is the server receipt, including removals and dependent workspace warnings.
function WebhookPlanReview({ plan }: { plan: SpecificationImportPlan }) {
  return <section aria-label="Webhook change review" className="space-y-2 rounded-lg border border-slate-200 p-3 text-sm"><h3 className="font-semibold">Review changes</h3><p>Destination: {plan.name} · {plan.target_version}</p><p>{plan.diff.added} added · {plan.diff.changed} changed · {plan.diff.removed} removed</p><ReviewNames label="Changed events" names={plan.diff.changed_names} /><ReviewNames label="Removed events" names={plan.diff.removed_names} />{plan.diff.settings_changed && <p className="font-medium text-amber-800">Shared webhook settings will change.</p>}<p className="text-slate-500">This review includes the draft's event extraction and verification settings. Saving does not configure receiving URLs or change bucket credentials.</p>{plan.usage && <p className="text-amber-800">{plan.usage.workspaces.length} workspace(s) rely on this service version.</p>}{Boolean(plan.diagnostics) && <details><summary className="cursor-pointer">Import diagnostics</summary><pre className="mt-2 max-h-48 overflow-auto whitespace-pre-wrap break-all text-xs">{JSON.stringify(plan.diagnostics, null, 2)}</pre></details>}</section>;
}

// Explicit names make destructive catalogue removals visible without expanding every full operation.
function ReviewNames({ label, names }: { label: string; names?: string[] }) {
  // Empty name lists need no additional visual noise beyond the aggregate counts.
  if (!names?.length) return null;
  return <details><summary className="cursor-pointer">{label} ({names.length})</summary><ul className="mt-2 max-h-40 overflow-y-auto break-all pl-4">{names.map((name) => <li key={name}>{name}</li>)}</ul></details>;
}

// Recovery metadata remains visible and unknown commit outcomes never present an ordinary retry button.
function EditorError({ editor }: { editor: Editor }) {
  // No error means there is no failure or recovery state to announce.
  if (!editor.error) return null;
  return <div role="alert" className="space-y-2 break-words rounded-lg border border-red-200 bg-red-50 p-3 text-sm text-red-900"><p className="whitespace-pre-wrap">{editor.error}</p>{editor.uncertain && <><p>The apply outcome is not yet confirmed. Check its durable status before attempting another save.</p><button type="button" disabled={editor.busy} onClick={() => void editor.checkStatus()} className="font-semibold underline">Check import status</button></>}</div>;
}

// Save appears only after review, and invalid edits or unresolved apply outcomes cannot reuse that receipt.
function EditorFooter({ editor, invalid, onClose }: { editor: Editor; invalid: boolean; onClose: () => void }) {
  const disabled = editor.busy || invalid || !editor.draft || editor.uncertain || editor.staleTarget || Boolean(editor.pendingFile);
  return <footer className="flex items-center justify-between gap-3 border-t border-slate-200 p-4 sm:px-6"><button type="button" disabled={editor.busy} onClick={onClose} className="rounded-md border border-slate-300 px-4 py-2 text-sm disabled:opacity-50">Cancel</button>{editor.plan ? <button type="button" disabled={disabled} onClick={() => void editor.save()} className="rounded-md bg-indigo-600 px-4 py-2 text-sm font-medium text-white disabled:opacity-50">Save changes</button> : <button type="button" disabled={disabled || !editor.dirty} onClick={() => void editor.review()} className="rounded-md bg-indigo-600 px-4 py-2 text-sm font-medium text-white disabled:opacity-50">Review changes</button>}</footer>;
}
