import { useEffect, useRef, useState, type FormEvent } from "react";
import { Link, useSearchParams, type MetaFunction } from "@remix-run/react";
import { ExternalLink, KeyRound, Loader2 } from "lucide-react";
import { Logo } from "~/components/Logo";
import { safeInternalPath } from "~/lib/safe-navigation";
import { api } from "~/lib/api";
import { APIRequestError } from "~/lib/authorization-error";
import { purgeLegacyBrowserCredential } from "~/lib/session";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m) => m.id === "root").flatMap((m) => m.meta ?? []);
  return [
    ...parentMeta.filter((m) => !("title" in m)),
    { title: "Sign in - Fused" },
  ];
};

const POLL_INTERVAL_MS = 1500;

type ManagedPollOutcome = "authenticated" | "closed" | "expired" | "cancelled";

function loginDestination(next: string): string {
  if (next !== "/cli-login" || typeof window === "undefined") return next;
  // CLI approval capabilities stay in the fragment so neither the Engine nor
  // identity provider receives them in request logs during authentication.
  const fragment = new URLSearchParams(window.location.hash.slice(1));
  return fragment.has("transaction_id") && fragment.has("browser_token") ? next + window.location.hash : next;
}

function isolateManagedLoginPopup(popup: Window): boolean {
  try {
    // The provider is cross-origin. Severing opener while the document is
    // still our blank page prevents it from navigating the Engine tab.
    popup.opener = null;
    popup.document.title = "Fused sign in";
    return true;
  } catch {
    popup.close();
    return false;
  }
}

function canRetryManagedPoll(error: unknown, failures: number): boolean {
  return failures < 3 && (!(error instanceof APIRequestError) || error.status >= 500);
}

async function waitForManagedLogin(
  popup: Window,
  transaction: Awaited<ReturnType<typeof api.auth.startManaged>>,
  signal: AbortSignal
): Promise<ManagedPollOutcome> {
  const expiresAt = Date.parse(transaction.expires_at);
  let failures = 0;
  while (!signal.aborted && Date.now() < expiresAt) {
    if (popup.closed) return "closed";
    try {
      const result = await api.auth.pollManaged(transaction.transaction_id, transaction.poll_token, signal);
      failures = 0;
      if (result.status === "authenticated") return "authenticated";
    } catch (error) {
      if (signal.aborted) return "cancelled";
      failures += 1;
      if (!canRetryManagedPoll(error, failures)) throw error;
    }
    await new Promise((resolve) => window.setTimeout(resolve, POLL_INTERVAL_MS));
  }
  return signal.aborted ? "cancelled" : "expired";
}

export default function Login() {
  const [searchParams] = useSearchParams();
  const next = safeInternalPath(searchParams.get("next"));
  const [apiKey, setAPIKey] = useState("");
  const [error, setError] = useState("");
  const [managedLoading, setManagedLoading] = useState(false);
  const [apiKeyLoading, setAPIKeyLoading] = useState(false);
  const cancelled = useRef(false);
  const pollController = useRef<AbortController | null>(null);

  useEffect(() => {
    cancelled.current = false;
    purgeLegacyBrowserCredential();
    api.auth.session().then((session) => {
      if (!cancelled.current && session.authenticated) window.location.replace(loginDestination(next));
    }).catch(() => undefined);
    return () => {
      cancelled.current = true;
      pollController.current?.abort();
    };
  }, [next]);

  async function handleManagedLogin() {
    // Embedded browsers can render cross-origin identity providers as a blank
    // page in popup-style windows. A fresh regular tab also avoids reusing a
    // completed cross-origin callback tab that this page can no longer navigate.
    const popup = window.open("about:blank", "_blank");
    if (!popup) {
      setError("Allow pop-ups for this Engine to continue with managed sign-in.");
      return;
    }
    if (!isolateManagedLoginPopup(popup)) {
      setError("This browser could not open sign-in safely. Try another browser.");
      return;
    }
    setManagedLoading(true);
    setError("");
    const controller = new AbortController();
    pollController.current?.abort();
    pollController.current = controller;
    try {
      const transaction = await api.auth.startManaged();
      popup.location.replace(transaction.verification_url);
      const outcome = await waitForManagedLogin(popup, transaction, controller.signal);
      if (outcome === "authenticated") {
        popup.close();
        window.location.replace(loginDestination(next));
        return;
      }
      popup.close();
      if (outcome === "closed") setError("Sign-in was closed before it completed.");
      if (outcome === "expired") setError("Sign-in expired. Start again when you are ready.");
    } catch {
      popup.close();
      setError("Managed sign-in could not be completed. Check that you were invited to this Engine.");
    } finally {
      controller.abort();
      if (pollController.current === controller) pollController.current = null;
      setManagedLoading(false);
    }
  }

  async function handleAPIKeyLogin(event: FormEvent) {
    event.preventDefault();
    const value = apiKey.trim();
    if (!value) return;
    setAPIKeyLoading(true);
    setError("");
    try {
      await api.auth.exchangeAPIKey(value);
      setAPIKey("");
      window.location.replace(loginDestination(next));
    } catch {
      setError("The API Key was not accepted for Engine access.");
      setAPIKeyLoading(false);
    }
  }

  return (
    <div className="min-h-screen flex flex-col bg-slate-50">
      <main className="flex-1 flex items-center justify-center px-4 py-10">
        <div className="w-full max-w-sm">
          <div className="mb-8 text-center flex flex-col items-center">
            <Logo size="lg" logoClassName="w-10 h-10" textClassName="text-3xl font-extrabold tracking-tight" />
            <p className="mt-2 text-sm text-slate-500">Sign in to your team&apos;s Engine</p>
          </div>

          <div className="bg-white rounded-xl shadow-sm border border-slate-200 p-8">
            <h1 className="text-lg font-semibold text-slate-900">Welcome back</h1>
            <p className="mt-1 text-sm text-slate-500">Use email, Google, Microsoft, or your enterprise identity.</p>
            <button
              data-track="start_managed_login"
              type="button"
              onClick={handleManagedLogin}
              disabled={managedLoading || apiKeyLoading}
              className="mt-6 w-full inline-flex items-center justify-center gap-2 py-2.5 px-4 bg-blue-600 hover:bg-blue-700 disabled:opacity-50 text-white text-sm font-semibold rounded-lg transition-colors"
            >
              {managedLoading ? <Loader2 className="w-4 h-4 animate-spin" /> : <ExternalLink className="w-4 h-4" />}
              {managedLoading ? "Waiting for sign-in…" : "Continue with email or SSO"}
            </button>

            <div className="my-6 flex items-center gap-3 text-xs uppercase tracking-wide text-slate-400">
              <span className="h-px flex-1 bg-slate-200" />
              API Key
              <span className="h-px flex-1 bg-slate-200" />
            </div>

            <form onSubmit={handleAPIKeyLogin} className="space-y-3" toolname="login_with_api_key" tooldescription="Use an Engine administrator API Key to sign in.">
              <label htmlFor="api-key" className="block text-sm font-medium text-slate-700">API Key</label>
              <input
                id="api-key"
                type="password"
                value={apiKey}
                onChange={(event) => setAPIKey(event.target.value)}
                placeholder="API Key"
                autoComplete="off"
                toolparam="password"
                className="w-full px-3 py-2 rounded-lg border border-slate-300 text-sm focus:outline-none focus:ring-2 focus:ring-blue-500"
              />
              <button
                data-track="submit_api_key_login"
                type="submit"
                disabled={apiKeyLoading || managedLoading || !apiKey.trim()}
                className="w-full inline-flex items-center justify-center gap-2 py-2 px-4 border border-slate-300 hover:bg-slate-50 disabled:opacity-50 text-slate-700 text-sm font-medium rounded-lg transition-colors"
              >
                {apiKeyLoading ? <Loader2 className="w-4 h-4 animate-spin" /> : <KeyRound className="w-4 h-4" />}
                {apiKeyLoading ? "Checking…" : "Use API Key"}
              </button>
            </form>

            {error && <p role="alert" className="mt-4 text-sm text-red-600">{error}</p>}

            <div className="mt-6 pt-5 border-t border-slate-100 text-center text-sm text-slate-500">
              Need access?{" "}
              <Link to="/#access" className="text-blue-600 hover:text-blue-700 font-semibold hover:underline">Ask your administrator</Link>
            </div>
          </div>
        </div>
      </main>
    </div>
  );
}
