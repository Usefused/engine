import { useEffect, useState } from "react";
import type { MetaFunction } from "@remix-run/react";
import { CheckCircle2, Loader2, XCircle } from "lucide-react";
import { Logo } from "~/components/Logo";
import { api } from "~/lib/api";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m) => m.id === "root").flatMap((m) => m.meta ?? []);
  return [...parentMeta.filter((m) => !("title" in m)), { title: "Connect CLI - Fused" }];
};

type ApprovalState = "approving" | "approved" | "failed";

function approvalCapability(): { transactionId: string; browserToken: string } | null {
  const fragment = new URLSearchParams(window.location.hash.slice(1));
  const transactionId = fragment.get("transaction_id") ?? "";
  const browserToken = fragment.get("browser_token") ?? "";
  // Remove the capability from browser history as soon as it is held in this
  // page's memory; refresh intentionally cannot replay an approval.
  window.history.replaceState(null, "", "/cli-login");
  if (!transactionId || !browserToken) return null;
  return { transactionId, browserToken };
}

export default function CLILogin() {
  const [state, setState] = useState<ApprovalState>("approving");

  useEffect(() => {
    let active = true;
    const capability = approvalCapability();
    if (!capability) {
      setState("failed");
      return () => { active = false; };
    }
    void (async () => {
      try {
        const session = await api.auth.session();
        if (!active) return;
        if (!session.authenticated) {
          const fragment = new URLSearchParams({
            transaction_id: capability.transactionId,
            browser_token: capability.browserToken,
          });
          window.location.replace(`/login?next=/cli-login#${fragment.toString()}`);
          return;
        }
        await api.auth.approveCLI(capability.transactionId, capability.browserToken);
        if (active) setState("approved");
      } catch {
        if (active) setState("failed");
      }
    })();
    return () => { active = false; };
  }, []);

  return (
    <div className="min-h-screen flex items-center justify-center bg-slate-50 px-4">
      <main className="w-full max-w-sm rounded-xl border border-slate-200 bg-white p-8 text-center shadow-sm">
        <Logo size="lg" logoClassName="w-10 h-10" textClassName="text-3xl font-extrabold tracking-tight" />
        {state === "approving" && (
          <><Loader2 className="mx-auto mt-8 h-8 w-8 animate-spin text-blue-600" /><h1 className="mt-4 text-lg font-semibold">Connecting your CLI…</h1></>
        )}
        {state === "approved" && (
          <><CheckCircle2 className="mx-auto mt-8 h-9 w-9 text-emerald-600" /><h1 className="mt-4 text-lg font-semibold">CLI connected</h1><p className="mt-2 text-sm text-slate-500">You can close this tab and return to your terminal.</p></>
        )}
        {state === "failed" && (
          <><XCircle className="mx-auto mt-8 h-9 w-9 text-red-600" /><h1 className="mt-4 text-lg font-semibold">CLI connection failed</h1><p className="mt-2 text-sm text-slate-500">This request may have expired or already been used. Run <code>fused-cli login</code> again.</p></>
        )}
      </main>
    </div>
  );
}
