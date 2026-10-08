import { PageBackLink } from "~/components/layout/PageBackLink";
import { useEffect, useState, type FormEvent } from "react";
import { Link, useNavigate, useSearchParams } from "@remix-run/react";
import { ArrowRight, Check, Loader2 } from "lucide-react";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";
import { SecretReferenceField } from "~/components/buckets/SecretReferenceField";
import { FieldLabel } from "~/components/forms/FieldLabel";
import { Select } from "~/components/forms/Select";
import { createWebhook, planWebhook, webhookServices } from "~/lib/webhook-discovery-api";
import { listAppOwningTeams } from "~/lib/app-builder";
import { readAllBoundedPages } from "~/lib/bounded-pages";
import type { ActivatedService } from "~/lib/api";
import type { AppOwningTeam, AppPlanResponse } from "~/lib/app-builder-contract";

// Controls use explicit regular-weight values instead of inheriting the enclosing label typography.
const field = "w-full rounded-lg border border-slate-300 bg-white px-3 py-2.5 text-sm font-normal text-slate-900 placeholder:text-slate-400 disabled:bg-slate-50";
const button = "inline-flex items-center justify-center gap-2 rounded-lg bg-[var(--brand-violet)] px-4 py-2.5 text-sm font-semibold text-white disabled:opacity-50";

/** Gives webhook pages a clear browser identity distinct from service detail routes. */
export const meta = () => [{ title: "Create webhook - Fused" }];

/** Creates a named receiving URL through the same review/apply protocol as the CLI. */
export default function CreateWebhookPage() {
  const navigate = useNavigate(), [params] = useSearchParams();
  const { access, loading: accessLoading } = useCurrentActorAccess();
  const allowed = hasWorkspacePermission(access, "app.webhook.create");
  const [services, setServices] = useState<ActivatedService[]>([]), [teams, setTeams] = useState<AppOwningTeam[]>([]);
  const [name, setName] = useState(""), [serviceID, setServiceID] = useState(params.get("service") || "");
  const [baseURL, setBaseURL] = useState(""), [secret, setSecret] = useState(""), [teamID, setTeamID] = useState("");
  const [plan, setPlan] = useState<AppPlanResponse | null>(null), [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(true), [error, setError] = useState(""), [ownerError, setOwnerError] = useState("");
  const [uncertain, setUncertain] = useState(false);
  useEffect(() => {
    let active = true;
    // Creation dependencies are fetched only after permission has been established.
    if (accessLoading || !allowed) { setLoading(false); return; }
    setLoading(true);
    // Only an operator-configured public address may prefill provider routing.
    setBaseURL((current) => current || window.__FUSED_ENV?.ENGINE_PUBLIC_URL || "");
    /** Keep discovery mandatory while an unavailable team list leaves personal ownership explicit. */
    async function load() {
      const results = await Promise.allSettled([webhookServices(), readAllBoundedPages((limit, offset) => listAppOwningTeams("", limit, offset), 100, 100)]);
      // A request cannot update a form that has been left behind.
      if (!active) return;
      // A failed catalogue cannot be treated as an empty successful response.
      if (results[0].status === "fulfilled") setServices(results[0].value); else setError(String(results[0].reason));
      // Optional team lookup failure remains visible without silently choosing another team.
      if (results[1].status === "fulfilled") setTeams(results[1].value); else setOwnerError("Teams could not be loaded. Refresh to choose a team, or explicitly use personal ownership.");
      setLoading(false);
    }
    void load();
    return () => { active = false; };
  }, [accessLoading, allowed]);
  const service = services.find((item) => item.service_id === serviceID);
  /** Every edit invalidates the prior review before another apply is offered. */
  function change(update: () => void) { update(); setPlan(null); setError(""); }
  /** A review records intent without provisioning or contacting the provider. */
  async function review(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    // Only an available service may enter Engine's authoritative permission and configuration validation.
    if (!service || busy || uncertain) return;
    setBusy(true); setError("");
    try { setPlan(await planWebhook({ name, service: service.service_slug || service.service_name, baseURL, secret }, teams.find((team) => team.id === teamID)?.slug || "")); }
    catch (cause) { setError(String(cause)); }
    finally { setBusy(false); }
  }
  /** An ambiguous apply outcome directs the user to inspect registrations before another attempt. */
  async function provision() {
    // The saved plan and hash are the only authority for the creation action.
    if (!plan || busy || uncertain) return;
    setBusy(true); setError("");
    try {
      await createWebhook(plan);
      navigate(`/integrations/webhooks?service=${encodeURIComponent(serviceID)}&created=${encodeURIComponent(name.trim())}`);
    } catch (cause) { setError(`${String(cause)} Check the webhook list before trying again.`); setUncertain(true); }
    finally { setBusy(false); }
  }
  // Permission checks precede the form rather than presenting a mutation that cannot be authorized.
  if (accessLoading) return <p role="status">Checking webhook access…</p>;
  // Reading URLs does not imply permission to provision another registration.
  if (!allowed) return <section className="space-y-3"><PageBackLink to="/integrations/webhooks">Back to webhooks</PageBackLink><p>You need webhook creation access to provision a receiving URL.</p></section>;
  return <div className="mx-auto max-w-2xl space-y-6">
    <PageBackLink to="/integrations/webhooks">Back to webhooks</PageBackLink>
    <header><h1 className="text-2xl font-bold text-slate-900">Create webhook</h1><p className="mt-2 text-sm text-slate-500">Create a receiving URL for events from a service.</p></header>
    {error && <p role="alert" className="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700">{error}</p>}
    {ownerError && <p role="status" className="text-sm text-amber-800">{ownerError}</p>}
    <form onSubmit={review} className="rounded-xl border border-slate-200 bg-white p-5 sm:p-6"><fieldset disabled={busy || loading || uncertain} className="space-y-5">
      <label className="block space-y-2 text-sm font-medium text-slate-600"><FieldLabel required>Name</FieldLabel><input required autoComplete="off" placeholder="stripe-events" className={field} value={name} onChange={(event) => change(() => setName(event.target.value))} /></label>
      {/* An empty required selection is a prompt; chosen services use the same dark value styling as text inputs. */}
      <label className="block space-y-2 text-sm font-medium text-slate-600"><FieldLabel required>Service</FieldLabel><Select required className={`${field} invalid:text-slate-400 [&_option]:text-slate-900`} value={serviceID} onChange={(event) => change(() => setServiceID(event.target.value))}><option value="">Choose a service</option>{services.map((item) => <option key={item.service_id} value={item.service_id}>{item.service_name}</option>)}</Select></label>
      {!loading && services.length === 0 && <p className="text-sm text-slate-500">Add a service to your workspace first. <Link to="/integrations" className="text-[var(--brand-violet)]">Browse services</Link></p>}
      <label className="block space-y-2 text-sm font-medium text-slate-600"><FieldLabel required>Public Fused URL</FieldLabel><input required type="url" placeholder="https://fused.example.com" className={field} value={baseURL} onChange={(event) => change(() => setBaseURL(event.target.value))} /><span className="block text-xs font-normal text-slate-500">The public address where your provider can reach Fused.</span></label>
      <SecretReferenceField value={secret} disabled={busy || loading || uncertain} onChange={(reference) => change(() => setSecret(reference))} />
      <p className="text-xs text-slate-500">Leave the reference blank only if your provider does not require verification.</p>
      <label className="block space-y-2 text-sm font-medium text-slate-600"><FieldLabel>Owner</FieldLabel><Select className={field} value={teamID} onChange={(event) => change(() => setTeamID(event.target.value))}><option value="">Personal (you)</option>{teams.map((team) => <option key={team.id} value={team.id}>{team.name}</option>)}</Select></label>
      {/* A reviewed receipt is invalidated by every editable field above. */}
      {plan ? <section className="space-y-4 rounded-lg border border-emerald-200 bg-emerald-50 p-4"><h2 className="flex items-center gap-2 font-semibold text-emerald-900"><Check className="h-4 w-4" />Ready to create</h2><p className="text-sm text-emerald-800">Create “{name.trim()}” for {service?.service_name}. Copy the receiving URL into your provider’s settings after creation.</p><button type="button" onClick={provision} className={button}>{busy ? <Loader2 className="h-4 w-4 animate-spin" /> : <Check className="h-4 w-4" />}Create webhook</button></section> : <button type="submit" disabled={!service} className={button}>{busy || loading ? <Loader2 className="h-4 w-4 animate-spin" /> : <ArrowRight className="h-4 w-4" />}Review webhook</button>}
    </fieldset></form>
    {uncertain && <Link to="/integrations/webhooks" className="inline-flex text-sm font-semibold text-[var(--brand-violet)]">Check registered webhooks</Link>}
  </div>;
}
