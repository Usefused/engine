import { useEffect, useRef, useState } from "react";
import { ArrowUpRight, RefreshCw } from "lucide-react";
import { api, type BillingState } from "~/lib/api";
import { syncBillingAfterReturn } from "~/lib/billing-sync";
import { SettingsDisclosureCard } from "./SettingsDisclosureCard";
import { useToast } from "~/components/Toast";

const secondaryButton = "inline-flex items-center justify-center gap-2 rounded-lg border border-slate-300 bg-white px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50 disabled:opacity-50";
const primaryButton = "inline-flex items-center justify-center gap-2 rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white hover:bg-slate-800 disabled:opacity-50";

// useBilling confirms cancellation before a provider handoff and retains request identity for account-scoped actions.
function useBilling(active: boolean) {
 const toast = useToast();
 const [state, setState] = useState<BillingState | null>(null);
 const [canManage, setCanManage] = useState(false);
 const [busy, setBusy] = useState(false);
 const [message, setMessage] = useState("");
 const [selected, setSelected] = useState("");
 const request = useRef({ intent: "", id: "" });
 // Remote actions require both the actor grant and Registry adapter availability.
 const canChange = canManage && Boolean(state?.available);
 // refresh recovers from webhook lock contention before reading the paid plan and updated Engine access.
 async function refresh() {
  setBusy(true); setMessage("");
  try {
   const access = await api.billing.access();
   setCanManage(access.can_manage);
   const next = await api.billing.state();
   // Start from the provider subscription when present; paid access can lag behind a pending change.
   setState(next); setSelected(next.subscription_plan || next.plan);
   // Reconciliation needs both an operational adapter and the existing management grant.
   if (access.can_manage && next.available) {
    await syncBillingAfterReturn(api.billing.sync);
    const verified = await api.billing.state();
    // Reconciliation resets the choice to the actual subscription rather than a pending or unpaid access tier.
    setState(verified); setSelected(verified.subscription_plan || verified.plan);
   }
  } catch (error) { setMessage(error instanceof Error ? error.message : "Billing is temporarily unavailable."); }
  finally { setBusy(false); }
 }
 useEffect(() => {
  // Opening Billing or returning to this tab verifies current state without affecting other settings drafts.
  if (active) void refresh();
 }, [active]);
 // openBilling confirms cancellation locally before creating a provider session; dismissal has no billing effect.
 async function openBilling(action: "subscribe" | "manage" | "cancel") {
  // UI permission checks are convenience only; Engine enforces the grant again on every request.
  if (!canChange || busy) return;
  setBusy(true); setMessage("");
  const intent = `${action}:${selected}`;
  // Retrying one action retains its identity; changing intent must start a different request.
  if (request.current.intent !== intent) request.current = { intent, id: crypto.randomUUID() };
  try {
   // Keep the busy state while the prompt is open so repeated clicks cannot stack cancellation requests.
   if (action === "cancel") {
    const confirmed = await toast.confirm("Continue to Stripe to cancel your subscription? Access remains until the end of your paid period.");
    // Dismissal must leave the provider and the current subscription untouched.
    if (!confirmed) return;
   }
   // Management and cancellation must never carry an unintended plan update.
   const result = await api.billing.link(action, action === "subscribe" ? selected : undefined, request.current.id);
   const target = new URL(result.url);
   // Protect navigation even if a malformed proxy response reaches this UI.
   if (target.protocol !== "https:" || target.username || target.password) throw Error("Invalid billing link.");
   window.location.assign(target.href);
  } catch (error) { setMessage(error instanceof Error ? error.message : "Billing is temporarily unavailable."); }
  finally { setBusy(false); }
 }
 return { state, canManage, busy, message, selected, setSelected, refresh, openBilling };
}

type BillingControls = ReturnType<typeof useBilling>;

// CurrentSubscription distinguishes paid access from the provider subscription and its pending change.
function CurrentSubscription({ state, canManage, busy, message, refresh, openBilling }: BillingControls) {
 // Ended subscriptions require a new checkout rather than management of a historical record.
 const subscribed = Boolean(state?.status && !["none", "canceled"].includes(state.status));
 const planName = billingPlanName(state);
 return <SettingsDisclosureCard id="subscription-settings" title="Subscription" description="Manage the plan for this Fused account." defaultExpanded>
  <div className="space-y-5">
   <div className="flex flex-wrap items-start justify-between gap-4"><div><p className="text-xs font-medium uppercase tracking-wide text-slate-500">Current access</p><p className="text-lg font-semibold text-slate-900">{planName}</p><p className="mt-1 text-sm text-slate-500">{state?.status.replaceAll("_", " ") || "No subscription"}</p></div><button className={secondaryButton} disabled={busy} onClick={refresh}><RefreshCw size={15}/>Refresh</button></div>
   <SubscriptionPeriod state={state} />
   <PendingSubscription state={state} />
   <p className="text-sm text-slate-500">Review changes before confirming payment or cancellation. Your Engine receives updated access automatically.</p>
   {/* Only the existing billing.manage grant enables mutation controls. */}
   {canManage ? <ManagementActions subscribed={subscribed} cancelling={Boolean(state?.cancel_at_period_end)} busy={busy || !state?.available} openBilling={openBilling} /> : <p className="rounded-lg bg-slate-50 p-3 text-sm text-slate-600">Ask your account owner to change this subscription.</p>}
   <p className="text-sm text-red-700" role="status">{message}</p>
  </div>
 </SettingsDisclosureCard>;
}

// PendingSubscription explains provider-confirmed changes without suggesting they already grant access.
function PendingSubscription({ state }: { state: BillingState | null }) {
 // Ordinary subscriptions need no additional panel; older Registry responses remain compatible.
 if (!state?.pending_change || !state.pending_plan) return null;
 // Resolve display names from Registry's offers rather than duplicating plan configuration.
 const name = state.plans.find(plan => plan.id === state.pending_plan)?.name || state.pending_plan;
 // Only provider-scheduled changes have a known effective date; payment-dependent upgrades do not.
 const message = state.pending_change === "scheduled" && state.change_at
  ? `${name} scheduled for ${new Date(state.change_at).toLocaleDateString()}. Your current paid access continues until then.`
  : `${name} is awaiting payment. Your current paid access remains available; the upgrade activates after successful payment.`;
 const subscriptionName = state.plans.find(plan => plan.id === state.subscription_plan)?.name || state.subscription_plan;
 return <div className="rounded-lg border border-amber-200 bg-amber-50 p-4 text-sm text-amber-950"><p className="font-medium">Pending plan change</p><p className="mt-1">Subscription plan: {subscriptionName}</p><p className="mt-1">{message}</p></div>;
}

// SubscriptionPeriod shows only dates verified by Registry's payment evidence.
function SubscriptionPeriod({ state }: { state: BillingState | null }) {
 if (!state?.access_until) return null; // Missing payment evidence is not an invented renewal date.
 return <p className="text-sm text-slate-600">{state.cancel_at_period_end ? "Cancels" : "Paid through"} {new Date(state.access_until).toLocaleDateString()}.</p>;
}

// ManagementActions keeps cancellation separate from upgrades and relies on the provider's final confirmation.
function ManagementActions({ subscribed, cancelling, busy, openBilling }: { subscribed: boolean; cancelling: boolean; busy: boolean; openBilling: BillingControls["openBilling"] }) {
 if (!subscribed) return null; // Historical subscriptions cannot be changed through a fresh portal.
 return <div className="flex flex-wrap gap-3"><button className={secondaryButton} disabled={busy} onClick={() => { /* Open invoices, payment details, and any scheduled cancellation management. */ void openBilling("manage"); }}>Payment details & invoices <ArrowUpRight size={15}/></button>
  {/* A scheduled cancellation should not be requested twice. */}
  {!cancelling && <button className="rounded-lg px-4 py-2 text-sm font-medium text-red-700 hover:bg-red-50 disabled:opacity-50" disabled={busy} onClick={() => { /* Only the provider confirmation can apply cancellation. */ void openBilling("cancel"); }}>Cancel subscription</button>}
 </div>;
}

// PlanSettings distinguishes current paid access from the plan being explored and reveals support guidance for lower selections.
function PlanSettings({ state, selected, setSelected, busy, canManage, openBilling }: BillingControls) {
 // Only current paid access earns the badge; selecting a card or awaiting an upgrade payment cannot move it.
 const activePlan = state?.access_until && ["active", "past_due"].includes(state.status) && new Date(state.access_until).getTime() > Date.now() ? state.plan : null;
 return <SettingsDisclosureCard id="billing-plan-settings" title="Plans" description="Select a plan to review your subscription options." defaultExpanded>
  <div className="space-y-5"><div className="grid gap-3 sm:grid-cols-2">
   {/* Lower plans remain selectable for support guidance; permission, loading, and availability still control interaction. */}
   {state?.plans.map(plan => <label key={plan.id} className={`flex cursor-pointer has-[:disabled]:cursor-not-allowed gap-3 rounded-xl border p-4 ${selected === plan.id ? "border-slate-900 bg-slate-50" : "border-slate-200"}`}><input type="radio" name="subscription-plan" value={plan.id} checked={selected === plan.id} disabled={busy || !canManage || !state.available} onChange={() => { /* Preserve the selected offer until an explicit refresh. */ setSelected(plan.id); }}/><span className="min-w-0 flex-1"><span className="flex flex-wrap items-center justify-between gap-2"><strong className="text-sm text-slate-900">{plan.name}</strong>
    {/* The badge follows verified access independently of the radio selection. */}
    {activePlan === plan.id && <span className="rounded-full bg-emerald-50 px-2 py-0.5 text-xs font-medium text-emerald-700 ring-1 ring-inset ring-emerald-600/20">Active</span>}
   </span><span className="mt-1 block text-sm text-slate-600">{new Intl.NumberFormat("en-US", { style: "currency", currency: plan.currency }).format(plan.monthly_price_cents / 100)} / month</span></span></label>)}
  </div>
  <p className="text-sm text-slate-600">Upgrades are prorated and activate after successful payment.</p>
  {/* Selecting a lower plan explains the support path without exposing a self-service downgrade action. */}
  {isBillingDowngrade(state, selected) && <div className="rounded-xl bg-slate-50 p-4 text-sm" role="status">
   <p className="font-medium text-slate-900">Contact support to downgrade</p>
   <p className="mt-1 text-slate-600">Our team will review your resources, worker configuration, and renewal date before changing your plan.</p>
   <a className="mt-3 inline-flex items-center gap-1 font-medium text-slate-900 hover:underline underline-offset-4" href="mailto:hello@usefused.com?subject=Fused%20subscription%20downgrade" target="_blank" rel="noreferrer">Contact support <ArrowUpRight size={14} aria-hidden="true"/></a>
  </div>}
  <PlanImpact state={state} selected={selected} />
  <BillingAvailability state={state} />
  {/* Unknown and negotiated plans never enter self-service checkout. */}
  {canManage && state?.plans.some(plan => plan.id === selected) && !isBillingDowngrade(state, selected) && <PlanAction state={state} busy={busy} openBilling={openBilling} />}
  {/* Keep general plan discovery together; downgrade guidance belongs to the selected plan above. */}
  <div className="flex flex-wrap items-center justify-between gap-4 border-t border-slate-200 pt-5">
   <a className={`${secondaryButton} focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-slate-900`} href="https://usefused.com/billing" target="_blank" rel="noreferrer">Compare all plan features <ArrowUpRight size={15} aria-hidden="true"/></a>
   <a className="inline-flex items-center gap-1 text-sm text-slate-600 hover:text-slate-950 hover:underline underline-offset-4" href="https://usefused.com/?plan=enterprise#enterprise-interest" target="_blank" rel="noreferrer">Talk to us about Enterprise <ArrowUpRight size={14} aria-hidden="true"/></a>
  </div></div>
 </SettingsDisclosureCard>;
}

// isBillingDowngrade considers both paid access and provider state so a pending payment cannot reopen a cheaper plan.
function isBillingDowngrade(state: BillingState | null, targetId: string): boolean {
 if (!state) return false; // Loading does not invent an account tier.
 if (state.plan === "enterprise") return true; // Negotiated contracts always require support review.
 const target = state.plans.find(plan => plan.id === targetId);
 const current = state.plans.filter(plan => [state.plan, state.subscription_plan].includes(plan.id));
 return Boolean(target && current.some(plan => plan.monthly_price_cents > target.monthly_price_cents));
}

// PlanImpact shows every commercial limit and capability affected by selection, using Registry's canonical bundles.
function PlanImpact({ state, selected }: { state: BillingState | null; selected: string }) {
 const current = state?.plans.find(plan => plan.id === (state.subscription_plan || state.plan));
 const target = state?.plans.find(plan => plan.id === selected);
 // Older servers and an unchanged selection have no reliable difference to render.
 if (!current?.entitlements || !target?.entitlements || current.id === target.id || isBillingDowngrade(state, selected)) return null;
 const fields: Array<[string, string]> = [
  ["max_api_families", "API apps"], ["max_sdk_families", "SDK apps"], ["max_mcp_families", "MCP apps"],
  ["max_unified_app_families", "Unified Apps"], ["max_buckets", "Buckets"], ["max_services", "Services"],
  ["max_unified_app_concurrency", "Concurrent requests per app worker"], ["max_sandbox_concurrency", "Concurrent provider calls"],
  ["unified_app_always_on_enabled", "Always-on workers"], ["webhook_ingestion_enabled", "Webhook ingestion"],
  ["public_service_insights_enabled", "Public service insights"], ["sso_enabled", "SSO / team management"],
  ["execution_retention_days", "Execution history (days)"],
 ];
 const changes = fields.filter(([key]) => current.entitlements![key] !== target.entitlements![key]);
 return <div className="rounded-xl border border-slate-200 p-4 text-sm"><p className="font-semibold text-slate-900">{target.name}: what changes</p>
  <table className="mt-3 w-full text-left"><thead><tr className="text-slate-500"><th className="pb-2 font-medium">Resource or setting</th><th className="pb-2 font-medium">Current</th><th className="pb-2 font-medium">New</th></tr></thead><tbody>
   {/* Display all changed contract fields, including runtime policy rather than app counts alone. */}
   {changes.map(([key,label]) => <tr key={key} className="border-t border-slate-100"><td className="py-2 pr-2">{label}</td><td className="py-2 pr-2">{planLimitLabel(current.entitlements![key])}</td><td className="py-2">{planLimitLabel(target.entitlements![key])}</td></tr>)}
  </tbody></table>

 </div>;
}

// planLimitLabel retains explicit zero, distinguishes disabled features, and names unlimited capacity.
function planLimitLabel(value: number | boolean | string | undefined): string {
 if (typeof value === "boolean") return value ? "Enabled" : "Disabled"; // Capabilities are not numeric capacity limits.
 if (value === -1) return "Unlimited"; // Preserve the Registry contract's unlimited sentinel.
 return String(value ?? "Unavailable");
}

// BillingSettings integrates the account controls into Engine's existing settings layout without accepting credentials.
export function BillingSettings({ active }: { active: boolean }) {
 const controls = useBilling(active);
 return <div className="max-w-3xl space-y-6"><CurrentSubscription {...controls}/><PlanSettings {...controls}/></div>;
}

// billingPlanName keeps an unloaded account distinct from an unknown or negotiated plan.
function billingPlanName(state: BillingState | null) {
 if (!state) return "Subscription"; // Never label loading state as a free plan.
 return state.plans.find(plan => plan.id === state.plan)?.name || state.plan;
}

// BillingAvailability distinguishes a disabled payment adapter from the account's subscription status.
function BillingAvailability({ state }: { state: BillingState | null }) {
 // Loading and configured adapters need no availability notice.
 if (!state || state.available) return null;
 return <p id="billing-availability" className="rounded-lg bg-slate-50 p-3 text-sm text-slate-600">Payments are not available yet. You can review plans, but subscription changes cannot be submitted.</p>;
}

// PlanAction names the unavailable state instead of inviting the user to continue through a disabled control.
function PlanAction({ state, busy, openBilling }: { state: BillingState; busy: boolean; openBilling: BillingControls["openBilling"] }) {
 // Registry availability controls both the label and whether checkout may be requested.
 const label = state.available ? "Continue with selected plan" : "Payments unavailable";
 return <button className={primaryButton} disabled={busy || !state.available} aria-describedby="billing-availability" onClick={() => { /* The provider still owns the final confirmation before any change. */ void openBilling("subscribe"); }}>{label}<ArrowUpRight size={15}/></button>;
}
