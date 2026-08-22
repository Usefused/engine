import { useState, useEffect } from "react";
import { Link, type MetaFunction } from "@remix-run/react";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m) => m.id === "root").flatMap((m) => m.meta ?? []);
  return [
    ...parentMeta.filter((m) => !('title' in m)),
    { title: "MCP servers - Fused" },
  ];
};
import { Trash2, TerminalSquare, AlertCircle, Play, ServerCrash, BarChart2, Info } from "lucide-react";
import { api } from "~/lib/api";
import { useToast } from "~/components/Toast";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasResourcePermission, hasWorkspacePermission } from "~/lib/current-actor-access";
import { McpTransportEndpoints, type McpTransportEndpointData } from "~/components/mcp/McpTransportEndpoints";

interface McpServerItem extends McpTransportEndpointData {
  id: string;
  app_family_id: string;
  name: string;
  version: string;
  status: string;
	active: boolean;
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
  onEndpointCopied: (transport: "streamable_http" | "sse") => void;
  canManage: boolean;
  canReadActivity: boolean;
}

function McpServerStatusBadge({ status }: { status: string }) {
  if (status === "deprecated") {
    return <span className="flex items-center gap-1.5 text-xs font-medium px-2 py-0.5 rounded-full text-amber-700 bg-amber-50">Deprecated</span>;
  }
  return (
    <span className="flex items-center gap-1.5 text-xs font-medium px-2 py-0.5 rounded-full text-emerald-600 bg-emerald-50">
      <span className="w-1.5 h-1.5 rounded-full bg-emerald-500 animate-pulse"></span>
      Running
    </span>
  );
}

// Kill/Reactivate toggle, not Restart -- Kill just flips `active` (deployment
// and its selections are untouched), so "restarting" is the same server
// coming back, not a redeploy. Execution-token lifecycle stays separate from
// app-version lifecycle so ending a client session never mutates credentials.
function McpKillReactivateButton({ server, onKill, onReactivate }: { server: McpServerItem; onKill: (id: string, name: string) => void; onReactivate: (id: string) => void }) {
  if (server.status === "active") {
    return (
      <button
        data-track="kill_mcp_server"
        onClick={() => onKill(server.id, server.name)}
        className="inline-flex items-center gap-1.5 px-3 py-1.5 bg-amber-50 text-amber-600 hover:bg-amber-100 hover:text-amber-700 rounded-lg text-xs font-semibold transition-colors border border-amber-100 cursor-pointer"
      >
        <ServerCrash className="w-3.5 h-3.5" />
        Deprecate
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
      Restore
    </button>
  );
}

// McpActivityLink keeps execution analytics undiscoverable until the exact
// app and workspace audit capabilities are both present.
function McpActivityLink({ serverId, visible }: { serverId: string; visible: boolean }) {
  if (!visible) return null;
  return (
    <Link
      to={`/integrations/mcp/${serverId}/analytics`}
      className="inline-flex items-center gap-1.5 px-3 py-1.5 bg-slate-100 text-slate-700 hover:bg-slate-200 hover:text-slate-900 rounded-lg text-xs font-semibold transition-colors border border-slate-200 cursor-pointer"
    >
      <BarChart2 className="w-3.5 h-3.5" />
      Analytics
    </Link>
  );
}

/** Renders one MCP server with lifecycle controls scoped to its app family. */
function McpServerCard({ server, isSelected, anySelected, onToggleSelect, onDelete, onKill, onReactivate, onEndpointCopied, canManage, canReadActivity }: McpServerCardProps) {
  return (
    <div className={mcpServerCardClass(server.active)}>
      <div className="p-5 border-b border-slate-100 flex items-start justify-between">
        <div className="flex items-start gap-3">
          {canManage && <div className={`pt-2 transition-opacity duration-200 ${anySelected || isSelected ? 'opacity-100' : 'opacity-0 group-hover:opacity-100 focus-within:opacity-100'}`}>
            <input
              type="checkbox"
              className="rounded border-slate-300 text-blue-600 focus:ring-blue-500 w-4 h-4 cursor-pointer"
              checked={isSelected}
              onChange={() => onToggleSelect(server.id)}
            />
          </div>}
          <div className={`w-10 h-10 rounded-lg border flex items-center justify-center shrink-0 ${server.active ? 'bg-slate-100 border-slate-200' : 'bg-slate-50 border-slate-200'}`}>
            <TerminalSquare className={`w-5 h-5 ${server.active ? 'text-slate-700' : 'text-slate-400'}`} />
          </div>
          <div>
            <h3 className="font-semibold text-slate-900 leading-tight">{server.name || "Untitled MCP server"}</h3>
            <div className="flex items-center gap-2 mt-1">
              <McpServerStatusBadge status={server.status} />
            </div>
          </div>
        </div>
        {canManage && <button
          data-track="delete_mcp_server"
          onClick={() => onDelete(server.id, server.name)}
          className="p-1.5 text-slate-400 hover:text-rose-600 hover:bg-rose-50 rounded-lg transition-colors cursor-pointer"
          title="Delete server"
        >
          <Trash2 className="w-4 h-4" />
        </button>}
      </div>

      <div className="p-5 bg-slate-50/50 space-y-4">
        <div>
          <label className="text-xs font-semibold text-slate-500 uppercase tracking-wider mb-2 block">Connection endpoints</label>
          <McpTransportEndpoints endpoints={server} enabled={server.active} onCopied={onEndpointCopied} />
        </div>

        <div className="flex items-center justify-between pt-2">
          <span className="text-xs text-slate-400 font-medium">
            Created {server.created_at ? new Date(server.created_at).toLocaleDateString() : ""}
          </span>
          <div className="flex items-center gap-2">
            {canManage && <McpKillReactivateButton server={server} onKill={onKill} onReactivate={onReactivate} />}
            <McpActivityLink serverId={server.id} visible={canReadActivity} />
          </div>
        </div>
      </div>
    </div>
  );
}

/** Selects the inactive or hoverable MCP card presentation. */
function mcpServerCardClass(active: boolean): string {
  return `group bg-white border border-slate-200 rounded-xl overflow-hidden shadow-sm transition-shadow ${active ? "hover:shadow-md" : "opacity-75"}`;
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
          Use the server's recommended <strong>Streamable HTTP endpoint</strong> and send its execution token in the authorization header:
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

/** Lists readable MCP servers and gates create and lifecycle operations. */
export default function McpServers() {
  const toast = useToast();
  const { access } = useCurrentActorAccess();
  const canCreate = hasWorkspacePermission(access, "app.create");
  const [mcpServers, setMcpServers] = useState<McpServerItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [page, setPage] = useState(1);
  const [total, setTotal] = useState(0);
  const [showHowToUse, setShowHowToUse] = useState(false);
  const [selectedIds, setSelectedIds] = useState<string[]>([]);
  const [isDeletingMultiple, setIsDeletingMultiple] = useState(false);
  const limit = 10;

  /** Loads one variable-bound page of readable MCP app versions. */
  const fetchServers = () => {
    setLoading(true);
    const queryStr = `
      query($limit: Int!, $offset: Int!) {
        apps(kind: "mcp", limit: $limit, offset: $offset) {
          items {
            app_id
            app_family_id
            name
            version
            status
            created_at
            default_transport
            transport_urls { streamable_http sse }
          }
          total
        }
      }
    `;
    type MCPApp = Omit<McpServerItem, "id" | "active"> & { app_id: string; status: string };
    api.mcpGraphql<{ apps: { items: MCPApp[], total: number } }>(queryStr, { limit, offset: (page - 1) * limit })
      .then(res => {
        setMcpServers(res.apps.items.map(app => ({
          ...app,
          id: app.app_id,
          active: app.status === "active" || app.status === "deprecated",
        })));
        setTotal(res.apps.total);
      })
      .catch(e => setError(e instanceof Error ? e.message : "Failed to load MCP servers"))
      .finally(() => setLoading(false));
  };

  useEffect(() => {
    fetchServers();
    setSelectedIds([]);
  }, [page]);

  /** Deprecates one manageable MCP app after confirmation. */
  const handleKill = async (id: string, name: string) => {
    const confirmed = await toast.confirm(`Deprecate MCP app "${name}"? Existing clients can continue while teammates move to another version.`);
    if (!confirmed) return;
    try {
      await api.mcpGraphql(`mutation($appId: String!, $message: String!) { deprecateApp(app_id: $appId, message: $message) }`, { appId: id, message: "A newer MCP app version is available" });
      fetchServers();
      toast.success(`MCP app "${name}" deprecated.`);
    } catch (err) {
      toast.error(`Failed to deprecate MCP app: ${err instanceof Error ? err.message : "Unknown error"}`);
    }
  };

  /** Deactivates one manageable MCP app after confirmation. */
  const handleDelete = async (id: string, name: string) => {
    const confirmed = await toast.confirm(`Are you sure you want to completely delete the MCP server "${name}"? This cannot be undone.`);
    if (!confirmed) return;
    try {
      await api.mcpGraphql(`mutation($appId: String!) { deactivateApp(app_id: $appId) }`, { appId: id });
      fetchServers();
      toast.success(`Server "${name}" deleted successfully.`);
      setSelectedIds(prev => prev.filter(i => i !== id));
    } catch (err) {
      toast.error(`Failed to delete server: ${err instanceof Error ? err.message : "Unknown error"}`);
    }
  };

  /** Deactivates the selected manageable MCP apps as one UI action. */
  const handleDeleteMultiple = async () => {
    if (selectedIds.length === 0) return;
    const confirmed = await toast.confirm(`Are you sure you want to completely delete ${selectedIds.length} MCP server(s)? This cannot be undone.`);
    if (!confirmed) return;

    setIsDeletingMultiple(true);
    try {
      await Promise.all(selectedIds.map(id => api.mcpGraphql(`mutation($appId: String!) { deactivateApp(app_id: $appId) }`, { appId: id })));
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

  /** Restores one deprecated MCP app. */
  const handleReactivate = async (id: string) => {
    try {
      await api.mcpGraphql(`mutation($appId: String!) { undeprecateApp(app_id: $appId) }`, { appId: id });
      fetchServers();
      toast.success("MCP app restored.");
    } catch (err) {
      toast.error(`Failed to reactivate server: ${err instanceof Error ? err.message : "Unknown error"}`);
    }
  };

  const manageableServerIds = mcpServers
    .filter((server) => hasResourcePermission(access, "app.manage", "APP", server.app_family_id))
    .map((server) => server.id);

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
          {canCreate && <Link
            to="/integrations/builder?tab=mcp"
            className="inline-flex items-center gap-2 px-4 py-2 bg-slate-950 text-white text-sm font-medium rounded-lg hover:bg-slate-800 transition-colors shadow-sm self-start sm:self-auto"
          >
            <Play className="w-4 h-4" />
            Create MCP server
          </Link>}
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
            allSelected={selectedIds.length === manageableServerIds.length && manageableServerIds.length > 0}
            onToggle={() => setSelectedIds(prev => prev.length === manageableServerIds.length ? [] : manageableServerIds)}
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
              onEndpointCopied={(transport) => toast.success(`${transport === "streamable_http" ? "Streamable HTTP" : "SSE"} URL copied to clipboard!`)}
              canManage={hasResourcePermission(access, "app.manage", "APP", server.app_family_id)}
              canReadActivity={hasWorkspacePermission(access, "audit.read") && hasResourcePermission(access, "app.read", "APP", server.app_family_id)}
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
