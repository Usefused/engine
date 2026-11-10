import { useEffect, useState } from "react";
import { Link, useParams, useSearchParams } from "@remix-run/react";
import { Webhook } from "lucide-react";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { AppDetailTabs } from "~/components/apps/AppDetailChrome";
import { AppDetailBody, AppDetailSection } from "~/components/apps/AppDetailBody";
import { PageBackLink } from "~/components/layout/PageBackLink";
import { CopyValue } from "~/components/CopyValue";
import { WebhookSigningSecret } from "~/components/webhooks/WebhookSigningSecret";
import { WebhookRegistrationAnalytics, WebhookRegistrationEvents } from "~/components/webhooks/WebhookRegistrationActivity";
import { hasResourcePermission, hasWorkspacePermission } from "~/lib/current-actor-access";
import { copyableWebhookURL, type WebhookListing } from "~/lib/webhook-discovery-contract";
import { webhookDetails } from "~/lib/webhook-detail-api";
import { canReadWebhookActivity, selectedWebhookTab, webhookDetailTabs, type WebhookEventStatus } from "~/lib/webhook-detail";

/** Identifies webhook details independently from the public provider callback endpoint. */
export const meta = () => [{ title: "Webhook details - Fused" }];

/** Gates detail loading before any data query, including while access is pending or unavailable. */
export default function WebhookDetailsPage() {
  const { serviceId = "", slug = "" } = useParams();
  const { access, loading, failed } = useCurrentActorAccess();
  // Loading access is not a denial, and must not start a registration or Activity request.
  if (loading) return <p role="status" className="p-6 text-sm text-slate-500">Checking webhook access…</p>;
  // Access failure must not reuse a previously authorized registration's data.
  if (failed) return <p role="alert" className="p-6 text-sm text-red-700">Your access could not be checked. Refresh to try again.</p>;
  // Exact service grants do not imply visibility of sibling services' registrations.
  if (!hasResourcePermission(access, "service.read", "SERVICE", serviceId)) return <p role="status" className="p-6 text-sm text-slate-600">Service read access is needed to view this webhook.</p>;
  return <WebhookDetailLoader key={`${serviceId}:${slug}`} serviceId={serviceId} slug={slug} />;
}

/** Owns one exact route's cancellable load and explicit retry state without retaining a previous registration. */
function WebhookDetailLoader({ serviceId, slug }: { serviceId: string; slug: string }) {
  const [item, setItem] = useState<WebhookListing | null>(null);
  const [loading, setLoading] = useState(true), [error, setError] = useState("");
  const [refresh, setRefresh] = useState(0);
  useEffect(() => {
    let active = true;
    const controller = new AbortController();
    setLoading(true); setError(""); setItem(null);
    webhookDetails(serviceId, slug, controller.signal).then((result) => {
      // Only the mounted route may own this receiving URL and registration identity.
      if (active) setItem(result);
    }).catch(() => {
      // A transport error must not masquerade as a deleted registration.
      if (active) setError("Webhook details could not be loaded. Try again.");
    }).finally(() => {
      // A cancelled request cannot stop a newer route's loading state.
      if (active) setLoading(false);
    });
    // Cancellation owns the network request as well as subsequent state updates.
    return () => { active = false; controller.abort(); };
  }, [serviceId, slug, refresh]);
  // Loading, unavailable, removed, and readable registrations have separate reviewable states.
  return <div className="min-w-0 max-w-full space-y-6">
    <PageBackLink to="/integrations/webhooks">Back to webhooks</PageBackLink>
    {loading ? <p role="status" className="py-12 text-center text-sm text-slate-500">Loading webhook details…</p> : error ? <div role="alert" className="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700">{error}<button type="button" onClick={() => setRefresh((value) => value + 1)} className="ml-3 underline">Try again</button></div> : item ? <WebhookDetailView key={item.id} item={item} onReload={() => setRefresh((value) => value + 1)} /> : <p role="status" className="py-12 text-center text-sm text-slate-600">Webhook not found. It may have been removed or may belong to another service.</p>}
  </div>;
}

/** Uses the shared app detail navigation with exact-registration overview, analytics, and received-event views. */
function WebhookDetailView({ item, onReload }: { item: WebhookListing; onReload: () => void }) {
  const { access } = useCurrentActorAccess();
  const [params, setParams] = useSearchParams();
  const [start, setStart] = useState(""), [end, setEnd] = useState("");
  const canReadActivity = canReadWebhookActivity(access, item.service_id);
  const active = selectedWebhookTab(params.get("tab"), canReadActivity);
  const url = copyableWebhookURL(item);
  // Only bounded outcome selections may be restored from an analytics drill-down link.
  const status: WebhookEventStatus = ["failed", "rejected", "success"].includes(params.get("status") || "") ? params.get("status") as WebhookEventStatus : "";
  /** Keeps the chosen period mounted while moving from an aggregate into matching receipts. */
  function openEvents(nextStatus: WebhookEventStatus) {
    setParams({ tab: "events", status: nextStatus });
  }
  return <>
    <header className="flex min-w-0 flex-col justify-between gap-4 md:flex-row md:items-start">
      <div className="min-w-0"><h1 className="break-words text-2xl font-bold text-slate-900">{item.label}</h1><p className="mt-1 text-slate-500">Receive service events and inspect deliveries for this webhook registration.</p>
        <div className="mt-3 flex flex-wrap items-center gap-3 text-sm"><span className="inline-flex items-center gap-1.5 text-[var(--brand-violet)]"><Webhook className="h-4 w-4" />Webhook</span><Link to={`/integrations/${item.service_id}?tab=webhooks`} className="break-all text-slate-600 hover:underline">{item.service_ref || item.service_name}</Link>{/* Older registrations may lack a creation timestamp. */}{item.created_at && <span className="text-slate-500">Created {new Date(item.created_at).toLocaleDateString()}</span>}</div>
      </div>
    </header>
    <AppDetailTabs label="Webhook details" active={active} tabs={webhookDetailTabs(canReadActivity)} onChange={(tab) => setParams({ tab })} />
    <AppDetailBody>
      {/* Only an authorized Activity tab mounts a reporting loader; direct URL selection cannot bypass this boundary. */}
      {active === "overview" && <div className="space-y-7">
        <AppDetailSection title="Receiving URL">
          {/* Managed subscriptions never advertise a provider callback, even if old metadata retained one. */}
          {url ? <><CopyValue value={url} label="webhook URL" /><p className="mt-2 text-xs text-slate-500">Paste this URL into your provider’s webhook settings.</p></> : item.delivery_mode === "managed" ? <p className="text-sm text-slate-500">Events arrive through your managed connection. No provider URL is needed.</p> : <div className="space-y-2 text-sm text-slate-500"><p>Configure the Engine public URL to display a provider-ready address.</p><CopyValue value={`/webhook/${item.slug}`} label="webhook receiving path" /></div>}
        </AppDetailSection>
        <AppDetailSection title="Verification"><p className="mb-3 text-sm text-slate-600">{item.signature === "set" ? "Signing secret configured" : "No signing secret configured"}</p>
          <div className="flex flex-wrap items-center gap-4">{/* Credential locations remain separately permission-gated by Engine. */}{item.signing_secret && <Link to={`/integrations/buckets?${new URLSearchParams({ bucket: item.signing_secret.bucket_id, tab: "secrets", secret: item.signing_secret.key_name })}`} className="text-sm font-medium text-[var(--brand-violet)] hover:underline">Manage secret</Link>}{hasWorkspacePermission(access, "app.webhook.manage") && item.delivery_mode === "direct" && <WebhookSigningSecret slug={item.slug} onSaved={onReload} />}</div>
        </AppDetailSection>
      </div>}
      {active === "analytics" && <WebhookRegistrationAnalytics serviceId={item.service_id} registrationId={item.id} start={start} end={end} onStart={setStart} onEnd={setEnd} onEvents={openEvents} />}
      {active === "events" && <WebhookRegistrationEvents key={status} serviceId={item.service_id} registrationId={item.id} initialStatus={status} start={start} end={end} onStart={setStart} onEnd={setEnd} />}
    </AppDetailBody>
  </>;
}
