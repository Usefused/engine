import { useEffect, useState } from "react";
import { CheckSquare, Square, ChevronDown, ChevronRight, Search, X } from "lucide-react";
import { api } from "~/lib/api";
import { filterMcpItems, mcpCatalogSections, mcpItemKey, type McpCatalogKind, type McpCatalogItem, type McpCatalogSnapshot } from "~/lib/mcp-catalog";

export type ImportedMcpSelection = { revision_id: string } & Partial<Record<McpCatalogKind, string[]>>;
type PickerProps = { serviceID: string; versionID: string; value?: ImportedMcpSelection; onChange: (serviceID: string, value?: ImportedMcpSelection) => void };

/** Counts explicit capability identities independently of the revision pin. */
export function importedMcpCount(selection?: ImportedMcpSelection): number {
  return mcpCatalogSections.reduce((count, section) => count + (selection?.[section.kind]?.length ?? 0), 0);
}

/** Cancels stale catalog reads when the service version changes. */
function useImportedCatalog(serviceID: string, versionID: string) {
  const [snapshot, setSnapshot] = useState<McpCatalogSnapshot | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [attempt, setAttempt] = useState(0);
  // Metadata belongs to one exact service version and must never survive a superseding read.
  useEffect(() => {
    const controller = new AbortController();
    setSnapshot(null); setLoading(true); setError("");
    // No version pin means there is no authorized catalog endpoint to read.
    if (!versionID) { setLoading(false); return () => controller.abort(); }
    api.mcpCatalog.get(serviceID, versionID, controller.signal).then((result) => {
      // Ignore reads completed after unmount or a service-version change.
      if (!controller.signal.aborted) setSnapshot(result.catalog);
    }).catch(() => {
      // Navigation cancellation is expected; actual failures remain visible and retryable.
      if (!controller.signal.aborted) setError("Could not load the imported MCP catalog.");
    }).finally(() => {
      // Only the current request can finish its loading state.
      if (!controller.signal.aborted) setLoading(false);
    });
    return () => controller.abort();
  }, [serviceID, versionID, attempt]);
  return { snapshot, loading, error, retry: () => setAttempt((previous) => previous + 1) };
}

/** Uses the same compact section header and independent Select All control as endpoints and webhooks. */
export function ImportedMcpPicker({ serviceID, versionID, value, onChange }: PickerProps) {
  const { snapshot, loading, error, retry } = useImportedCatalog(serviceID, versionID);
  const [expanded, setExpanded] = useState(false);
  const total = mcpCatalogSections.reduce((count, section) => count + (snapshot?.catalog[section.kind].length ?? 0), 0);
  const allSelected = Boolean(snapshot && value?.revision_id === snapshot.id && importedMcpCount(value) === total);

  /** Select All freezes today's catalog identities; it never grants future imports automatically. */
  function toggleAll() {
    // The control remains unavailable until a complete saved catalog is loaded.
    if (!snapshot) return;
    // Clearing the last capability removes the imported selection from the submitted document.
    if (allSelected) { onChange(serviceID, undefined); return; }
    const next: ImportedMcpSelection = { revision_id: snapshot.id };
    for (const { kind } of mcpCatalogSections) next[kind] = snapshot.catalog[kind].map((item) => mcpItemKey(kind, item)).sort();
    onChange(serviceID, next);
  }

  /** Individual selections retain a single reviewed catalog revision. */
  function toggle(kind: McpCatalogKind, key: string) {
    // Caller choices cannot manufacture metadata while a catalog is unavailable.
    if (!snapshot) return;
    const next: ImportedMcpSelection = value?.revision_id === snapshot.id ? { ...value } : { revision_id: snapshot.id };
    const selected = new Set(next[kind] ?? []);
    // Resource identities use URIs, so duplicate display names remain independently selectable.
    if (selected.has(key)) selected.delete(key); else selected.add(key);
    next[kind] = [...selected].sort();
    onChange(serviceID, importedMcpCount(next) > 0 ? next : undefined);
  }

  return <section aria-label="Imported MCP capabilities" className="border-t border-slate-100">
    <McpPickerHeader expanded={expanded} total={total} allSelected={allSelected} onExpand={() => setExpanded(!expanded)} toggleAll={toggleAll} />
    {expanded && <McpPickerContents snapshot={snapshot} loading={loading} error={error} retry={retry} value={value} toggle={toggle} />}
  </section>;
}

/** Groups compact capability rows with the same bordered search and resource headers as endpoint selectors. */
function McpCapabilityChoices({ snapshot, value, toggle }: { snapshot: McpCatalogSnapshot; value?: ImportedMcpSelection; toggle: (kind: McpCatalogKind, key: string) => void }) {
  const [query, setQuery] = useState("");
  const [kindFilter, setKindFilter] = useState<McpCatalogKind | "all">("all");
  const current = value?.revision_id === snapshot.id;
  const groups = mcpCatalogSections.filter(({ kind }) => kindFilter === "all" || kindFilter === kind).map((section) => ({ ...section, items: filterMcpItems(snapshot.catalog[section.kind], query) })).filter((section) => section.items.length > 0);
  return <>
    {value && !current && <p role="alert" className="mb-2 px-2 text-xs text-amber-700">The catalog has changed. Select its capabilities again.</p>}
    <div className="flex flex-col overflow-hidden rounded-lg border border-slate-200 bg-white shadow-sm">
      <div className="flex items-center gap-2 border-b border-slate-100 bg-slate-50/50 p-2">
        <div className="relative min-w-0 flex-1">
          <Search className="pointer-events-none absolute left-2.5 top-2 h-4 w-4 text-slate-400" />
          <input aria-label="Search MCP capabilities" value={query} onChange={(event) => setQuery(event.target.value)} placeholder="Search MCP capabilities..." className="w-full rounded-md border border-slate-200 bg-white py-1.5 pl-9 pr-8 text-xs text-slate-800 placeholder:text-slate-400 focus:border-blue-500 focus:outline-none focus:ring-1 focus:ring-blue-500" />
          {query && <button type="button" aria-label="Clear MCP search" onClick={() => setQuery("")} className="absolute right-2 top-2 text-slate-400 hover:text-slate-600"><X className="h-3.5 w-3.5" /></button>}
        </div>
        <select aria-label="MCP capability type" value={kindFilter} onChange={(event) => setKindFilter(event.target.value as McpCatalogKind | "all")} className="min-w-0 max-w-[40%] rounded-md border border-slate-200 bg-white px-2 py-1.5 text-xs text-slate-600">
          <option value="all">All types</option>{mcpCatalogSections.map(({ kind, label }) => <option key={kind} value={kind}>{label}</option>)}
        </select>
      </div>
      {groups.map(({ kind, label, items }) => <McpCapabilityGroup key={kind} kind={kind} label={label} items={items} selected={current ? value?.[kind] ?? [] : []} toggle={toggle} />)}
      {groups.length === 0 && <p className="p-8 text-center text-sm text-slate-400">No matching capabilities found</p>}
    </div>
  </>;
}

/** Keeps each MCP namespace collapsible like an endpoint resource group. */
function McpCapabilityGroup({ kind, label, items, selected, toggle }: { kind: McpCatalogKind; label: string; items: McpCatalogItem[]; selected: string[]; toggle: (kind: McpCatalogKind, key: string) => void }) {
  const [collapsed, setCollapsed] = useState(false);
  return <div className="bg-white">
    <button type="button" aria-expanded={!collapsed} onClick={() => setCollapsed(!collapsed)} className="flex w-full items-center gap-1.5 border-y border-slate-200 bg-slate-100 px-3 py-2 text-left transition-colors first:border-t-0 hover:bg-slate-200/70">
      {collapsed ? <ChevronRight className="h-3.5 w-3.5 text-slate-400" /> : <ChevronDown className="h-3.5 w-3.5 text-slate-400" />}
      <span className="text-xs font-bold uppercase tracking-wider text-slate-600">{label} ({items.length})</span>
    </button>
    {!collapsed && <div className="divide-y divide-slate-100">{items.map((item) => {
      const key = mcpItemKey(kind, item);
      return <McpCapabilityRow key={key} kind={kind} item={item} selected={selected.includes(key)} onToggle={() => toggle(kind, key)} />;
    })}</div>}
  </div>;
}

/** Substitutes the MCP capability type for the endpoint method badge without changing row spacing. */
function McpCapabilityRow({ kind, item, selected, onToggle }: { kind: McpCatalogKind; item: McpCatalogItem; selected: boolean; onToggle: () => void }) {
  const badge = { tools: "TOOL", prompts: "PROMPT", resources: "RESOURCE", resource_templates: "TEMPLATE" }[kind];
  return <label className={`flex cursor-pointer items-start gap-3 p-3 transition-colors ${selected ? "bg-blue-50/40" : "hover:bg-slate-50"}`}>
    <input type="checkbox" checked={selected} onChange={onToggle} className="mt-1 h-4 w-4 shrink-0 cursor-pointer rounded border-slate-300 text-blue-600 focus:ring-blue-500" />
    <span className="min-w-0 flex-1">
      <span className="mb-1 flex items-center gap-2">
        <span className="rounded border border-purple-200 bg-purple-100 px-1.5 py-0.5 text-[10px] font-bold text-purple-700 shadow-sm">{badge}</span>
        <span className={`truncate text-sm font-medium ${selected ? "text-blue-900" : "text-slate-900"}`}>{item.title || item.name}</span>
      </span>
      <span className="block truncate text-xs text-slate-500">{item.description}</span>
    </span>
  </label>;
}

/** Mirrors the adjacent endpoint and webhook header without coupling disclosure to selection. */
function McpPickerHeader({ expanded, total, allSelected, onExpand, toggleAll }: { expanded: boolean; total: number; allSelected: boolean; onExpand: () => void; toggleAll: () => void }) {
  return (
    <div className="flex w-full items-center justify-between px-3 py-2 transition-colors hover:bg-slate-100">
      <button type="button" aria-expanded={expanded} onClick={onExpand} className="flex min-h-7 flex-1 items-center gap-2 text-left">
        {expanded ? <ChevronDown className="h-3.5 w-3.5 text-slate-400" /> : <ChevronRight className="h-3.5 w-3.5 text-slate-400" />}
        <span className="text-xs font-bold uppercase tracking-wider text-slate-500">MCP{total > 0 ? ` (${total})` : ""}</span>
      </button>
      {total > 0 && <button type="button" onClick={toggleAll} aria-label={allSelected ? "Deselect all MCP capabilities" : "Select all MCP capabilities"} className="flex items-center gap-1.5 rounded px-2 py-1 text-[10px] font-medium text-slate-500 transition-colors hover:bg-slate-200 hover:text-slate-700">
        {allSelected ? <CheckSquare className="h-3.5 w-3.5 text-slate-950" /> : <Square className="h-3.5 w-3.5 text-slate-400" />}
        {allSelected ? "Deselect All" : `Select All (${total})`}
      </button>}
    </div>
  );
}
/** Keeps loading, empty, and failure states inside the same collapsible section. */
function McpPickerContents({ snapshot, loading, error, retry, value, toggle }: { snapshot: McpCatalogSnapshot | null; loading: boolean; error: string; retry: () => void; value?: ImportedMcpSelection; toggle: (kind: McpCatalogKind, key: string) => void }) {
  return (
    <div className="px-2 pb-2">
      {loading && <p role="status" className="px-3 py-4 text-xs text-slate-400">Loading MCP capabilities…</p>}
      {error && <p role="alert" className="px-3 py-3 text-xs text-red-600">{error} <button type="button" onClick={retry} className="underline">Retry</button></p>}
      {!loading && !error && !snapshot && <p className="px-3 py-3 text-xs text-slate-400">No MCP server imported for this service version.</p>}
      {snapshot && <McpCapabilityChoices snapshot={snapshot} value={value} toggle={toggle} />}
    </div>
  );
}
