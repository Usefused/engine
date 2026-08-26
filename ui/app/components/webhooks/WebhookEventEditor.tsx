import { newWebhookEvent, webhookPayloadField, webhookRecord, type WebhookDraftEvent, type WebhookEditorDraft } from "~/lib/webhook-editor-draft";

export const webhookFieldClass = "mt-1 w-full rounded-md border border-slate-300 px-3 py-2 text-sm text-slate-900 disabled:bg-slate-100";

// Developer-facing authoring keeps the catalogue terse while retaining opaque operation metadata.
export function WebhookEventEditor({ draft, onChange }: { draft: WebhookEditorDraft; onChange: (next: WebhookEditorDraft) => void }) {
  // Stable row IDs prevent renaming an event from resetting its form focus.
  function update(id: string, patch: Partial<WebhookDraftEvent>) {
    onChange({ ...draft, events: draft.events.map((event) => event.id === id ? { ...event, ...patch } : event) });
  }
  // Deletion is explicit and remains subject to the subsequent server removal review.
  function remove(id: string) { onChange({ ...draft, events: draft.events.filter((event) => event.id !== id) }); }
  return <section className="space-y-3" aria-label="Webhook events">
    <div className="flex items-center justify-between"><h3 className="font-semibold">Events ({draft.events.length})</h3><button type="button" className="text-sm font-medium text-indigo-700" onClick={() => onChange({ ...draft, events: [...draft.events, newWebhookEvent()] })}>Add event</button></div>
    {draft.events.map((event) => <EventFields key={event.id} event={event} onChange={(patch) => update(event.id, patch)} onRemove={() => remove(event.id)} />)}
  </section>;
}

// Delivery overrides stay secondary to event authoring and never rebuild the preserved OpenAPI operation.
function EventFields({ event, onChange, onRemove }: { event: WebhookDraftEvent; onChange: (patch: Partial<WebhookDraftEvent>) => void; onRemove: () => void }) {
  return <fieldset className="min-w-0 rounded-lg border border-slate-200 p-3">
    <legend className="sr-only">Webhook event</legend>
    <label className="block text-sm">Event name<input className={webhookFieldClass} value={event.name} onChange={(e) => onChange({ name: e.target.value })} placeholder="invoice.created" /></label>
    <label className="mt-3 block text-sm">Description<textarea className={webhookFieldClass} rows={2} value={event.description} onChange={(e) => onChange({ description: e.target.value })} /></label>
    <DeliveryMethod event={event} onChange={onChange} />
    <PayloadFields event={event} onChange={onChange} />
    <button type="button" className="mt-3 text-sm text-red-700" onClick={onRemove}>Remove event</button>
  </fieldset>;
}

// Concise capability warnings distinguish preserved imports from executable methods without teaching HTTP semantics.
function DeliveryMethod({ event, onChange }: { event: WebhookDraftEvent; onChange: (patch: Partial<WebhookDraftEvent>) => void }) {
  // GET has real query-delivery support, while other OpenAPI verbs are retained contract metadata rather than receiver capabilities.
  const supported = event.method === "post" || event.method === "get";
  // Non-default transports need immediate visibility; the default POST control stays out of the primary editing flow.
  return <details className="mt-3 text-sm" open={event.method !== "post"}>
    <summary className="cursor-pointer text-slate-600">Delivery method: {event.method.toUpperCase()} · Advanced</summary>
    <div className="space-y-2 pt-3">
      <label className="block">HTTP delivery method<select className={webhookFieldClass} value={event.method} onChange={(e) => onChange({ method: e.target.value })}>
        <option value="post">POST (default)</option>
        <option value="get">GET</option>
        {/* A selected disabled option exposes the original value without offering unsupported verbs for new events. */}
        {!supported && <option value={event.method} disabled>{event.method.toUpperCase()} (imported; unsupported)</option>}
      </select></label>
      {/* Non-default imports must not silently acquire POST semantics or appear executable by this receiver. */}
      {!supported && <p role="note" className="text-sm text-amber-800">{event.method.toUpperCase()} is preserved from the imported spec but unsupported by Engine ingress (POST/GET only).</p>}
      {/* Undo restores only this row's original transport, without offering unsupported verbs for unrelated events. */}
      {event.method !== event.originalMethod && <button type="button" className="text-sm font-medium text-indigo-700" onClick={() => onChange({ method: event.originalMethod })}>Restore original {event.originalMethod.toUpperCase()} method</button>}
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
