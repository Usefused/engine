import { useState, useEffect } from "react";
import { Link, type MetaFunction } from "@remix-run/react";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m) => m.id === "root").flatMap((m) => m.meta ?? []);
  return [
    ...parentMeta.filter((m) => !('title' in m)),
    { title: "MCP servers - Fused" },
  ];
};
import { Trash2, Copy, TerminalSquare, AlertCircle, Play, ServerCrash, BarChart2, Info } from "lucide-react";
import { api } from "~/lib/api";
import { useToast } from "~/components/Toast";

interface McpServerItem {
  id: string;
  name: string;
  active: boolean;
  mcp_url?: string;
  deactivated_at?: string;
  created_at?: string;
}

interface McpServerCardProps {
  server: McpServerItem;
  isSelected: boolean;
  anySelected: boolean;
  onToggleSelect: (id: string) => void;
  onDelete: (id: string, name: string) => void;
  onKill: (id: string, name: string) => void;
  onReactivate: (id: string) => void;
  onCopyUrl: (url: string) => void;
}

function McpServerStatusBadge({ active }: { active: boolean }) {
  if (!active) {
    return <span className="flex items-center gap-1.5 text-xs font-medium px-2 py-0.5 rounded-full text-slate-500 bg-slate-100">Killed</span>;
  }
  return (
    <span className="flex items-center gap-1.5 text-xs font-medium px-2 py-0.5 rounded-full text-emerald-600 bg-emerald-50">
      <span className="w-1.5 h-1.5 rounded-full bg-emerald-500 animate-pulse"></span>
      Running
    </span>
  );
}

function McpConnectionUrlRow({ server, onCopyUrl }: { server: McpServerItem; onCopyUrl: (url: string) => void }) {
  if (!server.active) {
    return (
      <code className="flex-1 bg-white px-3 py-2 rounded-lg border border-slate-200 text-xs font-mono text-slate-800 break-all shadow-sm">
        <span className="text-slate-400 italic">Server killed -- reactivate to reconnect</span>
      </code>
    );
  }
  return (
    <>
      <code className="flex-1 bg-white px-3 py-2 rounded-lg border border-slate-200 text-xs font-mono text-slate-800 break-all shadow-sm">
        {server.mcp_url}
      </code>
      <button
        data-track="copy_mcp_sandbox_url"
        onClick={() => onCopyUrl(server.mcp_url || "")}
        className="p-2 bg-white border border-slate-200 text-slate-600 rounded-lg hover:bg-slate-50 hover:text-slate-900 transition-colors shadow-sm cursor-pointer"
        title="Copy URL"
      >
        <Copy className="w-4 h-4" />
      </button>
    </>
  );
}

// Kill/Reactivate toggle, not Restart -- Kill just flips `active` (deployment
// and its selections are untouched), so "restarting" is the same server
// coming back, not a redeploy. There's also no token-regenerate button here
// anymore: the Engine schema has no mutation for it, since the scope's auth
// token is tied to the deploy itself.
function McpKillReactivateButton({ server, onKill, onReactivate }: { server: McpServerItem; onKill: (id: string, name: string) => void; onReactivate: (id: string) => void }) {
  if (server.active) {
    return (
      <button
        data-track="kill_mcp_server"
        onClick={() => onKill(server.id, server.name)}
        className="inline-flex items-center gap-1.5 px-3 py-1.5 bg-amber-50 text-amber-600 hover:bg-amber-100 hover:text-amber-700 rounded-lg text-xs font-semibold transition-colors border border-amber-100 cursor-pointer"
      >
        <ServerCrash className="w-3.5 h-3.5" />
        Kill
      </button>
    );
  }
  return (
    <button
      data-track="reactivate_mcp_server"
      onClick={() => onReactivate(server.id)}
      className="inline-flex items-center gap-1.5 px-3 py-1.5 bg-emerald-50 text-emerald-600 hover:bg-emerald-100 hover:text-emerald-700 rounded-lg text-xs font-semibold transition-colors border border-emerald-100 cursor-pointer"
    >
      <Play className="w-3.5 h-3.5 fill-current" />
      Reactivate
    </button>
  );
}

function McpServerCard({ server, isSelected, anySelected, onToggleSelect, onDelete, onKill, onReactivate, onCopyUrl }: McpServerCardProps) {
  return (
    <div className={`group bg-white border border-slate-200 rounded-xl overflow-hidden shadow-sm transition-shadow ${!server.active ? 'opacity-75' : 'hover:shadow-md'}`}>
      <div className="p-5 border-b border-slate-100 flex items-start justify-between">
        <div className="flex items-start gap-3">
          <div className={`pt-2 transition-opacity duration-200 ${anySelected || isSelected ? 'opacity-100' : 'opacity-0 group-hover:opacity-100 focus-within:opacity-100'}`}>
            <input
              type="checkbox"
              className="rounded border-slate-300 text-blue-600 focus:ring-blue-500 w-4 h-4 cursor-pointer"
              checked={isSelected}
              onChange={() => onToggleSelect(server.id)}
            />
          </div>
          <div className={`w-10 h-10 rounded-lg border flex items-center justify-center shrink-0 ${server.active ? 'bg-slate-100 border-slate-200' : 'bg-slate-50 border-slate-200'}`}>
            <TerminalSquare className={`w-5 h-5 ${server.active ? 'text-slate-700' : 'text-slate-400'}`} />
          </div>
          <div>
            <h3 className="font-semibold text-slate-900 leading-tight">{server.name || "Untitled MCP server"}</h3>
            <div className="flex items-center gap-2 mt-1">
              <McpServerStatusBadge active={server.active} />
            </div>
          </div>
        </div>
        <button
          data-track="delete_mcp_server"
          onClick={() => onDelete(server.id, server.name)}
          className="p-1.5 text-slate-400 hover:text-rose-600 hover:bg-rose-50 rounded-lg transition-colors cursor-pointer"
          title="Delete server"
        >
          <Trash2 className="w-4 h-4" />
        </button>
      </div>

      <div className="p-5 bg-slate-50/50 space-y-4">
        <div>
          <label className="text-xs font-semibold text-slate-500 uppercase tracking-wider mb-2 block">Connection URL</label>
          <div className="flex items-center gap-2">
            <McpConnectionUrlRow server={server} onCopyUrl={onCopyUrl} />
          </div>
        </div>

        <div className="flex items-center justify-between pt-2">
          <span className="text-xs text-slate-400 font-medium">
            Created {server.created_at ? new Date(server.created_at).toLocaleDateString() : ""}
          </span>
          <div className="flex items-center gap-2">
            <McpKillReactivateButton server={server} onKill={onKill} onReactivate={onReactivate} />
            <Link
              to={`/integrations/mcp/${server.id}/analytics`}
              className="inline-flex items-center gap-1.5 px-3 py-1.5 bg-slate-100 text-slate-700 hover:bg-slate-200 hover:text-slate-900 rounded-lg text-xs font-semibold transition-colors border border-slate-200 cursor-pointer"
            >
              <BarChart2 className="w-3.5 h-3.5" />
              Analytics
            </Link>
          </div>
        </div>
      </div>
    </div>
  );
}

function McpConnectionGuide({ show }: { show: boolean }) {
  if (!show) return null;
  return (
    <div className="p-4 bg-slate-50 border border-slate-200 rounded-xl animate-in fade-in slide-in-from-top-2">
      <h4 className="text-sm font-semibold text-slate-900 mb-2 flex items-center gap-1.5">
        <AlertCircle className="w-4 h-4" />
        Connect an MCP client
      </h4>
      <div className="space-y-2 text-sm text-slate-700 leading-relaxed">
        <p>
          Use the server's <strong>Connection URL</strong> and send its execution token in the authorization header:
        </p>
        <code className="inline-block max-w-full break-all rounded bg-slate-200/70 px-2 py-1 font-mono text-xs text-slate-900">
          Authorization: Bearer &lt;execution-token&gt;
        </code>
        <p>
          Service credentials come from the credential set selected when the server was created, so your MCP client never needs to send provider keys. The execution token is shown once at creation and should be stored securely.
        </p>
      </div>
    </div>
  );
}

function DeleteSelectedButton({ count, isDeleting, onClick }: { count: number; isDeleting: boolean; onClick: () => void }) {
  if (count === 0) return null;
  return (
    <button
      onClick={onClick}
      disabled={isDeleting}
      className="inline-flex items-center gap-2 px-4 py-2 bg-rose-50 text-rose-600 hover:bg-rose-100 text-sm font-medium rounded-lg transition-all border border-rose-200 self-start sm:self-auto cursor-pointer"
    >
      <Trash2 className="w-4 h-4" />
      {isDeleting ? "Deleting..." : `Delete Selected (${count})`}
    </button>
  );
}

function SelectAllRow({ selectedCount, allSelected, onToggle }: { selectedCount: number; allSelected: boolean; onToggle: () => void }) {
  if (selectedCount === 0) return null;
  return (
    <div className="flex items-center animate-in fade-in slide-in-from-top-2 duration-200">
      <label className="flex items-center gap-2 text-sm text-slate-600 cursor-pointer bg-blue-50 px-3 py-1.5 rounded-lg border border-blue-100">
        <input
          type="checkbox"
          className="rounded border-blue-300 text-blue-600 focus:ring-blue-500 w-4 h-4 cursor-pointer"
          checked={allSelected}
          onChange={onToggle}
        />
        <span className="font-medium text-blue-800">Select All on Page</span>
      </label>
    </div>
  );
}

function McpPagination({ page, limit, total, onPrevious, onNext }: { page: number; limit: number; total: number; onPrevious: () => void; onNext: () => void }) {
  if (total <= limit) return null;
  return (
    <div className="flex items-center justify-between px-6 py-4 bg-white border border-slate-200 rounded-xl">
      <span className="text-sm text-slate-500">
        Showing <span className="font-medium">{(page - 1) * limit + 1}</span> to <span className="font-medium">{Math.min(page * limit, total)}</span> of <span className="font-medium">{total}</span>
      </span>
      <div className="flex gap-2">
        <button
          data-track="paginate_previous"
          onClick={onPrevious}
          disabled={page === 1}
          className="px-3 py-1.5 text-sm font-medium border border-slate-200 rounded-md disabled:opacity-50 disabled:cursor-not-allowed hover:bg-slate-50 transition-colors"
        >
          Previous
        </button>
        <button
          data-track="paginate_next"
          onClick={onNext}
          disabled={page * limit >= total}
          className="px-3 py-1.5 text-sm font-medium border border-slate-200 rounded-md disabled:opacity-50 disabled:cursor-not-allowed hover:bg-slate-50 transition-colors"
        >
          Next
        </button>
      </div>
    </div>
  );
}

export default function McpServers() {
  const toast = useToast();
  const [mcpServers, setMcpServers] = useState<McpServerItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [page, setPage] = useState(1);
  const [total, setTotal] = useState(0);
  const [showHowToUse, setShowHowToUse] = useState(false);
  const [selectedIds, setSelectedIds] = useState<string[]>([]);
  const [isDeletingMultiple, setIsDeletingMultiple] = useState(false);
  const limit = 10;

  // mcpServers/killMcpServer/reactivateMcpServer/deleteMcpServer all live on
  // the Engine's own MCP GraphQL schema (internal/engine/api/mcp_graphql.go),
  // not the Registry-proxied api.graphql this page used to call -- MCP
  // creation no longer generates a Registry SDK row for these servers.
  const fetchServers = () => {
    setLoading(true);
    const queryStr = `
      query {
        mcpServers(limit: ${limit}, offset: ${(page - 1) * limit}) {
          items {
            id
            name
            active
            mcp_url
            deactivated_at
            created_at
          }
          total
        }
      }
    `;
    api.mcpGraphql<{ mcpServers: { items: McpServerItem[], total: number } }>(queryStr)
      .then(res => {
        setMcpServers(res.mcpServers.items);
        setTotal(res.mcpServers.total);
      })
      .catch(e => setError(e instanceof Error ? e.message : "Failed to load MCP servers"))
      .finally(() => setLoading(false));
  };

  useEffect(() => {
    fetchServers();
    setSelectedIds([]);
  }, [page]);

  const handleKill = async (id: string, name: string) => {
    const confirmed = await toast.confirm(`Are you sure you want to kill the MCP server "${name}"? This will terminate any active connections immediately and block new ones until you reactivate it.`);
    if (!confirmed) return;
    try {
      await api.mcpGraphql(`mutation { killMcpServer(id: "${id}") { id } }`);
      fetchServers();
      toast.success(`Server "${name}" killed successfully.`);
    } catch (err) {
      toast.error(`Failed to kill server: ${err instanceof Error ? err.message : "Unknown error"}`);
    }
  };

  const handleDelete = async (id: string, name: string) => {
    const confirmed = await toast.confirm(`Are you sure you want to completely delete the MCP server "${name}"? This cannot be undone.`);
    if (!confirmed) return;
    try {
      await api.mcpGraphql(`mutation { deleteMcpServer(id: "${id}") }`);
      fetchServers();
      toast.success(`Server "${name}" deleted successfully.`);
      setSelectedIds(prev => prev.filter(i => i !== id));
    } catch (err) {
      toast.error(`Failed to delete server: ${err instanceof Error ? err.message : "Unknown error"}`);
    }
  };

  const handleDeleteMultiple = async () => {
    if (selectedIds.length === 0) return;
    const confirmed = await toast.confirm(`Are you sure you want to completely delete ${selectedIds.length} MCP server(s)? This cannot be undone.`);
    if (!confirmed) return;

    setIsDeletingMultiple(true);
    try {
      await Promise.all(selectedIds.map(id => api.mcpGraphql(`mutation { deleteMcpServer(id: "${id}") }`)));
      fetchServers();
      setSelectedIds([]);
      toast.success(`Successfully deleted ${selectedIds.length} server(s).`);
    } catch (err) {
      toast.error(`Failed to delete some servers: ${err instanceof Error ? err.message : "Unknown error"}`);
      fetchServers();
    } finally {
      setIsDeletingMultiple(false);
    }
  };

  const handleReactivate = async (id: string) => {
    try {
      await api.mcpGraphql(`mutation { reactivateMcpServer(id: "${id}") { id } }`);
      fetchServers();
      toast.success("Server reactivated successfully.");
    } catch (err) {
      toast.error(`Failed to reactivate server: ${err instanceof Error ? err.message : "Unknown error"}`);
    }
  };

  if (loading) return <div className="text-center py-12 text-slate-500">Loading MCP servers...</div>;
  if (error) return <div className="text-center py-12 text-red-500">Error: {error}</div>;

  return (
    <div className="space-y-6 animate-in fade-in slide-in-from-bottom-4 duration-500">
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5">
            <h1 className="text-xl font-semibold text-slate-900">MCP servers</h1>
            <button
              data-track="toggle_mcp_connection_guide"
              onClick={() => setShowHowToUse(!showHowToUse)}
              className={`inline-flex items-center gap-1.5 text-xs font-semibold transition-colors cursor-pointer ${showHowToUse ? 'text-slate-800 underline' : 'text-slate-500 hover:text-slate-800 hover:underline'}`}
            >
              <Info className="w-3.5 h-3.5 shrink-0" />
              How to connect
            </button>
          </div>
          <p className="text-sm text-slate-500 mt-1">Manage the MCP servers your agents connect to.</p>
        </div>
        <div className="flex items-center gap-3">
          <DeleteSelectedButton count={selectedIds.length} isDeleting={isDeletingMultiple} onClick={handleDeleteMultiple} />
          <Link
            to="/integrations/builder?tab=mcp"
            className="inline-flex items-center gap-2 px-4 py-2 bg-slate-950 text-white text-sm font-medium rounded-lg hover:bg-slate-800 transition-colors shadow-sm self-start sm:self-auto"
          >
            <Play className="w-4 h-4" />
            Create MCP server
          </Link>
        </div>
      </div>

      <McpConnectionGuide show={showHowToUse} />

      {mcpServers.length === 0 ? (
        <div className="bg-white border border-slate-200 rounded-xl p-12 text-center">
          <TerminalSquare className="w-12 h-12 text-slate-300 mx-auto mb-4" />
          <h3 className="text-lg font-medium text-slate-900">No MCP servers yet</h3>
          <p className="text-slate-500 mt-2 mb-6 max-w-md mx-auto">
            You haven't created an MCP server yet. Create one to expose selected services and operations to your agents.
          </p>
        </div>
      ) : (
        <div className="space-y-4">
          <SelectAllRow
            selectedCount={selectedIds.length}
            allSelected={selectedIds.length === mcpServers.length && mcpServers.length > 0}
            onToggle={() => setSelectedIds(prev => prev.length === mcpServers.length ? [] : mcpServers.map(s => s.id))}
          />
          <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
          {mcpServers.map(server => (
            <McpServerCard
              key={server.id}
              server={server}
              isSelected={selectedIds.includes(server.id)}
              anySelected={selectedIds.length > 0}
              onToggleSelect={(id) => setSelectedIds(prev => prev.includes(id) ? prev.filter(i => i !== id) : [...prev, id])}
              onDelete={handleDelete}
              onKill={handleKill}
              onReactivate={handleReactivate}
              onCopyUrl={(url) => { navigator.clipboard.writeText(url); toast.success("URL copied to clipboard!"); }}
            />
          ))}
        </div>
        </div>
      )}

      <McpPagination
        page={page}
        limit={limit}
        total={total}
        onPrevious={() => setPage(p => Math.max(1, p - 1))}
        onNext={() => setPage(p => p + 1)}
      />
    </div>
  );
}
