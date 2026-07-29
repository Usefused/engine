import { useState, useEffect, type FormEvent } from "react";
import { useNavigate, Link, useSearchParams, type MetaFunction } from "@remix-run/react";
import { Logo } from "~/components/Logo";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m: any) => m.id === "root").flatMap((m: any) => m.meta ?? []);
  return [
    ...parentMeta.filter((m: any) => !("title" in m)),
    { title: "Login - Fused" },
  ];
};
import { CreditBanner } from "~/components/CreditBanner";
import { setApiKey, clearApiKey, isAuthenticated } from "~/lib/session";
import { api } from "~/lib/api";

export default function Login() {
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const next = searchParams.get("next") || "/integrations";
  const [key, setKey] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (isAuthenticated()) window.location.replace(next);
  }, []);

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    if (!key.trim()) return;
    setLoading(true);
    setError("");

    // Set the key so the api client can use it, then verify against an
    // authenticated endpoint. /health has no auth so it can't validate the key.
    setApiKey(key.trim());
    try {
      await api.graphql("query { sdks { total } }");
      window.location.href = next;
    } catch (err: unknown) {
      // Clear the invalid key so it doesn't persist in sessionStorage.
      clearApiKey();
      const msg = err instanceof Error ? err.message : "";
      if (msg.includes("403") || msg.includes("401") || msg.includes("invalid") || msg.includes("missing")) {
        setError("Invalid API key.");
      } else {
        setError("Could not connect — make sure the server is running.");
      }
      setLoading(false);
    }
  }

  return (
    <div className="min-h-screen flex flex-col bg-slate-50">
      <CreditBanner />
      <main className="flex-1 flex items-center justify-center">
        <div className="w-full max-w-sm">
          <div className="mb-8 text-center flex flex-col items-center">
            <Logo size="lg" logoClassName="w-10 h-10" textClassName="text-3xl font-extrabold tracking-tight" />
            <p className="mt-2 text-sm text-slate-500">The integration layer for teams that want control</p>
          </div>

        <div className="bg-white rounded-xl shadow-sm border border-slate-200 p-8">
          <h2 className="text-base font-semibold text-slate-800 mb-4">Enter your API key</h2>
          <form 
            onSubmit={handleSubmit} 
            className="space-y-4"
            toolname="login_with_api_key"
            tooldescription="Authenticate into the Fused dashboard using your API key."
          >
            <input
              type="password"
              value={key}
              onChange={(e) => setKey(e.target.value)}
              placeholder="key1"
              toolparam="password"
              tooldescription="The user's API key for authentication"
              className="w-full px-3 py-2 rounded-lg border border-slate-300 text-sm focus:outline-none focus:ring-2 focus:ring-blue-500"
              autoFocus
            />
            {error && <p className="text-sm text-red-600">{error}</p>}
            <button
              data-track="submit_login"
              type="submit"
              disabled={loading || !key.trim()}
              className="w-full py-2 px-4 bg-blue-600 hover:bg-blue-700 disabled:opacity-50 text-white text-sm font-medium rounded-lg transition-colors"
            >
              {loading ? "Connecting…" : "Connect"}
            </button>
          </form>
          <div className="mt-6 pt-5 border-t border-slate-100 text-center text-sm text-slate-500">
            Don't have an account?{" "}
            <Link
              to="/#access"
              className="text-blue-600 hover:text-blue-700 font-semibold hover:underline transition-colors focus:outline-none focus:ring-2 focus:ring-blue-500 rounded"
            >
              Request Access
            </Link>
          </div>
        </div>
      </div>
      </main>
    </div>
  );
}
