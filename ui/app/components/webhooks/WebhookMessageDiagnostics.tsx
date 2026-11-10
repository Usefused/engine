import { useEffect, useState } from "react";
import { ChevronDown, ChevronUp, LockKeyhole } from "lucide-react";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasResourcePermission, hasWorkspacePermission } from "~/lib/current-actor-access";
import { api, type PrivateWebhookDiagnostics, type WebhookEventEntry } from "~/lib/api";

// messageText pretty-prints valid JSON without interpreting provider content as markup or script.
function messageText(detail: PrivateWebhookDiagnostics): string {
  // Binary evidence stays labelled base64 rather than pretending to be readable UTF-8.
  if (detail.encoding === "base64") return detail.body;
  // Non-JSON and truncated messages retain their original captured text.
  try { return JSON.stringify(JSON.parse(detail.body), null, 2); } catch { return detail.body; }
}

// AuthorizedWebhookMessage fetches on explicit expansion and drops evidence on collapse, scope change or unmount.
function AuthorizedWebhookMessage({ event }: { event: WebhookEventEntry }) {
  const [open, setOpen] = useState(false);
  const [detail, setDetail] = useState<PrivateWebhookDiagnostics | null>(null);
  const [failed, setFailed] = useState(false);
  // Cancellation and the active guard prevent stale responses repopulating a closed inspector.
  useEffect(() => {
    setDetail(null);
    setFailed(false);
    // Merely opening receipt details must never fetch or prefetch a private message.
    if (!open) return;
    const controller = new AbortController();
    let active = true;
    api.workspace.webhookDiagnostics(event.service_id, event.registration_id!, event.id, AbortSignal.any([controller.signal, AbortSignal.timeout(15_000)]))
      // Scope changes and collapse release private bytes even when the response arrived late.
      .then((result) => { if (active) setDetail(result); })
      // Failure displays no raw upstream response or potentially sensitive exception text.
      .catch(() => { if (active) setFailed(true); });
    return () => { active = false; controller.abort(); };
  }, [event.id, event.registration_id, event.service_id, open]);
  // Clearing first prevents an already loaded body remaining mounted after collapse.
  const toggle = () => { setDetail(null); setOpen((previous) => !previous); };
  return <section data-fused-visible="false" className="min-w-0 rounded-lg border border-slate-200 px-3 py-4 sm:px-5">
    <button type="button" onClick={toggle} aria-expanded={open} className="flex w-full items-center gap-2 text-left text-sm font-semibold text-slate-800">
      <LockKeyhole size={15} className="shrink-0 text-slate-400" /> Private webhook message
      {open ? <ChevronUp size={15} className="ml-auto shrink-0" /> : <ChevronDown size={15} className="ml-auto shrink-0" />}
    </button>
    <p className="mt-1 text-xs leading-relaxed text-slate-500">Original request body · restricted access · retained for up to 7 days. Headers and query parameters are excluded.</p>
    {/* The three states expose only an explicit, freshly authorized server response. */}
    {open && !detail && !failed ? <p role="status" className="mt-3 text-xs text-slate-500">Loading message…</p> : null}
    {open && failed ? <p role="status" className="mt-3 rounded-lg bg-slate-50 p-3 text-xs text-slate-600">Message unavailable. It was not captured, has expired, or your access has changed.</p> : null}
    {open && detail ? <div className="mt-4 min-w-0 space-y-3">
      <p className="break-words text-xs text-slate-500">{detail.method} · {detail.content_type || "Content type not recorded"} · {detail.original_bytes.toLocaleString()} B · {detail.encoding}</p>
      {/* Rejected messages are untrusted provider input, not verified business events. */}
      {event.verification_status === "rejected" ? <p className="text-xs text-amber-700">Unverified message: this request failed webhook verification.</p> : null}
      {/* A retained prefix must never be mistaken for the provider's complete message. */}
      {detail.truncated ? <p className="text-xs text-amber-700">Truncated: showing the first {detail.captured_bytes.toLocaleString()} of {detail.original_bytes.toLocaleString()} bytes.</p> : null}
      <pre className="max-h-96 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-slate-50 p-3 text-xs leading-relaxed text-slate-700">{messageText(detail) || "Empty request body"}</pre>
    </div> : null}
  </section>;
}

// WebhookMessageDiagnostics requires the same receipt capabilities plus an independent service diagnostic grant.
export function WebhookMessageDiagnostics({ event }: { event: WebhookEventEntry }) {
  const { access, loading, failed } = useCurrentActorAccess();
  // Unknown registration identity, pending access and ordinary receipt permissions cannot unlock body access.
  if (loading || failed || !event.registration_id || !hasWorkspacePermission(access, "audit.read") || !hasResourcePermission(access, "service.read", "SERVICE", event.service_id) || !hasResourcePermission(access, "service.webhook.diagnostics.read", "SERVICE", event.service_id)) return null;
  return <AuthorizedWebhookMessage key={`${event.service_id}:${event.registration_id}:${event.id}:${access?.authorization_revision}`} event={event} />;
}
