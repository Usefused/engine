import { useState, useEffect } from "react";
import { Link, type MetaFunction } from "@remix-run/react";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m: any) => m.id === "root").flatMap((m: any) => m.meta ?? []);
  return [
    ...parentMeta.filter((m: any) => !('title' in m)),
    { title: "MCP Servers - Fused" },
  ];
};
import { Trash2, Copy, TerminalSquare, AlertCircle, Play, ServerCrash, BarChart2, Info } from "lucide-react";
import { api } from "~/lib/api";
import { useToast } from "~/components/Toast";

export default function McpServers() {
  const toast = useToast();
  const [mcpServers, setMcpServers] = useState<any[]>([]);
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
    api.mcpGraphql<{ mcpServers: { items: any[], total: number } }>(queryStr)
      .then(res => {
        setMcpServers(res.mcpServers.items);
        setTotal(res.mcpServers.total);
      })
      .catch(e => setError(e.message))
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
    } catch (err: any) {
      toast.error(`Failed to kill server: ${err.message || "Unknown error"}`);
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
    } catch (err: any) {
      toast.error(`Failed to delete server: ${err.message || "Unknown error"}`);
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
    } catch (err: any) {
      toast.error(`Failed to delete some servers: ${err.message || "Unknown error"}`);
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
    } catch (err: any) {
      toast.error(`Failed to reactivate server: ${err.message || "Unknown error"}`);
    }
  };

  if (loading) return <div className="text-center py-12 text-slate-500">Loading MCP servers...</div>;
  if (error) return <div className="text-center py-12 text-red-500">Error: {error}</div>;

  return (
    <div className="space-y-6 animate-in fade-in slide-in-from-bottom-4 duration-500">
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5">
            <h1 className="text-xl font-semibold text-slate-900">MCP Servers</h1>
            <button
              data-track="toggle_mcp_connection_guide"
              onClick={() => setShowHowToUse(!showHowToUse)}
              className={`inline-flex items-center gap-1.5 text-xs font-semibold transition-colors cursor-pointer ${showHowToUse ? 'text-indigo-800 underline' : 'text-indigo-600 hover:text-indigo-800 hover:underline'}`}
            >
              <Info className="w-3.5 h-3.5 shrink-0" />
              How to connect
            </button>
          </div>
          <p className="text-sm text-slate-500 mt-1">Manage your deployed Model Context Protocol servers.</p>
        </div>
        <div className="flex items-center gap-3">
          {selectedIds.length > 0 && (
            <button
              onClick={handleDeleteMultiple}
              disabled={isDeletingMultiple}
              className="inline-flex items-center gap-2 px-4 py-2 bg-rose-50 text-rose-600 hover:bg-rose-100 text-sm font-medium rounded-lg transition-all border border-rose-200 self-start sm:self-auto cursor-pointer"
            >
              <Trash2 className="w-4 h-4" />
              {isDeletingMultiple ? "Deleting..." : `Delete Selected (${selectedIds.length})`}
            </button>
          )}
          <Link
            to="/integrations/sdk-builder?tab=mcp"
            className="inline-flex items-center gap-2 px-4 py-2 bg-blue-600 text-white text-sm font-medium rounded-lg hover:bg-blue-700 transition-all shadow-sm shadow-blue-200 self-start sm:self-auto"
          >
            <Play className="w-4 h-4" />
            Deploy
          </Link>
        </div>
      </div>

      {showHowToUse && (
        <div className="p-4 bg-indigo-50 border border-indigo-100 rounded-xl animate-in fade-in slide-in-from-top-2">
          <h4 className="text-sm font-semibold text-indigo-900 mb-2 flex items-center gap-1.5">
            <AlertCircle className="w-4 h-4" />
            Client Configuration Guide
          </h4>
          <p className="text-sm text-indigo-700 leading-relaxed">
            To securely connect your MCP client (like your agent or Claude Desktop) to these servers, use the SSE <strong>Connection URL</strong> provided for the MCP server.
            You <strong>must</strong> pass your required API credentials as HTTP headers prefixed with <code className="bg-indigo-100/70 px-1.5 py-0.5 rounded text-indigo-900 font-mono text-xs">X-Env-</code> during the connection handshake.
            <br/><br/>
            For example: <code className="bg-indigo-100/70 px-1.5 py-0.5 rounded text-indigo-900 font-mono text-xs">X-Env-STRIPE_KEY: sk_test_...</code>
            <br/><br/>
            The server will automatically and securely inject these into the sandbox environment without the LLM ever needing to handle the raw secret keys.
          </p>
        </div>
      )}

      {mcpServers.length === 0 ? (
        <div className="bg-white border border-slate-200 rounded-xl p-12 text-center">
          <TerminalSquare className="w-12 h-12 text-slate-300 mx-auto mb-4" />
          <h3 className="text-lg font-medium text-slate-900">No active servers</h3>
          <p className="text-slate-500 mt-2 mb-6 max-w-md mx-auto">
            You haven't deployed any MCP servers yet. Generate one from the SDK Builder to start exposing your APIs to LLMs.
          </p>
        </div>
      ) : (
        <div className="space-y-4">
          {selectedIds.length > 0 && (
            <div className="flex items-center animate-in fade-in slide-in-from-top-2 duration-200">
              <label className="flex items-center gap-2 text-sm text-slate-600 cursor-pointer bg-blue-50 px-3 py-1.5 rounded-lg border border-blue-100">
                <input
                  type="checkbox"
                  className="rounded border-blue-300 text-blue-600 focus:ring-blue-500 w-4 h-4 cursor-pointer"
                  checked={selectedIds.length === mcpServers.length && mcpServers.length > 0}
                  onChange={() => {
                    if (selectedIds.length === mcpServers.length) {
                      setSelectedIds([]);
                    } else {
                      setSelectedIds(mcpServers.map(s => s.id));
                    }
                  }}
                />
                <span className="font-medium text-blue-800">Select All on Page</span>
              </label>
            </div>
          )}
          <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
          {mcpServers.map(server => (
            <div key={server.id} className={`group bg-white border border-slate-200 rounded-xl overflow-hidden shadow-sm transition-shadow ${!server.active ? 'opacity-75' : 'hover:shadow-md'}`}>
              <div className="p-5 border-b border-slate-100 flex items-start justify-between">
                <div className="flex items-start gap-3">
                  <div className={`pt-2 transition-opacity duration-200 ${selectedIds.length > 0 || selectedIds.includes(server.id) ? 'opacity-100' : 'opacity-0 group-hover:opacity-100 focus-within:opacity-100'}`}>
                    <input
                      type="checkbox"
                      className="rounded border-slate-300 text-blue-600 focus:ring-blue-500 w-4 h-4 cursor-pointer"
                      checked={selectedIds.includes(server.id)}
                      onChange={() => {
                        setSelectedIds(prev => prev.includes(server.id) ? prev.filter(i => i !== server.id) : [...prev, server.id]);
                      }}
                    />
                  </div>
                  <div className={`w-10 h-10 rounded-lg border flex items-center justify-center shrink-0 ${server.active ? 'bg-indigo-50 border-indigo-100' : 'bg-slate-50 border-slate-200'}`}>
                    <TerminalSquare className={`w-5 h-5 ${server.active ? 'text-indigo-600' : 'text-slate-400'}`} />
                  </div>
                  <div>
                    <h3 className="font-semibold text-slate-900 leading-tight">{server.name || "Untitled MCP server"}</h3>
                    <div className="flex items-center gap-2 mt-1">
                      <span className={`flex items-center gap-1.5 text-xs font-medium px-2 py-0.5 rounded-full ${server.active ? 'text-emerald-600 bg-emerald-50' : 'text-slate-500 bg-slate-100'}`}>
                        {server.active && <span className="w-1.5 h-1.5 rounded-full bg-emerald-500 animate-pulse"></span>}
                        {server.active ? 'Running' : 'Killed'}
                      </span>
                    </div>
                  </div>
                </div>
                <button
                  data-track="delete_mcp_server"
                  onClick={() => handleDelete(server.id, server.name)}
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
                    <code className="flex-1 bg-white px-3 py-2 rounded-lg border border-slate-200 text-xs font-mono text-slate-800 break-all shadow-sm">
                      {server.active ? server.mcp_url : <span className="text-slate-400 italic">Server killed -- reactivate to reconnect</span>}
                    </code>
                    {server.active && (
                      <button
                        data-track="copy_mcp_sandbox_url"
                        onClick={() => {
                          navigator.clipboard.writeText(server.mcp_url);
                          toast.success("URL copied to clipboard!");
                        }}
                        className="p-2 bg-white border border-slate-200 text-slate-600 rounded-lg hover:bg-slate-50 hover:text-indigo-600 transition-colors shadow-sm cursor-pointer"
                        title="Copy URL"
                      >
                        <Copy className="w-4 h-4" />
                      </button>
                    )}
                  </div>
                </div>

                <div className="flex items-center justify-between pt-2">
                  <span className="text-xs text-slate-400 font-medium">
                    Created {new Date(server.created_at).toLocaleDateString()}
                  </span>
                  <div className="flex items-center gap-2">
                    {/* Kill/Reactivate toggle, not Restart -- Kill just flips
                        `active` (deployment and its selections are untouched),
                        so "restarting" is the same server coming back, not a
                        redeploy. There's also no token-regenerate button here
                        anymore: the Engine schema has no mutation for it, since
                        the scope's auth token is tied to the deploy itself. */}
                    {server.active ? (
                      <button
                        data-track="kill_mcp_server"
                        onClick={() => handleKill(server.id, server.name)}
                        className="inline-flex items-center gap-1.5 px-3 py-1.5 bg-amber-50 text-amber-600 hover:bg-amber-100 hover:text-amber-700 rounded-lg text-xs font-semibold transition-colors border border-amber-100 cursor-pointer"
                      >
                        <ServerCrash className="w-3.5 h-3.5" />
                        Kill
                      </button>
                    ) : (
                      <button
                        data-track="reactivate_mcp_server"
                        onClick={() => handleReactivate(server.id)}
                        className="inline-flex items-center gap-1.5 px-3 py-1.5 bg-emerald-50 text-emerald-600 hover:bg-emerald-100 hover:text-emerald-700 rounded-lg text-xs font-semibold transition-colors border border-emerald-100 cursor-pointer"
                      >
                        <Play className="w-3.5 h-3.5 fill-current" />
                        Reactivate
                      </button>
                    )}
                    <Link
                      to={`/integrations/mcp/${server.id}/analytics`}
                      className="inline-flex items-center gap-1.5 px-3 py-1.5 bg-indigo-50 text-indigo-600 hover:bg-indigo-100 hover:text-indigo-700 rounded-lg text-xs font-semibold transition-colors border border-indigo-100 cursor-pointer"
                    >
                      <BarChart2 className="w-3.5 h-3.5" />
                      Analytics
                    </Link>
                  </div>
                </div>
              </div>
            </div>
          ))}
        </div>
        </div>
      )}

      {total > limit && (
        <div className="flex items-center justify-between px-6 py-4 bg-white border border-slate-200 rounded-xl">
          <span className="text-sm text-slate-500">
            Showing <span className="font-medium">{(page - 1) * limit + 1}</span> to <span className="font-medium">{Math.min(page * limit, total)}</span> of <span className="font-medium">{total}</span>
          </span>
          <div className="flex gap-2">
            <button
              data-track="paginate_previous"
              onClick={() => setPage(p => Math.max(1, p - 1))}
              disabled={page === 1}
              className="px-3 py-1.5 text-sm font-medium border border-slate-200 rounded-md disabled:opacity-50 disabled:cursor-not-allowed hover:bg-slate-50 transition-colors"
            >
              Previous
            </button>
            <button
              data-track="paginate_next"
              onClick={() => setPage(p => p + 1)}
              disabled={page * limit >= total}
              className="px-3 py-1.5 text-sm font-medium border border-slate-200 rounded-md disabled:opacity-50 disabled:cursor-not-allowed hover:bg-slate-50 transition-colors"
            >
              Next
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
