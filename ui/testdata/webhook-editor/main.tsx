import { useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import { createBrowserRouter, RouterProvider, Link } from "react-router-dom";
import WebhooksTab from "../../app/components/WebhooksTab";
import { CurrentActorAccessProvider } from "../../app/components/access/CurrentActorAccess";
import type { Service } from "../../app/lib/api";

// Render the production catalogue/editor inside a real router while every API is synthetic and local.
function Fixture() {
  const [service, setService] = useState<Service | null>(null);
  const [owner, setOwner] = useState(true);
  const [selected, setSelected] = useState("");
  const [saved, setSaved] = useState(0);
  // Refreshing a fixture receipt uses the same post-save render lifecycle without a real Registry.
  async function refresh() { const response = await fetch("/fixture/service"); setService(await response.json()); }
  useEffect(() => { void refresh(); }, []);
  // Scenario selection affects only the isolated mock handler, never any Engine configuration.
  async function mode(value: string) { await fetch("/fixture/mode", { method: "POST", body: JSON.stringify({ mode: value }) }); }
  return <CurrentActorAccessProvider isAuth><main className="mx-auto max-w-5xl space-y-5 p-5"><h1 className="text-xl font-semibold">Webhook editor test fixture</h1><p className="text-sm text-slate-500">Synthetic data only. No live Engine, Registry, credentials or subscriptions.</p><div className="flex flex-wrap gap-4"><label>Viewer <select aria-label="Viewer" value={String(owner)} onChange={(e) => setOwner(e.target.value === "true")}><option value="true">Owner</option><option value="false">Non-owner</option></select></label><label>Apply outcome <select aria-label="Apply outcome" onChange={(e) => void mode(e.target.value)}><option value="success">Success</option><option value="stale">Stale revision</option><option value="uncertain">Interrupted after commit</option></select></label><Link to="/other-version">Switch version</Link></div><p role="status">Confirmed saves: {saved}</p>{selected && <p>Read-only event: {selected}</p>}{service && <WebhooksTab srv={{ ...service, is_owner: owner }} version="v1" setSelectedEndpoint={(event) => setSelected(event?.name ?? "")} onSaved={() => { setSaved((count) => count + 1); void refresh(); }} />}</main></CurrentActorAccessProvider>;
}

const router = createBrowserRouter([{ path: "*", element: <Fixture /> }]);
createRoot(document.getElementById("root")!).render(<RouterProvider router={router} />);
