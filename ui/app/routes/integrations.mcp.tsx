import { useEffect, useState } from "react";
import { Link, useNavigate, type MetaFunction } from "@remix-run/react";
import { AlertCircle, Info, Loader2, Play, Search, ServerCrash, TerminalSquare, Trash2, X } from "lucide-react";
import { AppRuntimeStatus } from "~/components/apps/AppRuntimeStatus";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { useToast } from "~/components/Toast";
import { api } from "~/lib/api";
import { hasResourcePermission, hasWorkspacePermission } from "~/lib/current-actor-access";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((match) => match.id === "root").flatMap((match) => match.meta ?? []);
  return [...parentMeta.filter((item) => !("title" in item)), { title: "MCP servers - Fused" }];
};

interface McpServerItem {
  app_id: string;
  app_family_id: string;
  name: string;
  description?: string;
  version: string;
  status: string;
  created_at?: string;
}

type McpPage = { items: McpServerItem[]; total: number };

const MCP_PAGE_SIZE = 20;

/** Splits an optional version suffix while preserving scoped names such as @provider/app. */
function mcpSearchParts(query: string): { search: string; version: string } {
  const trimmed = query.trim();
  const atIndex = trimmed.lastIndexOf("@");
  if (atIndex <= 0) return { search: trimmed, version: "" };
  return { search: trimmed.slice(0, atIndex), version: trimmed.slice(atIndex + 1) };
}

/** Reads one authorized MCP catalogue page through the shared app lifecycle query. */
function readMcpPage(query: string, page: number): Promise<McpPage> {
  const { search, version } = mcpSearchParts(query);
  const document = `
    query MCPApps($search: String!, $version: String!, $limit: Int!, $offset: Int!) {
      apps(kind: "mcp", search: $search, version: $version, limit: $limit, offset: $offset) {
        items { app_id app_family_id name description version status created_at }
        total
      }
    }
  `;
  return api.mcpGraphql<{ apps: McpPage }>(document, {
    search,
    version,
    limit: MCP_PAGE_SIZE,
    offset: page * MCP_PAGE_SIZE,
  }).then(({ apps }) => apps);
}

function McpConnectionGuide({ open }: { open: boolean }) {
  if (!open) return null;
  return (
    <div className="rounded-xl border border-slate-200 bg-slate-50 p-4 animate-in fade-in slide-in-from-top-2">
      <h2 className="flex items-center gap-1.5 text-sm font-semibold text-slate-900"><AlertCircle className="h-4 w-4" />Connect an MCP client</h2>
      <p className="mt-2 text-sm leading-relaxed text-slate-700">
        Open a server to copy its recommended Streamable HTTP endpoint. Authenticate requests with
        <code className="mx-1 rounded bg-slate-200/70 px-1.5 py-0.5 font-mono text-xs">Authorization: Bearer &lt;execution-token&gt;</code>.
      </p>
    </div>
  );
}

function McpVersion({ version }: { version: string }) {
  return <span className="inline-flex rounded border border-slate-200 bg-slate-100 px-2 py-0.5 text-xs font-medium text-slate-700">{version}</span>;
}

interface McpRowProps {
  server: McpServerItem;
  selected: boolean;
  anySelected: boolean;
  canManage: boolean;
  onSelect: (id: string) => void;
  onNavigate: (id: string) => void;
  onDeprecate: (server: McpServerItem) => void;
  onRestore: (server: McpServerItem) => void;
  onDeactivate: (server: McpServerItem) => void;
}

/** Renders one immutable MCP version as a navigable row with isolated lifecycle actions. */
function McpRow({ server, selected, anySelected, canManage, onSelect, onNavigate, onDeprecate, onRestore, onDeactivate }: McpRowProps) {
  const showCheckbox = canManage && (anySelected || selected);
  return (
    <tr className="group cursor-pointer transition-colors hover:bg-slate-50/70" onClick={() => onNavigate(server.app_id)}>
      <td className="min-w-0 px-3 py-4 sm:px-6">
        <div className="flex min-w-0 items-center gap-3">
          <div className="relative h-8 w-8 shrink-0 rounded">
            {canManage ? (
              <div className={`absolute inset-0 z-10 flex items-center justify-center rounded bg-white/90 transition-opacity ${showCheckbox ? "opacity-100" : "opacity-0 group-hover:opacity-100 focus-within:opacity-100"}`} onClick={(event) => event.stopPropagation()}>
                <input aria-label={`Select ${server.name}`} type="checkbox" checked={selected} onChange={() => onSelect(server.app_id)} className="h-4 w-4 cursor-pointer rounded border-slate-300 text-blue-600 focus:ring-blue-500" />
              </div>
            ) : null}
            <div className="absolute inset-0 flex items-center justify-center rounded bg-violet-100 text-violet-700"><TerminalSquare className="h-4 w-4" /></div>
          </div>
          <div className="min-w-0">
            <div className="truncate font-semibold text-slate-900">{server.name || "Untitled MCP server"}</div>
            <AppRuntimeStatus className="mt-0.5" status={server.status} />
          </div>
        </div>
      </td>
      <td className="px-2 py-4 sm:px-6"><McpVersion version={server.version} /></td>
      <td className="hidden px-6 py-4 text-slate-500 lg:table-cell">{server.created_at ? new Date(server.created_at).toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" }) : ""}</td>
      <td className="px-2 py-4 text-right sm:px-6">{canManage ? <McpRowActions server={server} onDeprecate={onDeprecate} onRestore={onRestore} onDeactivate={onDeactivate} /> : null}</td>
    </tr>
  );
}

function McpRowActions({ server, onDeprecate, onRestore, onDeactivate }: Pick<McpRowProps, "server" | "onDeprecate" | "onRestore" | "onDeactivate">) {
  return (
    <div className="flex justify-end gap-1 sm:gap-2" onClick={(event) => event.stopPropagation()}>
      {server.status === "active" ? (
        <button type="button" onClick={() => onDeprecate(server)} className="inline-flex h-8 w-8 items-center justify-center rounded-lg bg-amber-50 text-amber-700 hover:bg-amber-100" title="Deprecate MCP server"><ServerCrash className="h-4 w-4" /></button>
      ) : server.status === "deprecated" ? (
        <button type="button" onClick={() => onRestore(server)} className="inline-flex h-8 w-8 items-center justify-center rounded-lg bg-emerald-50 text-emerald-700 hover:bg-emerald-100" title="Restore MCP server"><Play className="h-4 w-4" /></button>
      ) : null}
      <button type="button" onClick={() => onDeactivate(server)} className="inline-flex h-8 w-8 items-center justify-center rounded-lg bg-rose-50 text-rose-600 hover:bg-rose-100" title="Deactivate MCP server"><Trash2 className="h-4 w-4" /></button>
    </div>
  );
}

function McpPagination({ page, total, onPage }: { page: number; total: number; onPage: (page: number) => void }) {
  const pageCount = Math.max(1, Math.ceil(total / MCP_PAGE_SIZE));
  if (pageCount === 1) return null;
  return (
    <div className="flex items-center justify-between gap-4 text-sm text-slate-600">
      <span>Page {page + 1} of {pageCount}</span>
      <div className="flex gap-2">
        <button type="button" disabled={page === 0} onClick={() => onPage(page - 1)} className="rounded-lg border border-slate-300 px-3 py-1.5 disabled:opacity-40">Previous</button>
        <button type="button" disabled={page + 1 >= pageCount} onClick={() => onPage(page + 1)} className="rounded-lg border border-slate-300 px-3 py-1.5 disabled:opacity-40">Next</button>
      </div>
    </div>
  );
}

/** Lists MCP servers using the same searchable app catalogue pattern as SDKs. */
export default function McpServers() {
  const toast = useToast();
  const navigate = useNavigate();
  const { access } = useCurrentActorAccess();
  const [servers, setServers] = useState<McpServerItem[]>([]);
  const [query, setQuery] = useState("");
  const [page, setPage] = useState(0);
  const [total, setTotal] = useState(0);
  const [selectedIds, setSelectedIds] = useState<string[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [showGuide, setShowGuide] = useState(false);
  const [deactivating, setDeactivating] = useState(false);
  const canCreate = hasWorkspacePermission(access, "app.create");

  const loadServers = () => {
    setLoading(true);
    setError("");
    readMcpPage(query, page)
      .then((result) => { setServers(result.items); setTotal(result.total); })
      .catch((cause) => setError(cause instanceof Error ? cause.message : "Failed to load MCP servers"))
      .finally(() => setLoading(false));
  };

  useEffect(() => {
    const timeout = window.setTimeout(loadServers, 200);
    return () => window.clearTimeout(timeout);
  }, [query, page]);

  const mutateServer = async (server: McpServerItem, action: "deprecate" | "restore" | "deactivate") => {
    const label = action === "deprecate" ? "Deprecate" : action === "restore" ? "Restore" : "Deactivate";
    const confirmed = await toast.confirm(`${label} MCP server "${server.name}"?`);
    if (!confirmed) return;
    const document = action === "deprecate"
      ? `mutation($appId: String!, $message: String!) { deprecateApp(app_id: $appId, message: $message) }`
      : action === "restore"
        ? `mutation($appId: String!) { undeprecateApp(app_id: $appId) }`
        : `mutation($appId: String!) { deactivateApp(app_id: $appId) }`;
    try {
      await api.mcpGraphql(document, { appId: server.app_id, message: "A newer MCP app version is available" });
      toast.success(`${server.name} ${action === "restore" ? "restored" : `${action}d`}.`);
      setSelectedIds((current) => current.filter((id) => id !== server.app_id));
      loadServers();
    } catch (cause) {
      toast.error(`${label} failed: ${cause instanceof Error ? cause.message : "Unknown error"}`);
    }
  };

  const deactivateSelected = async () => {
    if (selectedIds.length === 0) return;
    const confirmed = await toast.confirm(`Deactivate ${selectedIds.length} MCP server version(s)?`);
    if (!confirmed) return;
    setDeactivating(true);
    try {
      await Promise.all(selectedIds.map((appId) => api.mcpGraphql(`mutation($appId: String!) { deactivateApp(app_id: $appId) }`, { appId })));
      toast.success(`Deactivated ${selectedIds.length} MCP server version(s).`);
      setSelectedIds([]);
      loadServers();
    } catch (cause) {
      toast.error(`Deactivation failed: ${cause instanceof Error ? cause.message : "Unknown error"}`);
    } finally {
      setDeactivating(false);
    }
  };

  const manageableIds = servers.filter((server) => hasResourcePermission(access, "app.manage", "APP", server.app_family_id)).map((server) => server.app_id);

  return (
    <div className="space-y-6 animate-in fade-in slide-in-from-bottom-4 duration-500">
      <div className="flex flex-col justify-between gap-4 sm:flex-row sm:items-center">
        <div>
          <div className="flex flex-wrap items-center gap-3">
            <h1 className="text-xl font-semibold text-slate-900">MCP servers</h1>
            <button type="button" onClick={() => setShowGuide((open) => !open)} className="inline-flex items-center gap-1.5 text-xs font-semibold text-slate-500 hover:text-slate-800 hover:underline"><Info className="h-3.5 w-3.5" />How to connect</button>
          </div>
          <p className="mt-1 text-sm text-slate-500">Manage the MCP servers your agents connect to.</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {selectedIds.length > 0 ? <button type="button" disabled={deactivating} onClick={deactivateSelected} className="inline-flex items-center gap-2 rounded-lg border border-rose-200 bg-rose-50 px-4 py-2 text-sm font-medium text-rose-600 disabled:opacity-50"><Trash2 className="h-4 w-4" />{deactivating ? "Deactivating..." : `Deactivate selected (${selectedIds.length})`}</button> : null}
          {canCreate ? <Link to="/integrations/builder?tab=mcp" className="inline-flex items-center gap-2 rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white shadow-sm hover:bg-slate-800"><Play className="h-4 w-4" />Create MCP server</Link> : null}
        </div>
      </div>

      <McpConnectionGuide open={showGuide} />

      <div className="relative max-w-xl">
        <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-slate-400" />
        <input value={query} onChange={(event) => { setQuery(event.target.value); setPage(0); }} placeholder="Search MCP servers or versions" className="w-full rounded-lg border border-slate-200 bg-white py-2 pl-9 pr-9 text-sm outline-none focus:border-blue-500 focus:ring-2 focus:ring-blue-100" />
        {query ? <button type="button" onClick={() => setQuery("")} aria-label="Clear search" className="absolute right-3 top-1/2 -translate-y-1/2 text-slate-400 hover:text-slate-700"><X className="h-4 w-4" /></button> : null}
      </div>

      {error ? <div className="rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">{error}</div> : null}
      {loading ? (
        <div className="flex flex-col items-center justify-center py-20 text-slate-500"><Loader2 className="mb-4 h-8 w-8 animate-spin text-blue-500" />Loading MCP servers...</div>
      ) : servers.length === 0 ? (
        <div className="rounded-xl border border-slate-200 bg-white p-12 text-center"><TerminalSquare className="mx-auto mb-4 h-12 w-12 text-slate-300" /><h2 className="text-lg font-medium text-slate-900">{query ? "No MCP servers found" : "No MCP servers yet"}</h2><p className="mt-2 text-sm text-slate-500">{query ? "No server matches your search." : "Create a server to expose selected services and operations to agents."}</p></div>
      ) : (
        <div className="overflow-x-auto rounded-xl border border-slate-200 bg-white">
          <table className="w-full table-fixed whitespace-nowrap text-left text-sm md:table-auto">
            <thead className="border-b border-slate-200 bg-slate-50 text-slate-500"><tr><th className="w-[55%] px-3 py-4 font-medium sm:px-6 md:w-auto">Server</th><th className="px-2 py-4 font-medium sm:px-6">Version</th><th className="hidden px-6 py-4 font-medium lg:table-cell">Created</th><th className="px-2 py-4 text-right font-medium sm:px-6">Actions</th></tr></thead>
            <tbody className="divide-y divide-slate-100">
              {servers.map((server) => <McpRow key={server.app_id} server={server} selected={selectedIds.includes(server.app_id)} anySelected={selectedIds.length > 0} canManage={manageableIds.includes(server.app_id)} onSelect={(id) => setSelectedIds((current) => current.includes(id) ? current.filter((item) => item !== id) : [...current, id])} onNavigate={(id) => navigate(`/integrations/mcp/${id}`)} onDeprecate={(item) => mutateServer(item, "deprecate")} onRestore={(item) => mutateServer(item, "restore")} onDeactivate={(item) => mutateServer(item, "deactivate")} />)}
            </tbody>
          </table>
        </div>
      )}

      <McpPagination page={page} total={total} onPage={setPage} />
    </div>
  );
}
