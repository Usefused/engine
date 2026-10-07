import { useState } from "react";
import { createRoot } from "react-dom/client";
import { AppExecutionTokens } from "../../app/components/apps/AppExecutionTokens";
import { CurrentActorAccessProvider } from "../../app/components/access/CurrentActorAccess";
import { Select } from "../../app/components/forms/Select";

/** Exercises the production permission and issuance paths against a synthetic local Engine transport. */
function Preview() {
  const [kind, setKind] = useState<"sdk" | "mcp" | "api" | "unified_app">("mcp");
  return <CurrentActorAccessProvider isAuth><main className="mx-auto max-w-3xl space-y-6 p-5">
    <header><p className="text-xs text-slate-500">Local preview · synthetic credentials only</p><h1 className="mt-2 text-2xl font-bold text-slate-900">Customer checkout</h1></header>
    <label className="block max-w-xs text-sm text-slate-600">App type<Select value={kind} onChange={event => setKind(event.target.value as typeof kind)}><option value="mcp">MCP</option><option value="sdk">SDK</option><option value="api">REST API</option><option value="unified_app">Unified App</option></Select></label>
    <AppExecutionTokens familyID="11111111-1111-4111-8111-111111111111" kind={kind} />
  </main></CurrentActorAccessProvider>;
}
createRoot(document.getElementById("root")!).render(<Preview />);
