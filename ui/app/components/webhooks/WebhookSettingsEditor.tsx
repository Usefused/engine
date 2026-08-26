import { useEffect, useState } from "react";
import { updateWebhookSetting, webhookDiscriminatorText, webhookDiscriminatorValue, webhookRecord, type WebhookDocument, type WebhookEditorDraft } from "~/lib/webhook-editor-draft";
import { webhookFieldClass } from "./WebhookEventEditor";

// Settings define verification instructions only; bucket secrets and registration bindings are never loaded.
export function WebhookSettingsEditor({ draft, onChange, onInvalid }: { draft: WebhookEditorDraft; onChange: (next: WebhookEditorDraft) => void; onInvalid: (invalid: boolean) => void }) {
  const config = webhookRecord(draft.document["x-fused-webhook"]);
  const advanced = Boolean(draft.document["x-fused-signature-policy"] || config.signature_policy);
  // Extension replacement preserves the full document and invalidates review in the caller.
  function setConfig(value: WebhookDocument) { onChange(updateWebhookSetting(draft, "x-fused-webhook", value)); }
  return <section className="space-y-3 border-t border-slate-200 pt-5" aria-label="Webhook definition settings">
    <h3 className="font-semibold">Settings</h3>
    <p className="text-sm text-slate-500">These are shared service rules, not workspace setup. Signing secrets and tokens remain in credential buckets.</p>
    <DiscriminatorField draft={draft} onChange={onChange} onInvalid={onInvalid} />
    {advanced ? <AdvancedVerification document={draft.document} /> : <VerificationFields config={config} onChange={setConfig} />}
  </section>;
}

// Composite routing uses the existing extension grammar and is checked by the ordinary server planner.
function DiscriminatorField({ draft, onChange, onInvalid }: { draft: WebhookEditorDraft; onChange: (next: WebhookEditorDraft) => void; onInvalid: (invalid: boolean) => void }) {
  const [text, setText] = useState(() => webhookDiscriminatorText(draft.document));
  const [error, setError] = useState("");
  const currentText = webhookDiscriminatorText(draft.document);
  // Accepting an imported draft must replace the displayed routing value as well as its backing document.
  useEffect(() => { setText(currentText); setError(""); }, [currentText]);
  // Invalid JSON stays visible and disables review instead of falling back to the previous valid value.
  function change(value: string) {
    setText(value);
    try { onChange(updateWebhookSetting(draft, "x-fused-event-discriminator", webhookDiscriminatorValue(value))); setError(""); onInvalid(false); }
    catch { setError("Enter a path or valid JSON for composite extraction."); onInvalid(true); }
  }
  return <label className="block text-sm">Event extraction path<input className={webhookFieldClass} value={text} onChange={(e) => change(e.target.value)} placeholder="body.type or header.X-Event-Type" /><span className="mt-1 block text-xs text-slate-500">Supports body., header., query., or JSON arrays for composite extraction. Existing fallback rules are preserved.</span>{error && <span role="alert" className="text-xs text-red-700">{error}</span>}</label>;
}

// Switching primitive verification modes deliberately discards incompatible fields and requires review.
function VerificationFields({ config, onChange }: { config: WebhookDocument; onChange: (next: WebhookDocument) => void }) {
  const authType = String(config.auth_type ?? "none");
  // The server owns validation; local defaults only provide fields implemented by the existing runtime.
  function changeType(type: string) {
    // Disabled verification must not carry stale signature or token instructions.
    if (type === "none") { onChange({ auth_type: "none" }); return; }
    onChange({ auth_type: type, auth_location: "header", auth_key_name: "" });
  }
  return <div className="space-y-3">
    <label className="block text-sm">Verification method<select className={webhookFieldClass} value={authType} onChange={(e) => changeType(e.target.value)}><option value="none">None</option><option value="static_token">Static token</option><option value="hmac_signature">HMAC signature</option><option value="signature_header">Signature headers</option></select></label>
    {authType !== "none" && <VerificationKeyFields config={config} onChange={onChange} />}
    <p className="text-xs text-slate-500">Changing verification affects service users. This form never asks for the token or signing secret itself.</p>
  </div>;
}

// Header/query names identify incoming fields, not credential values or bucket keys.
function VerificationKeyFields({ config, onChange }: { config: WebhookDocument; onChange: (next: WebhookDocument) => void }) {
  const signature = config.auth_type !== "static_token";
  return <div className="space-y-3">
    <label className="block text-sm">Incoming credential location<select className={webhookFieldClass} value={String(config.auth_location ?? "header")} onChange={(e) => onChange({ ...config, auth_location: e.target.value })}><option value="header">Header</option><option value="query">Query parameter</option></select></label>
    <label className="block text-sm">Incoming field name<input className={webhookFieldClass} value={String(config.auth_key_name ?? "")} onChange={(e) => onChange({ ...config, auth_key_name: e.target.value })} placeholder="X-Webhook-Signature" /></label>
    {signature && <details><summary className="cursor-pointer text-sm text-slate-600">Advanced verification headers</summary><label className="mt-3 block text-sm">Signature header<input className={webhookFieldClass} value={String(config.signature_header ?? "")} onChange={(e) => onChange({ ...config, signature_header: e.target.value })} /></label><label className="mt-3 block text-sm">Required verification headers (comma separated)<input className={webhookFieldClass} value={verificationHeadersText(config)} onChange={(e) => onChange({ ...config, verification_headers: e.target.value.split(",").map((name) => name.trim()).filter(Boolean) })} /></label></details>}
  </div>;
}

// Preserve the server's ordered header list while presenting a compact owner-editable field.
function verificationHeadersText(config: WebhookDocument): string {
  // An absent optional list does not become a required header.
  if (!Array.isArray(config.verification_headers)) return "";
  return config.verification_headers.join(", ");
}

// Complex recipes remain intact; primitive controls must not silently downgrade their verification.
function AdvancedVerification({ document }: { document: WebhookDocument }) {
  return <details className="rounded-lg border border-slate-200 p-3 text-sm"><summary className="cursor-pointer font-medium">Advanced signature policy (preserved)</summary><p className="mt-2 text-slate-500">This service uses a structured verification policy. It is retained unchanged while you edit events or routing. Import OpenAPI to change the policy.</p><pre className="mt-3 max-h-64 overflow-auto whitespace-pre-wrap break-all text-xs">{JSON.stringify(document["x-fused-signature-policy"] ?? webhookRecord(document["x-fused-webhook"]).signature_policy, null, 2)}</pre></details>;
}
