import { useState, useEffect } from "react";
import { type MetaFunction } from "@remix-run/react";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches
    .filter((m) => m.id === "root")
    .flatMap((m) => m.meta ?? []);
  return [
    ...parentMeta.filter((m) => !("title" in m)),
    { title: "Settings - Fused" },
  ];
};
import { api, Account } from "~/lib/api";
import { useToast } from "~/components/Toast";
import { ConnectBrandingCard } from "~/components/settings/ConnectBrandingCard";
import { SettingsDisclosureCard } from "~/components/settings/SettingsDisclosureCard";

// readEngineEndpoints resolves the operator's public Engine addresses from the browser runtime.
function readEngineEndpoints() {
  const runtime = window.__FUSED_ENV || {};
  return {
    http: runtime.ENGINE_PUBLIC_URL || window.location.origin,
    grpc: runtime.ENGINE_PUBLIC_GRPC_URL || "",
  };
}

// displayEndpoint returns the configured address or its existing empty-state label.
function displayEndpoint(value: string, emptyLabel: string) {
  return value || emptyLabel;
}

// SettingsPage manages account details, Engine addresses, and API-key rotation.
export default function SettingsPage() {
  const toast = useToast();
  const [account, setAccount] = useState<Account | null>(null);
  const [email, setEmail] = useState("");
  const [savingEmail, setSavingEmail] = useState(false);
  const [emailSuccess, setEmailSuccess] = useState(false);

  const [regenerating, setRegenerating] = useState(false);
  const [newKey, setNewKey] = useState<string | null>(null);
  const [showRegenConfirm, setShowRegenConfirm] = useState(false);
  const [engineEndpoints, setEngineEndpoints] = useState({
    http: "",
    grpc: "",
  });

  useEffect(() => {
    api
      .getAccount()
      .then((acc) => {
        setAccount(acc);
        if (acc.email) setEmail(acc.email);
      })
      .catch(console.error);
  }, []);

  useEffect(() => {
    // Browser runtime configuration is unavailable during server rendering.
    setEngineEndpoints(readEngineEndpoints());
  }, []);

  function copyEndpoint(value: string, label: string) {
    navigator.clipboard.writeText(value);
    toast.success(`${label} copied to clipboard!`);
  }

  async function handleSaveEmail(e: React.FormEvent) {
    e.preventDefault();
    if (!account) return;
    setSavingEmail(true);
    setEmailSuccess(false);
    try {
      await api.updateEmail(email);
      setEmailSuccess(true);
      setTimeout(() => setEmailSuccess(false), 3000);
    } catch (err) {
      console.error("Failed to update email", err);
      toast.error("Failed to update email.");
    } finally {
      setSavingEmail(false);
    }
  }

  async function handleRegenerateKey() {
    setRegenerating(true);
    try {
      const res = await api.regenerateApiKey();
      setNewKey(res.api_key);
      setShowRegenConfirm(false);
    } catch (err) {
      console.error("Failed to regenerate API key", err);
      toast.error("Failed to regenerate API key.");
    } finally {
      setRegenerating(false);
    }
  }

  return (
    <div className="max-w-2xl mx-auto space-y-8">
      <div>
        <h1 className="text-2xl font-bold text-slate-900">Account Settings</h1>
        <p className="text-slate-500 mt-1">
          Manage your account details and API credentials.
        </p>
      </div>

      <div className="bg-white rounded-xl shadow-sm border border-slate-200 overflow-hidden">
        <div className="p-6">
          <h2 className="text-lg font-semibold text-slate-900">
            Engine Endpoints
          </h2>
          <p className="text-sm text-slate-500 mb-6">
            Use the Engine URL for the dashboard and HTTP API. Generated SDKs
            connect to the separate gRPC URL.
          </p>

          <div className="space-y-5">
            <div>
              <label className="block text-sm font-medium text-slate-700 mb-1">
                Engine Admin URL
              </label>
              <div className="flex items-center gap-2">
                <code className="flex-1 block px-3 py-2 bg-slate-50 border border-slate-200 rounded-md text-sm text-slate-800 break-all">
                  {displayEndpoint(engineEndpoints.http, "Loading...")}
                </code>
                <button
                  type="button"
                  data-track="copy_engine_url"
                  disabled={!engineEndpoints.http}
                  onClick={() =>
                    copyEndpoint(engineEndpoints.http, "Engine URL")
                  }
                  className="px-3 py-2 bg-white border border-slate-300 text-slate-700 text-sm font-medium rounded-md hover:bg-slate-50 disabled:opacity-50 transition-colors"
                >
                  Copy
                </button>
              </div>
            </div>

            <div>
              <label className="block text-sm font-medium text-slate-700 mb-1">
                Engine gRPC URL
              </label>
              <div className="flex items-center gap-2">
                <code className="flex-1 block px-3 py-2 bg-slate-50 border border-slate-200 rounded-md text-sm text-slate-800 break-all">
                  {displayEndpoint(
                    engineEndpoints.grpc,
                    "Not configured by this Engine operator",
                  )}
                </code>
                <button
                  type="button"
                  data-track="copy_engine_grpc_url"
                  disabled={!engineEndpoints.grpc}
                  onClick={() => copyEndpoint(engineEndpoints.grpc, "gRPC URL")}
                  className="px-3 py-2 bg-white border border-slate-300 text-slate-700 text-sm font-medium rounded-md hover:bg-slate-50 disabled:opacity-50 transition-colors"
                >
                  Copy
                </button>
              </div>
              <p className="mt-2 text-xs text-slate-500">
                Set <code className="font-mono">FUSED_ENGINE_GRPC_URL</code> to
                this value in applications using a generated SDK.
              </p>
            </div>
          </div>
        </div>
      </div>

      <ConnectBrandingCard />

      <SettingsDisclosureCard
        id="account-details-settings"
        title="Account Details"
        description="Update your personal information."
      >
        {account ? (
            <form
              onSubmit={handleSaveEmail}
              className="space-y-4"
              toolname="save_account_email"
              tooldescription="Save the account email address."
            >
              <div>
                <label className="block text-sm font-medium text-slate-700 mb-1">
                  Account Name
                </label>
                <input
                  type="text"
                  value={account.name}
                  disabled
                  className="w-full px-3 py-2 bg-slate-50 border border-slate-200 rounded-lg text-slate-500 cursor-not-allowed"
                />
              </div>

              <div>
                <label className="block text-sm font-medium text-slate-700 mb-1">
                  Email Address
                </label>
                <input
                  type="email"
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  placeholder="you@example.com"
                  toolparamdescription="The user's new account email address"
                  className="w-full px-3 py-2 bg-white border border-slate-300 rounded-lg focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-transparent transition-shadow"
                />
              </div>

              <div className="flex items-center gap-4 pt-2">
                <button
                  data-track="save_email_settings"
                  type="submit"
                  disabled={savingEmail}
                  className="px-4 py-2 bg-blue-600 text-white text-sm font-medium rounded-lg hover:bg-blue-700 focus:outline-none focus:ring-2 focus:ring-offset-2 focus:ring-blue-500 disabled:opacity-50 transition-colors"
                >
                  {savingEmail ? "Saving..." : "Save Changes"}
                </button>
                {emailSuccess && (
                  <span className="text-sm text-green-600">
                    Saved successfully!
                  </span>
                )}
              </div>
            </form>
        ) : (
          <div className="animate-pulse flex flex-col gap-4">
            <div className="h-10 bg-slate-100 rounded w-full"></div>
            <div className="h-10 bg-slate-100 rounded w-full"></div>
          </div>
        )}
      </SettingsDisclosureCard>

      <SettingsDisclosureCard
        id="api-key-management-settings"
        title="API Key Management"
        description={
          <>
            If your API key is compromised or lost, you can regenerate it here.
            <strong> This will immediately invalidate your old key.</strong>
          </>
        }
      >
        {!showRegenConfirm && !newKey && (
            <button
              data-track="show_regenerate_api_key_confirm"
              onClick={() => setShowRegenConfirm(true)}
              className="px-4 py-2 bg-white border border-slate-300 text-slate-700 text-sm font-medium rounded-lg hover:bg-slate-50 focus:outline-none focus:ring-2 focus:ring-offset-2 focus:ring-slate-500 transition-colors"
            >
              Regenerate API Key
            </button>
        )}

        {showRegenConfirm && (
          <div className="bg-orange-50 border border-orange-200 rounded-lg p-4">
              <h3 className="text-sm font-medium text-orange-800">
                Are you sure?
              </h3>
              <p className="mt-1 text-sm text-orange-700">
                Any existing applications using your current API key will stop
                working immediately. This action cannot be undone.
              </p>
              <div className="mt-4 flex gap-3">
                <button
                  data-track="confirm_regenerate_api_key"
                  onClick={handleRegenerateKey}
                  disabled={regenerating}
                  className="px-4 py-2 bg-orange-600 text-white text-sm font-medium rounded-lg hover:bg-orange-700 disabled:opacity-50 transition-colors"
                >
                  {regenerating ? "Regenerating..." : "Yes, Regenerate"}
                </button>
                <button
                  data-track="cancel_regenerate_api_key"
                  onClick={() => setShowRegenConfirm(false)}
                  disabled={regenerating}
                  className="px-4 py-2 bg-white border border-slate-300 text-slate-700 text-sm font-medium rounded-lg hover:bg-slate-50 transition-colors"
                >
                  Cancel
                </button>
              </div>
          </div>
        )}

        {newKey && (
          <div className="bg-green-50 border border-green-200 rounded-lg p-4 mt-4">
              <h3 className="text-sm font-medium text-green-800 mb-2">
                New API Key Generated!
              </h3>
              <p className="text-sm text-green-700 mb-3">
                Please copy your new API key now. You won't be able to see it
                again. We've automatically updated your current session so you
                can continue working.
              </p>
              <div className="flex items-center gap-2">
                <code className="flex-1 block px-3 py-2 bg-white border border-green-300 rounded-md text-sm text-slate-800 break-all">
                  {newKey}
                </code>
                <button
                  data-track="copy_new_api_key"
                  onClick={() => {
                    navigator.clipboard.writeText(newKey);
                    toast.success("Copied to clipboard!");
                  }}
                  className="px-3 py-2 bg-green-600 text-white text-sm font-medium rounded-md hover:bg-green-700 transition-colors"
                >
                  Copy
                </button>
              </div>
          </div>
        )}
      </SettingsDisclosureCard>
    </div>
  );
}
