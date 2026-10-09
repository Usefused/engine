import { McpCatalogDetails, catalogTypeBadges } from "./McpCatalogDetails";
import { ServiceCatalogHeader, ServiceCatalogOptions } from "~/components/integration-details/ServiceCatalogHeader";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { ChevronDown, Loader2, Search } from "lucide-react";
import { Select } from "~/components/forms/Select";
import { api } from "~/lib/api";
import { filterMcpItems, mcpCatalogSections, mcpItemKey, type McpCatalog, type McpCatalogItem, type McpCatalogKind, type McpCatalogPreview, type McpCatalogSnapshot, type McpDiscoveryInput } from "~/lib/mcp-catalog";

const inputClass = "w-full rounded-lg border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 focus:border-blue-500 focus:outline-none focus:ring-2 focus:ring-blue-100";
const buttonClass = "inline-flex items-center justify-center gap-2 rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-700 disabled:cursor-not-allowed disabled:opacity-50";

/** Mounts only an authorized exact version, so navigation cannot retain another version's draft. */
export default function McpCatalogTab({ serviceID, versionID, canRead, canManage }: { serviceID: string; versionID?: string; canRead: boolean; canManage: boolean }) {
  // A private catalog must never initiate discovery or reads from a public service page.
  if (!canRead) return <p className="rounded-xl border border-slate-200 bg-white p-8 text-sm text-slate-500">Sign in with service access to view your imported MCP catalog.</p>;
  // Catalogs attach to an exact active workspace version, never a floating service label.
  if (!versionID) return <p className="rounded-xl border border-slate-200 bg-white p-8 text-sm text-slate-500">Select a service version enabled in this workspace to import an MCP server.</p>;
  return <CatalogWorkspace key={`${serviceID}:${versionID}`} serviceID={serviceID} versionID={versionID} canManage={canManage} />;
}

/** Owns abortable discovery and reviewed imports while keeping the last saved catalog visible on failures. */
function CatalogWorkspace({ serviceID, versionID, canManage }: { serviceID: string; versionID: string; canManage: boolean }) {
  const [saved, setSaved] = useState<McpCatalogSnapshot | null>(null);
  const [preview, setPreview] = useState<McpCatalogPreview | null>(null);
  const [editing, setEditing] = useState(false);
  const [busy, setBusy] = useState("Loading catalog");
  const [error, setError] = useState("");
  const [reload, setReload] = useState(0);
  const controller = useRef<AbortController | null>(null);
  // Cleanup cancels requests when the selected service or version leaves the page.
  useEffect(() => {
    const current = new AbortController();
    controller.current = current;
    api.mcpCatalog.get(serviceID, versionID, current.signal).then((result) => {
      // Aborted responses must not replace the next mounted catalog state.
      if (!current.signal.aborted) setSaved(result.catalog);
    }).catch((cause) => {
      // Navigation cancellation is expected and should not appear as a provider failure.
      if (!current.signal.aborted) setError(cause.message);
    }).finally(() => {
      // The initial load owns its pending indicator until cancelled or settled.
      if (!current.signal.aborted) setBusy("");
    });
    return () => controller.current?.abort();
  }, [serviceID, versionID, reload]);

  /** Creates a fresh reviewable draft and preserves the saved snapshot until apply succeeds. */
  async function discover(input: McpDiscoveryInput) {
    controller.current?.abort();
    const current = new AbortController();
    controller.current = current;
    setBusy("Discovering server"); setError(""); setPreview(null);
    try {
      const result = await api.mcpCatalog.discover(serviceID, versionID, input, current.signal);
      // Only the current, mounted request may expose a draft for approval.
      if (!current.signal.aborted) { setPreview(result); setEditing(false); }
    } catch (cause) {
      // Cancelled discovery never discards or replaces a previously imported catalog.
      if (!current.signal.aborted) setError(String((cause as Error).message));
    } finally {
      // A cancelled request may already have been replaced by a newer action.
      if (!current.signal.aborted) setBusy("");
    }
  }

  /** Applies the exact server-held draft once the user has reviewed its catalog and diff. */
  async function apply() {
    // Only a completed preview is eligible for import.
    if (!preview) return;
    const current = new AbortController(); controller.current = current;
    setBusy("Importing catalog"); setError("");
    try {
      const result = await api.mcpCatalog.apply(serviceID, versionID, preview.id, current.signal);
      // Navigation must not surface an old service's success in the new tab.
      if (!current.signal.aborted) { setSaved(result); setPreview(null); }
    } catch (cause) {
      // Preserve the preview so failed imports remain reviewable and recoverable.
      if (!current.signal.aborted) setError(String((cause as Error).message));
    } finally {
      // The action owns the pending state only while it is still mounted.
      if (!current.signal.aborted) setBusy("");
    }
  }

  /** Retires stale drafts before editing the service's one server connection. */
  function editConnection() {
    setPreview(null); setError(""); setEditing(true);
  }

  // A draft is displayed for review but never replaces the saved connection before apply.
  const visible = preview ?? saved;
  return <section className="space-y-5" aria-label="MCP catalog">
    <CatalogSections catalog={visible?.catalog} actions={<CatalogActions saved={saved} canManage={canManage} busy={!!busy || editing || !!preview} onEdit={editConnection} onRefresh={discover} />}>
    {/* The connection belongs to this service; upstream handshake names are not a second service identity. */}
    {visible && <div className="flex flex-wrap items-center justify-between gap-2 border-b border-slate-100 bg-slate-50/50 px-4 py-2.5 text-xs sm:px-5"><p className="min-w-0 break-all text-slate-600"><span className="mr-2 text-slate-400">Server URL</span>{visible.url}</p><span className="text-slate-400">Updated {new Date(visible.created_at).toLocaleString()}</span></div>}
    {/* Failed requests remain explicit rather than presenting an empty successful catalog. */}
    {error && <div role="alert" className="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-800">{error}<button className="ml-3 underline" disabled={!!busy} onClick={() => { setPreview(null); setEditing(false); setError(""); setBusy("Loading catalog"); setReload((value) => value + 1); }}>Reload saved catalog</button></div>}
    {busy && <p role="status" className="flex items-center gap-2 text-sm text-slate-500"><Loader2 size={16} className="animate-spin" />{busy}…</p>}
    {/* Editing is explicit so opening a service page never contacts a third party. */}
    {/* Losing ownership must immediately hide the connection editor. */}
    {editing && canManage && <ImportForm initial={saved} busy={!!busy} onDiscover={discover} onCancel={() => setEditing(false)} />}
    <CatalogReview preview={preview} saved={saved} busy={!!busy} canManage={canManage} onDiscard={() => setPreview(null)} onApply={apply} />
    <CatalogEmpty visible={!!visible} busy={!!busy} editing={editing} error={error} />
    </CatalogSections>
  </section>;
}

/** Names the action after whether this service already has an attached server. */
function connectionActionLabel(saved: McpCatalogSnapshot | null) {
  // Existing attachments are edited in place instead of creating a second identity.
  return saved ? "Change server URL" : "Import MCP server";
}

/** Explains the single import entry point only after a successful empty read. */
function CatalogEmpty({ visible, busy, editing, error }: { visible: boolean; busy: boolean; editing: boolean; error: string }) {
  // Pending, failed, and editing states must never masquerade as an empty saved catalog.
  if (visible || busy || editing || error) return null;
  return <p className="px-5 py-12 text-center text-sm text-slate-500">No MCP server connected. The service owner can import one from Options.</p>;
}

/** Makes URL replacement and refresh reviewable before either can change the saved attachment. */
function CatalogReview({ preview, saved, busy, canManage, onDiscard, onApply }: { preview: McpCatalogPreview | null; saved: McpCatalogSnapshot | null; busy: boolean; canManage: boolean; onDiscard: () => void; onApply: () => Promise<void> }) {
  // No action can promote a catalog before discovery has produced a server-held draft.
  if (!preview) return null;
  return <div className="flex flex-wrap items-center justify-between gap-4 border-b border-blue-200 bg-blue-50 p-4">
    <div><p className="font-medium text-blue-950">Review discovered catalog</p><p className="mt-1 text-sm text-blue-800">{preview.changes.added} added · {preview.changes.changed} changed · {preview.changes.removed} removed. Preview expires {new Date(preview.expires_at).toLocaleTimeString()}.</p></div>
    <div className="flex gap-3"><button disabled={busy} onClick={onDiscard} className="text-sm text-slate-600">Discard</button>{/* Ownership must still hold when a reviewed draft is applied. */}<button disabled={busy || !canManage} onClick={onApply} className={buttonClass}>{/* An existing attachment is updated, never imported a second time. */}{saved ? "Save changes" : "Import catalog"}</button></div>
  </div>;
}

/** Keeps connection editing and refresh scoped to the service owner and selected catalog. */
function CatalogActions({ saved, canManage, busy, onEdit, onRefresh }: { saved: McpCatalogSnapshot | null; canManage: boolean; busy: boolean; onEdit: () => void; onRefresh: (input: McpDiscoveryInput) => Promise<void> }) {
  // Catalog read access never implies authority to change the service's MCP attachment.
  if (!canManage) return null;
  const actions = [{ label: connectionActionLabel(saved), onSelect: onEdit }];
  // Refresh is meaningful only after a connection has been saved; credentials remain references.
  if (saved) actions.push({ label: "Refresh catalog", onSelect: () => { void onRefresh({ url: saved.url, bucket_name: saved.bucket_name, secret_name: saved.secret_name }); } });
  return <ServiceCatalogOptions actions={actions} disabled={busy} />;
}

/** Collects an endpoint and optional secret reference without asking the browser to handle bearer tokens. */
function ImportForm({ initial, busy, onDiscover, onCancel }: { initial: McpCatalogSnapshot | null; busy: boolean; onDiscover: (input: McpDiscoveryInput) => Promise<void>; onCancel?: () => void }) {
  const [url, setURL] = useState(initial?.url ?? "");
  const [auth, setAuth] = useState(initial?.bucket_name ? "bearer" : "none");
  const [bucket, setBucket] = useState(initial?.bucket_name ?? "default");
  const [secret, setSecret] = useState(initial?.secret_name ?? "");
  return <form className="space-y-4 border-b border-slate-200 bg-slate-50/50 p-5" onSubmit={(event) => {
    // Browser validation admits only complete connection references; Engine performs authoritative validation.
    event.preventDefault();
    void onDiscover({ url, ...(auth === "bearer" ? { bucket_name: bucket, secret_name: secret } : {}) });
  }}>
    {/* The form changes the current service attachment; it never asks for another server identity. */}
    <h3 className="text-sm font-semibold text-slate-900">{connectionActionLabel(initial)}</h3>
    <label className="block space-y-1 text-sm font-medium text-slate-700"><span>Server URL</span><input className={inputClass} required type="url" pattern="https://.*" placeholder="https://example.com/mcp" value={url} disabled={busy} onChange={(event) => setURL(event.target.value)} /></label>
    <p className="text-xs text-slate-500">Remote Streamable HTTP servers over HTTPS are supported.</p>
    <label className="block space-y-1 text-sm font-medium text-slate-700"><span>Authentication</span><Select value={auth} disabled={busy} className={inputClass} onChange={(event) => setAuth(event.target.value)}><option value="none">No authentication</option><option value="bearer">Bearer token from a bucket</option></Select></label>
    {/* Credential references are shown only when the server requires bearer authentication. */}
    {auth === "bearer" && <div className="grid gap-4 sm:grid-cols-2"><label className="space-y-1 text-sm text-slate-700"><span>Bucket name</span><input className={inputClass} required value={bucket} disabled={busy} onChange={(event) => setBucket(event.target.value)} /></label><label className="space-y-1 text-sm text-slate-700"><span>Bucket secret name</span><input className={inputClass} required value={secret} disabled={busy} onChange={(event) => setSecret(event.target.value)} /></label><p className="text-xs text-slate-500 sm:col-span-2">Use an existing generic bucket secret containing the token. Requires credential management and bucket use access. Fused keeps its value in Engine.</p></div>}
    <div className="flex items-center gap-4"><button className={buttonClass} disabled={busy} type="submit">Discover server</button><button type="button" disabled={busy} className="text-sm text-slate-500" onClick={onCancel}>Cancel</button></div>
  </form>;
}

/** Opens MCP metadata using the same row-to-inspector flow as operation endpoints. */
function CatalogResourceRow({ kind, item, onSelect }: { kind: McpCatalogKind; item: McpCatalogItem; onSelect: () => void }) {
  const badge = catalogTypeBadges[kind];
  return <button type="button" onClick={onSelect} className="block w-full px-4 py-4 text-left transition-colors hover:bg-slate-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-slate-950 sm:px-5">
    <span className="mb-1 flex items-start justify-between gap-3 sm:items-center"><span className="flex min-w-0 items-start gap-3 sm:items-center"><span className={`shrink-0 rounded px-1.5 py-0.5 text-xs font-bold ${badge.color}`}>{badge.label}</span><code className="min-w-0 break-all text-sm text-slate-700">{mcpItemKey(kind, item)}</code></span><ChevronDown aria-hidden="true" className="h-4 w-4 shrink-0 -rotate-90 text-slate-400" /></span>
    {/* Long provider copy is bounded in the list and available in full inside the inspector. */}
    {item.description && <span className="mt-1 block line-clamp-3 whitespace-pre-wrap text-xs text-slate-500">{item.description}</span>}
  </button>;
}

/** Mirrors operation resource groups while keeping each MCP capability independently collapsible. */
function CatalogResourceGroup({ kind, label, items, onSelect }: { kind: McpCatalogKind; label: string; items: McpCatalogItem[]; onSelect: (kind: McpCatalogKind, item: McpCatalogItem) => void }) {
  const [expanded, setExpanded] = useState(true);
  return <div>
    <button type="button" aria-expanded={expanded} onClick={() => setExpanded(!expanded)} className="flex w-full items-center gap-2 border-y border-slate-100 bg-slate-50 px-4 py-2.5 text-left text-xs text-slate-600 hover:bg-slate-100 sm:px-5"><ChevronDown aria-hidden="true" className={`h-4 w-4 text-slate-400 ${expanded ? "" : "-rotate-90"}`} /><span className="font-semibold uppercase tracking-wider">{label}</span><span className="text-slate-400">({items.length})</span></button>
    {/* Collapsing a group affects presentation only; its snapshot and filters are preserved. */}
    {expanded && <div className="divide-y divide-slate-100">{items.map((item) => <CatalogResourceRow key={mcpItemKey(kind, item)} kind={kind} item={item} onSelect={() => onSelect(kind, item)} />)}</div>}
  </div>;
}

/** Shows the currently selected declaration and discards selection on a new catalog revision. */
function CatalogGroups({ catalog, query, kind }: { catalog?: McpCatalog; query: string; kind: McpCatalogKind | "all" }) {
  const [selected, setSelected] = useState<{ kind: McpCatalogKind; item: McpCatalogItem } | null>(null);
  // A discovery preview or refresh must never leave the previous definition open as current data.
  useEffect(() => setSelected(null), [catalog]);
  const sections = mcpCatalogSections.filter((section) => kind === "all" || section.kind === kind);
  return <><div aria-label="MCP resources">{sections.map((section) => {
    // Unsupported and empty capabilities do not add empty resource groups to the catalog.
    const items = catalog?.supported[section.kind] ? filterMcpItems(catalog[section.kind], query) : [];
    if (!items.length) return null;
    return <CatalogResourceGroup key={section.kind} kind={section.kind} label={section.label} items={items} onSelect={(selectedKind, item) => setSelected({ kind: selectedKind, item })} />;
  })}</div>{/* Selection is metadata-only; inspecting a resource never contacts its URI. */}{selected && <McpCatalogDetails key={`${selected.kind}:${mcpItemKey(selected.kind, selected.item)}`} kind={selected.kind} item={selected.item} onClose={() => setSelected(null)} />}</>;
}

/** Presents a single endpoint-style list with intersecting metadata and resource-type filters. */
export function CatalogSections({ catalog, actions, children }: { catalog?: McpCatalog; actions?: ReactNode; children?: ReactNode }) {
  const [query, setQuery] = useState("");
  const [kind, setKind] = useState<McpCatalogKind | "all">("all");
  // All types preserve the provider's ordering within each list; a type selection narrows that same catalog.
  const sections = mcpCatalogSections.filter((section) => kind === "all" || section.kind === kind);
  // Unsupported capabilities never contribute rows, even if stale provider data happens to be present.
  const rows = sections.flatMap((section) => catalog?.supported[section.kind]
    ? filterMcpItems(catalog[section.kind], query).map((item) => ({ kind: section.kind, item }))
    : []);
  const unsupported = kind !== "all" && !catalog?.supported[kind];

  return <div className="min-w-0 rounded-xl border border-slate-200 bg-white">
    <ServiceCatalogHeader title="Explore MCP resources" description="Browse tools, prompts, and resources available for this service.">{actions}</ServiceCatalogHeader>
    <div className="flex flex-wrap items-center justify-between gap-3 border-b border-slate-100 px-4 py-3 sm:px-5">
      <div className="relative w-full min-w-0 sm:w-auto sm:min-w-[250px] sm:flex-1">
        <Search aria-hidden="true" className="absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-slate-400" />
        <input aria-label="Search MCP catalog" className="w-full rounded-md border border-slate-300 py-1.5 pl-9 pr-8 text-sm focus:border-slate-500 focus:outline-none focus:ring-1 focus:ring-gray-500" placeholder="Search tools, prompts, resources…" value={query} onChange={(event) => setQuery(event.target.value)} />
        {/* Clearing search retains the selected type so the two filters remain independent. */}
        {query && <button type="button" aria-label="Clear MCP search" className="absolute right-2.5 top-1/2 -translate-y-1/2 text-slate-400 hover:text-slate-600" onClick={() => setQuery("")}>✕</button>}
      </div>
      <Select aria-label="MCP resource type" density="compact" value={kind} onChange={(event) => setKind(event.target.value as McpCatalogKind | "all")} className="w-full text-xs text-slate-600 sm:w-auto">
        <option value="all">All types</option>
        {/* Counts refer to the full catalog, so a search never hides which types the server offers. */}
        {mcpCatalogSections.map((section) => <option key={section.kind} value={section.kind}>{section.label} ({catalog?.[section.kind].length ?? 0})</option>)}
      </Select>
      <span role="status" className="text-xs text-slate-400">{rows.length} shown</span>
    </div>
    {children}
    <div className="divide-y divide-slate-100">
      <CatalogGroups catalog={catalog} query={query} kind={kind} />
      {/* A missing capability needs different guidance from an empty catalog or a search miss. */}
      {catalog && rows.length === 0 && <p className="p-8 text-center text-sm text-slate-500">{unsupported ? "This resource type is not supported by this server." : query.trim() ? "No matching items." : "This server returned no items."}</p>}
    </div>
  </div>;
}
