import { useState } from "react";
import { Search, Loader2 } from "lucide-react";
import { api } from "~/lib/api";

/** Chooses an existing app without introducing a separate workflow installation form. */
export function WorkflowAppDestination({ appID, onSelect, disabled }: { appID: string; onSelect: (id: string) => void; disabled: boolean }) {
  const [open, setOpen] = useState(Boolean(appID));
  const [search, setSearch] = useState("");
  const [items, setItems] = useState<Array<{ name: string; kind: string; latest_version_id: string; latest_version: string }>>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // Existing-app discovery is an explicit bounded request; the source endpoint separately enforces edit access.
  async function findApps() {
    setBusy(true); setError("");
    try {
      const result = await api.mcpGraphql<{ appFamilies: { items: typeof items } }>(`query WorkflowAppChoices($search: String!) {
        appFamilies(search: $search, limit: 20, offset: 0) { items { name kind latest_version_id latest_version } }
      }`, { search });
      setItems(result.appFamilies.items);
    } catch (cause) { setError(String(cause)); }
    finally { setBusy(false); }
  }
  return <section className="mb-6 text-sm">
    <button type="button" disabled={disabled} onClick={() => setOpen(!open)} className="text-slate-500 hover:text-slate-900">Add to an existing app</button>
    {/* Discovery is optional; new apps continue through the ordinary builder mode selection. */}
    {open && <div className="mt-3 space-y-3 rounded-lg border border-slate-200 bg-white p-4">
      <div className="relative">
        <input aria-label="Search existing apps" placeholder="Search existing apps" value={search} onChange={(event) => setSearch(event.target.value)}
          onKeyDown={(event) => { /* Enter performs discovery, not an app creation. */ if (event.key === "Enter") { event.preventDefault(); void findApps(); } }}
          className="w-full rounded-lg border border-slate-300 py-2 pl-9 pr-3 text-sm" />
        <button type="button" aria-label="Search apps" disabled={busy || disabled} onClick={findApps} className="absolute left-2.5 top-1/2 -translate-y-1/2 text-slate-400">
          {/* Progress belongs to the lookup, never to the selected app's immutable state. */}
          {busy ? <Loader2 className="h-4 w-4 animate-spin" /> : <Search className="h-4 w-4" />}
        </button>
      </div>
      <label className="block text-xs text-slate-500">Destination
        <select aria-label="Destination" value={appID} disabled={disabled || busy} onChange={(event) => onSelect(event.target.value)} className="mt-1 w-full rounded-lg border border-slate-300 p-2 text-sm text-slate-700">
          <option value="">Create a new app</option>
          {/* A deep-linked source remains selectable before the optional search has run. */}
          {appID && !items.some((item) => item.latest_version_id === appID) && <option value={appID}>Current app</option>}
          {items.map((item) => <option key={item.latest_version_id} value={item.latest_version_id}>{item.name} · {item.kind} · {item.latest_version}</option>)}
        </select>
      </label>
      {/* A failed lookup never changes the app selected in the URL. */}
      {error && <p role="alert" className="text-sm text-red-700">{error}</p>}
    </div>}
  </section>;
}
