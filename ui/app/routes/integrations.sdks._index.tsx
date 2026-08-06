import { useState, useEffect } from "react";
import { Link, useNavigate, useSearchParams, type MetaFunction } from "@remix-run/react";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m) => m.id === "root").flatMap((m) => m.meta ?? []);
  return [
    ...parentMeta.filter((m) => !('title' in m)),
    { title: "Apps - Fused" },
  ];
};
import { Download, Package, Plus, Trash2, Loader2, Search, X } from "lucide-react";
import { api } from "~/lib/api";
import { useToast } from "~/components/Toast";
import { AppRuntimeStatus } from "~/components/apps/AppRuntimeStatus";

interface SdkListItem {
  app_id: string;
  app_family_id: string;
  name: string;
  description?: string;
  version: string;
  target_type: string;
  target_language?: string;
  sandbox_url?: string;
  is_downloadable?: boolean;
  has_update_available?: boolean;
  has_deprecated_endpoints?: boolean;
  created_at?: string;
  killed_at?: string;
  downloads?: number;
  status: string;
}

function LanguageBadge({ targetLanguage }: { targetLanguage?: string }) {
  if (targetLanguage === "python") {
    return (
      <span className="inline-flex items-center justify-center w-5 h-5" title="Python" aria-label="Python">
        <svg viewBox="0 0 32 32" className="w-3.5 h-3.5 shrink-0" aria-hidden="true">
          <path fill="#3776AB" d="M15.9 3c-6.3 0-5.9 2.7-5.9 2.7v2.8h6v.9H7.6S3 8.9 3 15.9 7 22.6 7 22.6h2.4v-3.4s-.1-4 4-4h6.7s3.7.1 3.7-3.5V5.9S24.4 3 18 3h-2.1Zm-3.3 2a1.2 1.2 0 1 1 0 2.3 1.2 1.2 0 0 1 0-2.3Z"/>
          <path fill="#FFD43B" d="M16.1 29c6.3 0 5.9-2.7 5.9-2.7v-2.8h-6v-.9h8.4s4.6.5 4.6-6.5-4-6.7-4-6.7h-2.4v3.4s.1 4-4 4H12s-3.7-.1-3.7 3.5v5.8S7.6 29 14 29h2.1Zm3.3-2a1.2 1.2 0 1 1 0-2.3 1.2 1.2 0 0 1 0 2.3Z"/>
        </svg>
      </span>
    );
  }

  return (
    <span className="inline-flex items-center justify-center w-5 h-5" title="TypeScript" aria-label="TypeScript">
      <svg viewBox="0 0 32 32" className="w-3.5 h-3.5 shrink-0" aria-hidden="true">
        <rect x="3" y="3" width="26" height="26" rx="3" fill="#3178C6" />
        <path fill="#fff" d="M11.3 13.1h9.8v2.2h-3.7V26h-2.4V15.3h-3.7v-2.2Zm10.6 0h-2.4v8.5c0 2.5 1.3 4.4 4.7 4.4 1.2 0 2.5-.3 3.5-.8v-2.1c-.9.5-1.8.8-2.7.8-1.7 0-3.1-.8-3.1-2.6v-8.2Z"/>
      </svg>
    </span>
  );
}

interface SdkRowProps {
  sdk: SdkListItem;
  selectedIds: string[];
  setSelectedIds: React.Dispatch<React.SetStateAction<string[]>>;
  onNavigate: (id: string) => void;
  onDownload: (id: string, name: string, version: string) => void;
  onDeactivate: (id: string, name: string) => void;
}

function SdkNameCell({ sdk }: { sdk: SdkListItem }) {
  return (
    <div className="min-w-0">
      <div className="flex min-w-0 items-center gap-2">
        <span className="block min-w-0 truncate font-semibold text-slate-900">{sdk.name}</span>
        {sdk.target_type === "mcp" && (
          <span className="inline-flex items-center px-2 py-0.5 rounded text-[10px] font-bold bg-slate-200 text-slate-700 uppercase tracking-wider">
            MCP
          </span>
        )}
        {sdk.target_type === "sdk" && (
          <LanguageBadge targetLanguage={sdk.target_language} />
        )}
      </div>
      <AppRuntimeStatus className="mt-0.5" status={sdk.status} />
    </div>
  );
}

function SdkVersionBadges({ sdk }: { sdk: SdkListItem }) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      <span className="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-slate-100 text-slate-700 border border-slate-200">
        {sdk.version}
      </span>
      {sdk.killed_at && (
        <span className="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-slate-100 text-slate-500 border border-slate-200">
          Killed
        </span>
      )}
      {sdk.has_deprecated_endpoints && (
        <span className="inline-flex items-center px-2 py-0.5 rounded text-[10px] font-bold bg-red-100 text-red-700 border border-red-200 uppercase tracking-wider">
          Deprecated Endpoints
        </span>
      )}
      {sdk.has_update_available && (
        <span className="inline-flex items-center px-2 py-0.5 rounded text-[10px] font-bold bg-green-100 text-green-700 border border-green-200 uppercase tracking-wider">
          Update Available
        </span>
      )}
    </div>
  );
}

interface SdkActionButtonsProps {
  sdk: SdkListItem;
  onDownload: (id: string, name: string, version: string) => void;
  onDeactivate: (id: string, name: string) => void;
}

function SdkActionButtons({ sdk, onDownload, onDeactivate }: SdkActionButtonsProps) {
  return (
    <div className="flex justify-end gap-1 sm:gap-2">
      {sdk.is_downloadable ? (
        <button
          onClick={(e) => { e.stopPropagation(); onDownload(sdk.app_id, sdk.name, sdk.version); }}
          className="inline-flex items-center justify-center w-8 h-8 text-sm font-medium text-blue-600 bg-blue-50 hover:bg-blue-100 rounded-lg transition-colors cursor-pointer"
          title="Download SDK"
        >
          <Download className="w-4 h-4" />
        </button>
      ) : (
        <button
          disabled
          className="inline-flex items-center justify-center w-8 h-8 text-sm font-medium text-slate-400 bg-slate-100 rounded-lg cursor-not-allowed"
          title="This SDK has expired and its files have been cleaned up."
        >
          <Download className="w-4 h-4" />
        </button>
      )}
      <button
        onClick={(e) => { e.stopPropagation(); onDeactivate(sdk.app_id, sdk.name); }}
        className="inline-flex items-center justify-center w-8 h-8 text-sm font-medium text-red-600 bg-red-50 hover:bg-red-100 rounded-lg transition-colors cursor-pointer"
        title="Deactivate SDK version"
      >
        <Trash2 className="w-4 h-4" />
      </button>
    </div>
  );
}

interface SdkRowProps {
  sdk: SdkListItem;
  selectedIds: string[];
  setSelectedIds: React.Dispatch<React.SetStateAction<string[]>>;
  onNavigate: (id: string) => void;
  onDownload: (id: string, name: string, version: string) => void;
  onDeactivate: (id: string, name: string) => void;
}

function SdkRow({ sdk, selectedIds, setSelectedIds, onNavigate, onDownload, onDeactivate }: SdkRowProps) {
  const isSelected = selectedIds.includes(sdk.app_id);
  const showCheckbox = selectedIds.length > 0 || isSelected;
  return (
    <tr
      className="hover:bg-slate-50/50 transition-colors cursor-pointer group"
      onClick={() => onNavigate(sdk.app_id)}
    >
      <td className="px-3 sm:px-6 py-4 min-w-0">
        <div className="flex min-w-0 items-center gap-2 sm:gap-3">
          <div className="relative w-8 h-8 rounded shrink-0">
            <div className={`absolute inset-0 z-10 bg-white/90 rounded flex items-center justify-center transition-opacity duration-200 ${showCheckbox ? 'opacity-100' : 'opacity-0 group-hover:opacity-100 focus-within:opacity-100'}`} onClick={(e) => e.stopPropagation()}>
              <input
                type="checkbox"
                className="rounded border-slate-300 text-blue-600 focus:ring-blue-500 w-4 h-4 cursor-pointer"
                checked={isSelected}
                onChange={() => {
                  setSelectedIds(prev => prev.includes(sdk.app_id) ? prev.filter(i => i !== sdk.app_id) : [...prev, sdk.app_id]);
                }}
              />
            </div>
            <div className="absolute inset-0 w-8 h-8 rounded bg-blue-100 flex items-center justify-center text-blue-600">
              <Package className="w-4 h-4" />
            </div>
          </div>
          <SdkNameCell sdk={sdk} />
        </div>
      </td>
      <td className="px-2 sm:px-6 py-4">
        <SdkVersionBadges sdk={sdk} />
      </td>
      <td className="hidden md:table-cell px-6 py-4 text-slate-500 font-medium">
        {sdk.downloads || 0}
      </td>
      <td className="hidden lg:table-cell px-6 py-4 text-slate-500">
        {sdk.created_at ? new Date(sdk.created_at).toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' }) : ""}
      </td>
      <td className="px-2 sm:px-6 py-4 text-right">
        <SdkActionButtons sdk={sdk} onDownload={onDownload} onDeactivate={onDeactivate} />
      </td>
    </tr>
  );
}

interface SdkListContentProps {
  loading: boolean;
  searching: boolean;
  sdks: SdkListItem[];
  query: string;
  selectedIds: string[];
  setSelectedIds: React.Dispatch<React.SetStateAction<string[]>>;
  navigate: (path: string) => void;
  onDownload: (id: string, name: string, version: string) => void;
  onDeactivate: (id: string, name: string) => void;
}

function SdkListContent({ loading, searching, sdks, query, selectedIds, setSelectedIds, navigate, onDownload, onDeactivate }: SdkListContentProps) {
  if (loading || searching) {
    return (
      <div className="flex flex-col items-center justify-center py-20 text-slate-400">
        <Loader2 className="w-8 h-8 text-blue-500 animate-spin mb-4" />
        <p className="animate-pulse font-medium text-slate-500">Loading SDKs...</p>
      </div>
    );
  }
  if (sdks.length === 0) {
    return (
      <div className="bg-white rounded-xl border border-slate-200 p-12 text-center">
        <Package className="w-12 h-12 text-slate-300 mx-auto mb-4" />
        <h3 className="text-lg font-medium text-slate-900 mb-1">
          {query ? "No apps found" : "No apps yet"}
        </h3>
        <p className="text-slate-500 max-w-md mx-auto">
          {query
            ? "No apps match your search."
            : "Create an app to give it reusable access to selected services and operations."}
        </p>
        {!query && (
          <Link
            to="/integrations/builder"
            className="mt-5 inline-flex items-center gap-2 px-4 py-2 bg-slate-950 hover:bg-slate-800 text-white text-sm font-medium rounded-lg transition-colors shadow-sm cursor-pointer"
          >
            <Plus className="w-4 h-4" />
            Create app
          </Link>
        )}
      </div>
    );
  }
  return (
    <div className="bg-white rounded-xl border border-slate-200 overflow-x-auto">
      <table className="w-full table-fixed md:table-auto text-left text-sm whitespace-nowrap">
        <thead className="bg-slate-50 border-b border-slate-200 text-slate-500">
          <tr>
            <th className="w-[55%] md:w-auto px-3 sm:px-6 py-4 font-medium">
              <div className="flex items-center gap-3">
                <div className="flex items-center justify-center w-8 h-8">
                  <div className={`transition-opacity duration-200 ${selectedIds.length > 0 ? 'opacity-100' : 'opacity-0'}`}>
                    <input
                      type="checkbox"
                      className="rounded border-slate-300 text-blue-600 focus:ring-blue-500 w-4 h-4 cursor-pointer"
                      checked={selectedIds.length === sdks.length && sdks.length > 0}
                      onChange={() => {
                        if (selectedIds.length === sdks.length) {
                          setSelectedIds([]);
                        } else {
                          setSelectedIds(sdks.map(s => s.app_id));
                        }
                      }}
                    />
                  </div>
                </div>
                <span>App</span>
              </div>
            </th>
            <th className="w-[25%] md:w-auto px-2 sm:px-6 py-4 font-medium">Version</th>
            <th className="hidden md:table-cell px-6 py-4 font-medium">Downloads</th>
            <th className="hidden lg:table-cell px-6 py-4 font-medium">Date</th>
            <th className="w-[20%] md:w-auto px-2 sm:px-6 py-4 font-medium text-right">Action</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-slate-100">
          {sdks.map((sdk) => (
            <SdkRow
              key={sdk.app_id}
              sdk={sdk}
              selectedIds={selectedIds}
              setSelectedIds={setSelectedIds}
              onNavigate={(id) => navigate(`/integrations/sdks/${id}`)}
              onDownload={onDownload}
              onDeactivate={onDeactivate}
            />
          ))}
        </tbody>
      </table>
    </div>
  );
}

export default function SdkHistory() {
  const toast = useToast();
  const [searchParams, setSearchParams] = useSearchParams();
  const [sdks, setSdks] = useState<SdkListItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [query, setQuery] = useState(searchParams.get("q") ?? "");
  const [searching, setSearching] = useState(false);
  const [selectedIds, setSelectedIds] = useState<string[]>([]);
  const [isDeactivatingMultiple, setIsDeactivatingMultiple] = useState(false);
  const navigate = useNavigate();

  const fetchSdks = () => {
    setLoading(true);
    const queryStr = `
      query {
        apps(kind: "sdk", limit: 100, offset: 0) {
          items {
            app_id
            app_family_id
            name
            description
            version
            target_language
            created_at
            status
          }
          total
        }
      }
    `;
    api.mcpGraphql<{ apps: { items: SdkListItem[]; total: number } }>(queryStr)
      .then(res => {
        setSdks((res.apps.items ?? []).map(item => ({ ...item, target_type: "sdk", is_downloadable: true })));
        setSelectedIds([]);
      })
      .catch(e => setError(e instanceof Error ? e.message : "Failed to load apps"))
      .finally(() => setLoading(false));
  };

  async function runSearch(q: string) {
    if (!q.trim()) return;
    setSearchParams(prev => {
      const next = new URLSearchParams(prev);
      next.set("q", q);
      return next;
    }, { replace: true });
    setSearching(true);
    setError("");

    let searchName = q.trim();
    let searchVersion = "";
    const trimmedQ = q.trim();
    const atIndex = trimmedQ.lastIndexOf("@");
    // Only split if @ is not the first character (to support @org/package format if any)
    if (atIndex > 0) {
      searchName = trimmedQ.substring(0, atIndex);
      searchVersion = trimmedQ.substring(atIndex + 1);
    }

    try {
      const queryStr = `query($search: String!, $version: String!) { apps(kind: "sdk", search: $search, version: $version, limit: 100, offset: 0) { items { app_id app_family_id name description version target_language created_at status } } }`;
      const res = await api.mcpGraphql<{ apps: { items: SdkListItem[] } }>(queryStr, { search: searchName, version: searchVersion });
      setSdks((res.apps.items ?? []).map(item => ({ ...item, target_type: "sdk", is_downloadable: true })));
    } catch {
      setSdks([]);
    } finally {
      setSearching(false);
    }
  }

  // Debounced search on type
  useEffect(() => {
    if (!query.trim()) {
      setSearching(false);
      fetchSdks();
      return;
    }
    setLoading(false);
    setSearching(true);
    const id = setTimeout(() => runSearch(query), 400);
    return () => clearTimeout(id);
  }, [query]);

  async function handleSearch(e: React.FormEvent) {
    e.preventDefault();
    runSearch(query);
  }

  async function handleClear() {
    setQuery("");
    setSearchParams(prev => {
      const next = new URLSearchParams(prev);
      next.delete("q");
      return next;
    }, { replace: true });
    setSelectedIds([]);
    fetchSdks();
  }

  const handleDownload = async (id: string, name: string, version: string) => {
    try {
      await api.sdks.download(id, name, version);
    } catch {
      toast.error("Failed to download SDK");
    }
  };

  const handleDeactivate = async (id: string, name: string) => {
    const confirmed = await toast.confirm(`Deactivate SDK version "${name}"? This permanently removes its runtime and package.`);
    if (!confirmed) return;
    try {
      await api.sdks.deactivate(id);
      fetchSdks();
      toast.success(`SDK version "${name}" deactivated.`);
      setSelectedIds(prev => prev.filter(i => i !== id));
    } catch (err) {
      toast.error(`Failed to deactivate SDK: ${err instanceof Error ? err.message : "Unknown error"}`);
    }
  };

  const handleDeactivateMultiple = async () => {
    if (selectedIds.length === 0) return;
    const confirmed = await toast.confirm(`Deactivate ${selectedIds.length} SDK version(s)? This permanently removes their runtimes and packages.`);
    if (!confirmed) return;
    
    setIsDeactivatingMultiple(true);
    try {
      await Promise.all(selectedIds.map(id => api.sdks.deactivate(id)));
      fetchSdks();
      setSelectedIds([]);
      toast.success(`Deactivated ${selectedIds.length} SDK version(s).`);
    } catch (err) {
      toast.error(`Failed to deactivate some SDKs: ${err instanceof Error ? err.message : "Unknown error"}`);
      fetchSdks();
    } finally {
		setIsDeactivatingMultiple(false);
    }
  };

  return (
    <div className="space-y-6">
      <div className="flex flex-col sm:flex-row sm:items-start justify-between gap-4">
        <div className="min-w-0">
          <h1 className="text-xl font-semibold text-slate-900">Apps</h1>
          <p className="text-slate-500 text-sm mt-1">Choose which services and operations an app can use.</p>
        </div>
        <div className="flex w-full sm:w-auto items-center gap-3">
          {selectedIds.length > 0 && (
            <button
              onClick={handleDeactivateMultiple}
              disabled={isDeactivatingMultiple}
              className="inline-flex flex-1 sm:flex-none items-center justify-center gap-2 px-4 py-2 bg-rose-50 text-rose-600 hover:bg-rose-100 text-sm font-medium rounded-lg transition-all border border-rose-200 cursor-pointer"
            >
              <Trash2 className="w-4 h-4" />
              {isDeactivatingMultiple ? "Deactivating..." : `Deactivate selected (${selectedIds.length})`}
            </button>
          )}
          <Link
            to="/integrations/builder"
            className="inline-flex flex-1 sm:flex-none items-center justify-center gap-2 px-4 py-2 bg-slate-950 hover:bg-slate-800 text-white text-sm font-medium rounded-lg transition-colors shadow-sm cursor-pointer"
          >
            <Plus className="w-4 h-4" />
            Create app
          </Link>
        </div>
      </div>

      <form 
        onSubmit={handleSearch} 
        className="relative w-full mb-6"
      >
        <button
          type="submit"
          disabled={searching}
          className="absolute left-2.5 top-1/2 -translate-y-1/2 text-slate-400 hover:text-slate-600 disabled:opacity-50 cursor-pointer"
          title="Search"
        >
          {searching ? (
            <Loader2 className="w-3.5 h-3.5 animate-spin" />
          ) : (
            <Search className="w-3.5 h-3.5" />
          )}
        </button>
        <input
          type="text"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Search apps..."
          className="w-full text-sm border border-slate-300 rounded-lg pl-9 pr-8 py-2 focus:outline-none focus:ring-2 focus:ring-blue-500"
        />
        {query && (
          <button
            type="button"
            onClick={handleClear}
            className="absolute right-2.5 top-1/2 -translate-y-1/2 text-slate-400 hover:text-slate-600 cursor-pointer"
          >
            <X className="w-3.5 h-3.5" />
          </button>
        )}
      </form>

      {error && (
        <div className="mb-4 p-3 bg-red-50 border border-red-200 rounded-lg text-sm text-red-700">
          {error}
        </div>
      )}

      <SdkListContent
        loading={loading}
        searching={searching}
        sdks={sdks}
        query={query}
        selectedIds={selectedIds}
        setSelectedIds={setSelectedIds}
        navigate={navigate}
        onDownload={handleDownload}
        onDeactivate={handleDeactivate}
      />
    </div>
  );
}
