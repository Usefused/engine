import { useState, useEffect } from "react";
import { useNavigate, type MetaFunction } from "@remix-run/react";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m: any) => m.id === "root").flatMap((m: any) => m.meta ?? []);
  return [
    ...parentMeta.filter((m: any) => !('title' in m)),
    { title: "Credits - Fused" },
  ];
};
import { api, Account, CreditBundle, CreditsPricing } from "~/lib/api";
import { useToast } from "~/components/Toast";
import { Wallet, Zap, AlertCircle, Check, ChevronLeft, CreditCard } from "lucide-react";

export default function CreditsPage() {
  const toast = useToast();
  const navigate = useNavigate();
  const [account, setAccount] = useState<Account | null>(null);
  const [pricing, setPricing] = useState<CreditsPricing | null>(null);
  const [selectedBundle, setSelectedBundle] = useState<string | null>(null);

  const [autoTopupEnabled, setAutoTopupEnabled] = useState(false);
  const [autoTopupThreshold, setAutoTopupThreshold] = useState<number>(10);
  const [autoTopupBundle, setAutoTopupBundle] = useState<string>("");
  const [savingAutoTopup, setSavingAutoTopup] = useState(false);
  const [autoTopupSaved, setAutoTopupSaved] = useState(false);

  const [loading, setLoading] = useState(true);

  useEffect(() => {
    Promise.all([
      api.getAccount(),
      api.credits.getPricing(),
    ])
      .then(([acc, pr]) => {
        setAccount(acc);
        setPricing(pr);
        setAutoTopupEnabled(acc.auto_topup_enabled ?? false);
        setAutoTopupThreshold(acc.auto_topup_threshold ?? 10);
        setAutoTopupBundle(acc.auto_topup_bundle_id ?? "");
        setLoading(false);
      })
      .catch((err) => {
        console.error(err);
        setLoading(false);
      });
  }, []);

  async function handleSaveAutoTopup() {
    setSavingAutoTopup(true);
    setAutoTopupSaved(false);
    try {
      await api.updateAutoTopup(autoTopupEnabled, autoTopupThreshold, autoTopupBundle);
      setAutoTopupSaved(true);
      setTimeout(() => setAutoTopupSaved(false), 3000);
    } catch (err) {
      console.error("Failed to update auto top-up", err);
      toast.error("Failed to update auto top-up settings.");
    } finally {
      setSavingAutoTopup(false);
    }
  }

  function handlePurchase() {
    if (!selectedBundle) {
      toast.warning("Please select a credit bundle.");
      return;
    }
    // Payment integration placeholder
    toast.info(`Purchase flow for bundle "${selectedBundle}" coming soon!`);
  }

  const actionLabels: Record<string, string> = {
    sdk_generation: "SDK Generation",
    mcp_generation: "MCP Generation",
    mcp_sandbox_request: "MCP Sandbox Request",
    openapi_generation: "OpenAPI Generation",
    llm_per_1k_tokens: "LLM (per 1k tokens)",
    add_endpoint: "Add Endpoint",
    drift_monitoring: "Drift Monitoring",
    webhook_ingestion_charge: "Webhook Ingestion",
    initial_credit_balance: "Initial Credit Balance",
  };

  if (loading) {
    return (
      <div className="max-w-3xl mx-auto space-y-8">
        <div className="animate-pulse space-y-4">
          <div className="h-8 bg-slate-100 rounded w-1/3"></div>
          <div className="h-32 bg-slate-100 rounded"></div>
          <div className="h-64 bg-slate-100 rounded"></div>
        </div>
      </div>
    );
  }

  return (
    <div className="max-w-3xl mx-auto space-y-8">
      <button
        data-track="navigate_back_to_settings"
        onClick={() => navigate("/integrations/settings")}
        className="flex items-center gap-1 text-sm text-slate-500 hover:text-slate-800 transition-colors"
      >
        <ChevronLeft className="w-4 h-4" />
        Back to Settings
      </button>

      <div>
        <h1 className="text-2xl font-bold text-slate-900 flex items-center gap-2">
          <Wallet className="w-6 h-6 text-blue-600" />
          Add Credits
        </h1>
        <p className="text-slate-500 mt-1">Top up your credit balance and manage auto-renewal.</p>
      </div>

      {/* Balance Card */}
      <div className="bg-white rounded-xl shadow-sm border border-slate-200 overflow-hidden">
        <div className="p-6">
          <h2 className="text-lg font-semibold text-slate-900">Current Balance</h2>
          <div className="mt-4 flex items-center gap-3">
            <div className="w-12 h-12 rounded-full bg-blue-50 flex items-center justify-center">
              <CreditCard className="w-6 h-6 text-blue-600" />
            </div>
            <div>
              <p className="text-3xl font-bold text-slate-900">
                ${(account?.credit_balance ?? 0).toFixed(4)}
              </p>
              <p className="text-sm text-slate-500">Available credits</p>
            </div>
          </div>
        </div>
      </div>

      {/* Bundles */}
      <div className="bg-white rounded-xl shadow-sm border border-slate-200 overflow-hidden">
        <div className="p-6">
          <h2 className="text-lg font-semibold text-slate-900">Select a Bundle</h2>
          <p className="text-sm text-slate-500 mb-6">Choose the credit bundle that fits your needs.</p>

          <div className="grid grid-cols-1 sm:grid-cols-3 gap-4">
            {pricing?.bundles?.map((bundle: any) => {
              const bundleId = bundle.id || bundle.ID;
              const bundleName = bundle.name || bundle.Name;
              const bundlePrice = bundle.price_usd ?? bundle.PriceUSD ?? 0;
              const bundleCredits = bundle.credits ?? bundle.Credits ?? 0;
              
              return (
              <button
                data-track="select_credit_bundle"
                key={bundleId}
                onClick={() => setSelectedBundle(bundleId)}
                className={`relative text-left p-5 rounded-xl border-2 transition-all duration-200 ${
                  selectedBundle === bundleId
                    ? "border-blue-600 bg-blue-50/40 shadow-sm"
                    : "border-slate-200 hover:border-slate-300 hover:bg-slate-50/50"
                }`}
              >
                {selectedBundle === bundleId && (
                  <div className="absolute top-3 right-3 w-5 h-5 rounded-full bg-blue-600 flex items-center justify-center">
                    <Check className="w-3 h-3 text-white" />
                  </div>
                )}
                <p className="text-sm font-medium text-slate-500">{bundleName}</p>
                <p className="text-2xl font-bold text-slate-900 mt-1">${bundlePrice}</p>
                <p className="text-sm text-slate-600 mt-1">
                  {bundleCredits.toLocaleString()} credits
                </p>
                {bundleCredits > 0 && (
                  <p className="text-xs text-slate-400 mt-1">
                    ${(bundlePrice / bundleCredits).toFixed(4)} per credit
                  </p>
                )}
              </button>
            )})}
          </div>

          <div className="mt-6">
            <button
              data-track="purchase_credit_bundle"
              onClick={handlePurchase}
              disabled={!selectedBundle}
              className="px-6 py-2.5 bg-blue-600 text-white text-sm font-medium rounded-lg hover:bg-blue-700 focus:outline-none focus:ring-2 focus:ring-offset-2 focus:ring-blue-500 disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
            >
              Purchase Selected Bundle
            </button>
          </div>
        </div>
      </div>

      {/* Auto Top-up */}
      <div className="bg-white rounded-xl shadow-sm border border-slate-200 overflow-hidden">
        <div className="p-6">
          <div className="flex items-center gap-2">
            <Zap className="w-5 h-5 text-amber-500" />
            <h2 className="text-lg font-semibold text-slate-900">Auto Top-up</h2>
          </div>
          <p className="text-sm text-slate-500 mb-6">
            Automatically renew your credits when your balance falls below a threshold.
          </p>

          <div className="space-y-5">
            <label className="flex items-center gap-3 cursor-pointer">
              <div className="relative">
                <input
                  type="checkbox"
                  className="sr-only peer"
                  checked={autoTopupEnabled}
                  onChange={(e) => setAutoTopupEnabled(e.target.checked)}
                />
                <div className="w-11 h-6 bg-slate-200 peer-focus:outline-none peer-focus:ring-4 peer-focus:ring-blue-100 rounded-full peer peer-checked:after:translate-x-full peer-checked:after:border-white after:content-[''] after:absolute after:top-[2px] after:left-[2px] after:bg-white after:border-gray-300 after:border after:rounded-full after:h-5 after:w-5 after:transition-all peer-checked:bg-blue-600"></div>
              </div>
              <span className="text-sm font-medium text-slate-700">
                Enable auto top-up
              </span>
            </label>

            {autoTopupEnabled && (
              <div className="space-y-4 pl-1">
                <div>
                  <label className="block text-sm font-medium text-slate-700 mb-1.5">
                    Low Balance Threshold
                  </label>
                  <div className="flex items-center gap-3">
                    <input
                      type="number"
                      min={0}
                      step={0.01}
                      value={autoTopupThreshold}
                      onChange={(e) => setAutoTopupThreshold(parseFloat(e.target.value) || 0)}
                      className="w-40 px-3 py-2 bg-white border border-slate-300 rounded-lg focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-transparent text-sm"
                    />
                    <span className="text-sm text-slate-500">credits</span>
                  </div>
                  <p className="text-xs text-slate-400 mt-1">
                    We will auto-charge you when your balance drops below this amount.
                  </p>
                </div>

                <div>
                  <label className="block text-sm font-medium text-slate-700 mb-1.5">
                    Auto Top-up Bundle
                  </label>
                  <select
                    value={autoTopupBundle}
                    onChange={(e) => setAutoTopupBundle(e.target.value)}
                    className="w-full sm:w-64 px-3 py-2 bg-white border border-slate-300 rounded-lg focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-transparent text-sm"
                  >
                    <option value="">Select a bundle</option>
                    {pricing?.bundles.map((bundle) => (
                      <option key={bundle.id} value={bundle.id}>
                        {bundle.name} — ${bundle.price_usd} for {bundle.credits.toLocaleString()} credits
                      </option>
                    ))}
                  </select>
                </div>

                <div className="flex items-start gap-2 p-3 bg-amber-50 border border-amber-100 rounded-lg">
                  <AlertCircle className="w-4 h-4 text-amber-600 shrink-0 mt-0.5" />
                  <p className="text-xs text-amber-700">
                    Your saved payment method will be charged automatically when your balance falls below the threshold. You can disable this at any time.
                  </p>
                </div>
              </div>
            )}

            <div className="flex items-center gap-4 pt-2">
              <button
                data-track="save_auto_topup_settings"
                onClick={handleSaveAutoTopup}
                disabled={savingAutoTopup}
                className="px-4 py-2 bg-blue-600 text-white text-sm font-medium rounded-lg hover:bg-blue-700 focus:outline-none focus:ring-2 focus:ring-offset-2 focus:ring-blue-500 disabled:opacity-50 transition-colors"
              >
                {savingAutoTopup ? "Saving..." : "Save Auto Top-up Settings"}
              </button>
              {autoTopupSaved && (
                <span className="text-sm text-green-600">Saved successfully!</span>
              )}
            </div>
          </div>
        </div>
      </div>

      {/* Pricing Reference */}
      <div className="bg-white rounded-xl shadow-sm border border-slate-200 overflow-hidden">
        <div className="p-6">
          <h2 className="text-lg font-semibold text-slate-900">Credit Costs</h2>
          <p className="text-sm text-slate-500 mb-4">How much each action costs in credits.</p>

          <div className="overflow-hidden rounded-lg border border-slate-200">
            <table className="w-full text-sm">
              <thead className="bg-slate-50">
                <tr>
                  <th className="text-left px-4 py-2.5 font-medium text-slate-700">Action</th>
                  <th className="text-right px-4 py-2.5 font-medium text-slate-700">Cost (credits)</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-100">
                {pricing &&
                  Object.entries(pricing.actions).map(([key, cost]) => (
                    <tr key={key} className="hover:bg-slate-50/50">
                      <td className="px-4 py-2.5 text-slate-700">
                        {actionLabels[key] ?? key}
                      </td>
                      <td className="px-4 py-2.5 text-right font-medium text-slate-900">
                        {cost.toFixed(4)}
                      </td>
                    </tr>
                  ))}
              </tbody>
            </table>
          </div>
        </div>
      </div>
    </div>
  );
}
