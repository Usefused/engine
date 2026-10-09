import { useState, type ChangeEvent, type SyntheticEvent } from "react";
import { ChevronDown, Plus, Trash2 } from "lucide-react";
import { Select } from "../forms/Select.ts";
import { newWebhookEvent, webhookPayloadField, webhookRecord, type WebhookDraftEvent, type WebhookEditorDraft } from "~/lib/webhook-editor-draft";

export const webhookFieldClass = "mt-2 block w-full rounded-lg border border-slate-200 bg-white px-3 py-2.5 text-sm font-normal text-slate-900 shadow-sm transition-colors placeholder:text-slate-400 focus:border-violet-400 focus:outline-none focus:ring-2 focus:ring-violet-100 disabled:bg-slate-100";

// Developer-facing authoring keeps the catalogue terse while retaining opaque operation metadata.
export function WebhookEventEditor({ draft, onChange }: { draft: WebhookEditorDraft; onChange: (next: WebhookEditorDraft) => void }) {
  // Stable row IDs prevent renaming an event from resetting its form focus.
  function update(id: string, patch: Partial<WebhookDraftEvent>) {
    onChange({ ...draft, events: draft.events.map((event) => event.id === id ? { ...event, ...patch } : event) });
  }
  // Deletion is explicit and remains subject to the subsequent server removal review.
  function remove(id: string) { onChange({ ...draft, events: draft.events.filter((event) => event.id !== id) }); }
  // New events open immediately so adding a row leads directly into its required name field.
  function add() { onChange({ ...draft, events: [...draft.events, newWebhookEvent()] }); }
  return <section className="space-y-3" aria-label="Webhook events">
    <div className="flex items-center justify-between gap-3"><h3 className="text-sm font-semibold text-slate-900">Events <span className="ml-1 text-slate-400 font-normal">{draft.events.length}</span></h3><button type="button" className="inline-flex items-center gap-1.5 rounded-md px-2 py-1.5 text-sm font-medium text-slate-700 hover:bg-slate-100" onClick={add}><Plus className="h-4 w-4" />Add event</button></div>
    {draft.events.map((event) => <EventFields key={event.id} event={event} onChange={(patch) => update(event.id, patch)} onRemove={() => remove(event.id)} />)}
  </section>;
}

// Collapsed rows keep large catalogues scannable; new or unusual transports open for immediate review.
function EventFields({ event, onChange, onRemove }: { event: WebhookDraftEvent; onChange: (patch: Partial<WebhookDraftEvent>) => void; onRemove: () => void }) {
  // A new row or imported non-default transport must not conceal required input or a delivery warning.
  const [expanded, setExpanded] = useState(!event.name || event.method !== "post");
  // Keep expansion stable while typing a new name instead of collapsing on the first character.
  function toggled(change: SyntheticEvent<HTMLDetailsElement>) { setExpanded(change.currentTarget.open); }
  // Name edits retain the row's stable identity and opaque operation metadata.
  function changeName(change: ChangeEvent<HTMLInputElement>) { onChange({ name: change.target.value }); }
  // Descriptive copy is a draft change, never an immediate service mutation.
  function changeDescription(change: ChangeEvent<HTMLTextAreaElement>) { onChange({ description: change.target.value }); }
  return <details open={expanded} onToggle={toggled} className="group/event min-w-0 overflow-hidden rounded-xl border border-slate-200 bg-white">
    <summary className="flex cursor-pointer list-none items-center gap-3 px-4 py-3.5 hover:bg-slate-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-violet-300 [&::-webkit-details-marker]:hidden">
      {/* Empty labels are only a local draft placeholder, never a saved event name. */}
      <span className="min-w-0 flex-1 break-words font-mono text-sm font-medium text-slate-800 [overflow-wrap:anywhere]">{event.name || "New event"}</span>
      <span className="shrink-0 text-[11px] font-medium text-slate-400">{event.method.toUpperCase()}</span>
      <ChevronDown className="h-4 w-4 shrink-0 text-slate-400 transition-transform group-open/event:rotate-180" />
    </summary>
    <fieldset className="min-w-0 border-t border-slate-100 p-4">
      <legend className="sr-only">Webhook event</legend>
      <label className="block text-xs font-medium text-slate-600">Event name <span className="text-slate-400" aria-hidden="true">*</span><input required aria-label="Event name" className={webhookFieldClass} value={event.name} onChange={changeName} placeholder="invoice.created" /></label>
      <label className="mt-4 block text-xs font-medium text-slate-600">Description<textarea className={webhookFieldClass} rows={2} value={event.description} onChange={changeDescription} placeholder="What happens when this event is sent?" /></label>
      <div className="mt-4 border-t border-slate-100 pt-1"><DeliveryMethod event={event} onChange={onChange} /><PayloadFields event={event} onChange={onChange} /></div>
      <div className="mt-4 flex border-t border-slate-100 pt-3"><button type="button" className="inline-flex items-center gap-1.5 rounded-md px-2 py-1.5 text-xs font-medium text-slate-500 hover:bg-red-50 hover:text-red-700" onClick={onRemove}><Trash2 className="h-3.5 w-3.5" />Remove event</button></div>
    </fieldset>
  </details>;
}

// Concise capability warnings distinguish preserved imports from executable methods without teaching HTTP semantics.
function DeliveryMethod({ event, onChange }: { event: WebhookDraftEvent; onChange: (patch: Partial<WebhookDraftEvent>) => void }) {
  // GET has real query-delivery support, while other OpenAPI verbs are retained contract metadata rather than receiver capabilities.
  const supported = event.method === "post" || event.method === "get";
  // Non-default transports need immediate visibility; the default POST control stays out of the primary editing flow.
  return <details className="mt-3 text-sm" open={event.method !== "post"}>
    <summary className="cursor-pointer text-slate-600">Delivery method: {event.method.toUpperCase()} · Advanced</summary>
    <div className="space-y-2 pt-3">
      <label className="block">HTTP delivery method<Select className={webhookFieldClass} value={event.method} onChange={(e) => onChange({ method: e.target.value })}>
        <option value="post">POST (default)</option>
        <option value="get">GET</option>
        {/* A selected disabled option exposes the original value without offering unsupported verbs for new events. */}
        {!supported && <option value={event.method} disabled>{event.method.toUpperCase()} (imported; unsupported)</option>}
      </Select></label>
      {/* Non-default imports must not silently acquire POST semantics or appear executable by this receiver. */}
      {!supported && <p role="note" className="text-sm text-amber-800">{event.method.toUpperCase()} is preserved from the imported spec but unsupported by Fused ingress (POST/GET only).</p>}
      {/* Undo restores only this row's original transport, without offering unsupported verbs for unrelated events. */}
      {event.method !== event.originalMethod && <button type="button" className="text-sm font-medium text-slate-950" onClick={() => onChange({ method: event.originalMethod })}>Restore original {event.originalMethod.toUpperCase()} method</button>}
    </div>
  </details>;
}

// Shared request-body references stay read-only so a payload edit cannot silently inline or detach them.
function PayloadFields({ event, onChange }: { event: WebhookDraftEvent; onChange: (patch: Partial<WebhookDraftEvent>) => void }) {
  const referenced = Boolean(webhookRecord(event.operation.requestBody).$ref);
  return <details className="mt-3 text-sm">
    <summary className="cursor-pointer text-slate-600">Optional JSON payload</summary>
    {referenced ? <p className="mt-2 text-slate-500">This event uses a shared request-body reference. It is preserved; use OpenAPI to edit that definition.</p> : <div className="space-y-3 pt-3">
      <label className="block">JSON Schema<textarea className={`${webhookFieldClass} font-mono text-xs`} rows={4} value={event.schemaText ?? webhookPayloadField(event, "schema")} onChange={(e) => onChange({ schemaText: e.target.value })} placeholder="Optional; references are preserved" /></label>
      <label className="block">Example JSON<textarea className={`${webhookFieldClass} font-mono text-xs`} rows={3} value={event.exampleText ?? webhookPayloadField(event, "example")} onChange={(e) => onChange({ exampleText: e.target.value })} placeholder="Optional; no schema is inferred from this example" /></label>
    </div>}
  </details>;
}
