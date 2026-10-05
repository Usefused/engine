import { FieldLabel } from "~/components/forms/FieldLabel";
import { Select } from "../forms/Select.ts";
import { useEffect, useRef, useState } from "react";
import { useNavigate } from "@remix-run/react";
import { ChevronDown, Code2, Loader2, Search, TerminalSquare, X } from "lucide-react";
import { api } from "~/lib/api";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasResourcePermission, hasWorkspacePermission } from "~/lib/current-actor-access";

type Destination = { app_family_id: string; name: string; latest_version: string; latest_version_id: string | null };
type App = { app_id: string; app_family_id: string; name: string; version: string };

/** Carries one hosted capability into the ordinary SDK/MCP create-or-update builder. */
export function UnifiedAppUsage({ app }: { app: App }) {
  const { access } = useCurrentActorAccess();
  const navigate = useNavigate();
  const dialog = useRef<HTMLDialogElement>(null);
  const [kind, setKind] = useState<"sdk" | "mcp">("sdk");
  const [menuOpen, setMenuOpen] = useState(false);
  const menu = useRef<HTMLDivElement>(null);
  const [query, setQuery] = useState("");
  const [items, setItems] = useState<Destination[]>([]);
  const [selected, setSelected] = useState("");
  const [busy, setBusy] = useState(false);
  const searchEpoch = useRef(0);
  const [searched, setSearched] = useState(false);
  const [error, setError] = useState("");
  const canUse = hasResourcePermission(access, "app.unified_app.use", "APP", app.app_family_id);
  const label = kind === "sdk" ? "SDK" : "MCP server";

  // Changing adapters invalidates every destination chosen under the previous namespace.
  useEffect(() => { setItems([]); setSelected(""); setError(""); }, [kind]);

  // Dismiss the action disclosure without keeping document handlers alive while closed.
  useEffect(() => {
    if (!menuOpen) return;
    /** Outside clicks leave the overview untouched. */
    function outside(event: PointerEvent) { if (!menu.current?.contains(event.target as Node)) setMenuOpen(false); }
    /** Escape closes the menu and returns focus to its trigger. */
    function escape(event: KeyboardEvent) { if (event.key === "Escape") { setMenuOpen(false); menu.current?.querySelector("button")?.focus(); } }
    document.addEventListener("pointerdown", outside); document.addEventListener("keydown", escape);
    return () => { document.removeEventListener("pointerdown", outside); document.removeEventListener("keydown", escape); };
  }, [menuOpen]);

  /** Existing destinations need a modal; new consumers can go directly to configuration. */
  function choose(next: "sdk" | "mcp", existing: boolean) {
    setMenuOpen(false);
    // Dependency use is independent of the destination's create/manage grant.
    if (!canUse) return;
    if (!existing) { navigate(`/integrations/builder?${new URLSearchParams({ tab: next, unifiedApp: app.app_id })}`); return; }
    searchEpoch.current++; setBusy(false); setSearched(false);
    setKind(next); setQuery(""); setItems([]); setSelected(""); setError("");
    dialog.current?.showModal();
  }

  /** Closes without making any management request or changing app state. */
  function close() { searchEpoch.current++; setBusy(false); dialog.current?.close(); }

  /** Searches the selected adapter using the existing bounded Engine catalogue. */
  async function search() {
    const epoch = ++searchEpoch.current;
    setBusy(true); setSearched(false); setError(""); setSelected("");
    try {
      const result = await api.mcpGraphql<{ appFamilies: { items: Destination[] } }>(`query UnifiedAppDestinations($kind: String!, $search: String!) {
        appFamilies(kind: $kind, search: $search, limit: 50, offset: 0) { items { app_family_id name latest_version latest_version_id } }
      }`, { kind, search: query });
      // A closed or changed destination must never receive stale search results.
      if (epoch !== searchEpoch.current) return;
      setSearched(true);
      // Only exact versions with edit authority can be selected as a successor's source.
      setItems(result.appFamilies.items.filter((item) => item.latest_version_id && hasResourcePermission(access, `app.${kind}.manage`, "APP", item.app_family_id)));
    } catch (cause) {
      // Ignore failures from a previous modal session.
      if (epoch === searchEpoch.current) setError(String(cause));
    } finally {
      // Only the current request owns the loading state.
      if (epoch === searchEpoch.current) setBusy(false);
    }
  }

  /** Navigation records selection only; the shared builder owns reviewed plan/apply. */
  function continueSetup() {
    // A destination and dependency grant must be explicit before opening configuration.
    if (!canUse || !selected) return;
    const params = new URLSearchParams({ tab: kind, unifiedApp: app.app_id });
    params.set("app", selected);
    close(); navigate(`/integrations/builder?${params}`);
  }

  return <section className="rounded-xl border border-slate-200 bg-white p-5">
    <div className="flex flex-col justify-between gap-4 sm:flex-row sm:items-center"><div><h2 className="font-semibold text-slate-900">Use in your application</h2><p className="mt-1 text-sm text-slate-500">Create an SDK or MCP server, or add this app to an existing one.</p></div>
      <div ref={menu} className="relative shrink-0">
        {/* Keep this secondary action visually quieter than the page's primary Edit app action. */}
        <button type="button" disabled={!canUse} aria-expanded={menuOpen} aria-controls="unified-app-usage-actions" onClick={() => setMenuOpen(!menuOpen)} className="flex min-h-11 w-full items-center justify-center gap-2 rounded-lg border border-slate-200 bg-white px-4 text-sm font-medium text-slate-700 hover:bg-slate-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-violet-500 focus-visible:ring-offset-2 disabled:opacity-50 sm:w-auto">Use this app<ChevronDown className="h-4 w-4" /></button>
        {/* One disclosure keeps the overview compact; only existing-app actions open the modal. */}
        {menuOpen && <div id="unified-app-usage-actions" className="absolute right-0 top-full z-20 mt-2 w-full min-w-64 rounded-xl border border-slate-200 bg-white p-1.5 shadow-lg">
          {[{ kind: "sdk" as const, label: "SDK", Icon: Code2 }, { kind: "mcp" as const, label: "MCP", Icon: TerminalSquare }].map((option) => <div key={option.kind} className="border-b border-slate-100 last:border-0"><span className="flex items-center gap-2 px-3 pb-1 pt-2 text-xs font-medium text-slate-400"><option.Icon className="h-3.5 w-3.5" />{option.label}</span>
            <button type="button" disabled={!hasWorkspacePermission(access, `app.${option.kind}.create`)} onClick={() => choose(option.kind, false)} className="block min-h-10 w-full rounded-lg px-3 text-left text-sm text-slate-700 hover:bg-slate-50 disabled:opacity-40">Create {option.label}</button>
            <button type="button" onClick={() => choose(option.kind, true)} className="mb-1 block min-h-10 w-full rounded-lg px-3 text-left text-sm text-slate-700 hover:bg-slate-50">Add to existing {option.label}</button>
          </div>)}
        </div>}
      </div>
    </div>
    {!canUse && <p className="mt-3 text-xs text-slate-500">You need permission to use this Unified App.</p>}
    <dialog onCancel={close} ref={dialog} aria-labelledby="unified-usage-title" className="m-auto max-h-[calc(100dvh-2rem)] w-[calc(100%-2rem)] max-w-lg rounded-xl border border-slate-200 bg-white p-0 text-slate-900 shadow-xl backdrop:bg-slate-950/40">
      <div className="flex items-start justify-between gap-4 px-5 pt-5"><div><h2 id="unified-usage-title" className="text-lg font-semibold">Add to existing {label}</h2><p className="mt-1 text-sm text-slate-500">Choose where to add this Unified App.</p></div><button aria-label="Close" onClick={close} className="rounded p-2 text-slate-500 hover:bg-slate-50"><X className="h-4 w-4" /></button></div>
      <div className="space-y-4 p-5">
        <div className="flex flex-wrap justify-between gap-2 rounded-lg border border-slate-200 bg-slate-50 p-3 text-sm"><strong>{app.name}</strong><span className="text-slate-500">Version {app.version}</span></div>
        {/* Existing immutable apps are selected by name; their IDs remain routing details. */}
        <div className="space-y-3"><form className="flex gap-2" onSubmit={(event) => { event.preventDefault(); void search(); }}><input aria-label={`Search ${label}s`} placeholder={`Search ${label}s`} value={query} onChange={(event) => setQuery(event.target.value)} className="min-w-0 flex-1 rounded-lg border border-slate-300 px-3 py-2 text-sm" /><button type="submit" disabled={busy} aria-label="Search apps" className="rounded-lg border border-slate-200 p-3">{busy ? <Loader2 className="h-4 w-4 animate-spin" /> : <Search className="h-4 w-4" />}</button></form><label className="block text-sm font-medium"><FieldLabel required>{label}</FieldLabel><Select required value={selected} onChange={(event) => setSelected(event.target.value)} className="mt-2 min-h-11 w-full rounded-lg border border-slate-300 bg-white px-3 text-sm"><option value="">Select an existing {label}</option>{items.map((item) => <option key={item.app_family_id} value={item.latest_version_id!}>{item.name} · {item.latest_version}</option>)}</Select></label><p className="text-xs text-slate-500">Review a new version before applying. Existing operations and settings stay included.</p></div>
        {/* Empty searches remain actionable rather than leaving an unexplained blank selector. */}
        {searched && items.length === 0 && <p role="status" className="text-sm text-slate-500">No matching apps you can manage. Try another name.</p>}
        {error && <p role="alert" className="text-sm text-red-700">{error}</p>}
      </div>
      <div className="flex justify-end gap-2 border-t border-slate-100 p-4"><button type="button" onClick={close} className="min-h-11 rounded-lg border border-slate-200 px-4 text-sm">Cancel</button><button type="button" disabled={busy || !selected} onClick={continueSetup} className="min-h-11 rounded-lg bg-slate-950 px-4 text-sm font-medium text-white disabled:opacity-50">Continue</button></div>
    </dialog>
  </section>;
}
