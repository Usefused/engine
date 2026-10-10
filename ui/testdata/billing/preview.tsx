import { createRoot } from "react-dom/client";
import { Settings } from "lucide-react";
import { BillingSettings } from "../../app/components/settings/BillingSettings";
import { CataloguePageHeader } from "../../app/components/layout/CataloguePageHeader";
import { api } from "../../app/lib/api";
import { ToastProvider } from "../../app/components/Toast";

// Synthetic transport keeps visual review independent of live accounts, credentials, and provider payments.
api.billing.access = async () => ({ can_manage: new URLSearchParams(location.search).get("role") !== "viewer" });
// The fixture mirrors the normalized Registry contract without inventing a browser-side account identity.
api.billing.state = async () => ({ available: true, plan: "dev", status: "active", access_until: "2026-11-09T12:00:00Z", cancel_at_period_end: false, plans: [{id:"dev",name:"Developer",monthly_price_cents:2000,currency:"usd"},{id:"scale-up",name:"Scale-up",monthly_price_cents:19900,currency:"usd"}] });
// Refresh is read-only in this fixture and never calls a payment provider.
api.billing.sync = async () => ({ status: "synchronized" });
// Preview actions stop before navigation or payment while keeping the chosen intent visible for manual checking.
api.billing.link = async (action, plan) => { throw Error(`Preview only: ${action} ${plan || ""}`.trim()); };
// Preview renders the real billing component with the existing settings header and tab styles.
function Preview() {
 return <main className="mx-auto max-w-5xl space-y-6 p-6"><p className="text-xs text-slate-500">Local preview · synthetic account · no payments</p><CataloguePageHeader title="Settings" icon={Settings} description="Manage your account, connections, and API credentials."/><nav className="flex gap-2 border-b border-slate-200 pb-3"><span className="rounded-lg px-4 py-2 text-sm font-medium text-slate-600">General</span><span className="rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white">Billing</span><span className="rounded-lg px-4 py-2 text-sm font-medium text-slate-600">Connection branding</span></nav><BillingSettings active/></main>;
}
createRoot(document.getElementById("root")!).render(<ToastProvider><Preview/></ToastProvider>);
