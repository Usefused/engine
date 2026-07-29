import { useState, useEffect } from "react";
import { Link, useNavigate, useSearchParams, type MetaFunction } from "@remix-run/react";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m: any) => m.id === "root").flatMap((m: any) => m.meta ?? []);
  return [
    ...parentMeta.filter((m: any) => !('title' in m)),
    { title: "SDK History - Fused" },
  ];
};
import { Download, Package, Plus, Trash2, RefreshCw, Loader2, Search, X } from "lucide-react";
import { api } from "~/lib/api";
import { useToast } from "~/components/Toast";

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

export default function SdkHistory() {
  const toast = useToast();
  const [searchParams, setSearchParams] = useSearchParams();
  const [sdks, setSdks] = useState<any[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [query, setQuery] = useState(searchParams.get("q") ?? "");
  const [searching, setSearching] = useState(false);
  const [selectedIds, setSelectedIds] = useState<string[]>([]);
  const [isDeletingMultiple, setIsDeletingMultiple] = useState(false);
  const navigate = useNavigate();

  const fetchSdks = () => {
    setLoading(true);
    // Over-fetch then deduplicate client-side by name (keep latest by created_at)
    const queryStr = `
      query {
        sdks(limit: 100, offset: 0, target_type: "sdk", latest_only: true) {
          items {
            id
            name
            description
            version
            target_type
            target_language
            sandbox_url
            is_downloadable
            has_update_available
            has_deprecated_endpoints
            created_at
            killed_at
            downloads
          }
          total
        }
      }
    `;
    api.graphql<{ sdks: { items: any[]; total: number } }>(queryStr)
      .then(res => {
        setSdks(res.sdks.items ?? []);
        setSelectedIds([]);
      })
      .catch(e => setError(e.message))
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
      const queryStr = `
        query($name: String!, $version: String) {
          sdkByName(name: $name, version: $version) {
            id
            name
            description
            version
            target_type
            target_language
            sandbox_url
            is_downloadable
            has_update_available
            has_deprecated_endpoints
            created_at
            killed_at
            downloads
          }
        }
      `;
      const res = await api.graphql<{ sdkByName: any }>(queryStr, { name: searchName, version: searchVersion });
      if (res.sdkByName) {
        setSdks([res.sdkByName]);
      } else {
        setSdks([]);
      }
    } catch (e: unknown) {
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
    } catch (err) {
      toast.error("Failed to download SDK");
    }
  };

  const handleDelete = async (id: string, name: string) => {
    const confirmed = await toast.confirm(`Are you sure you want to delete SDK "${name}"?`);
    if (!confirmed) return;
    try {
      await api.sdks.delete(id);
      fetchSdks();
      toast.success(`SDK "${name}" deleted successfully.`);
      setSelectedIds(prev => prev.filter(i => i !== id));
    } catch (err: any) {
      toast.error(`Failed to delete SDK: ${err.message || "Unknown error"}`);
    }
  };

  const handleDeleteMultiple = async () => {
    if (selectedIds.length === 0) return;
    const confirmed = await toast.confirm(`Are you sure you want to delete ${selectedIds.length} SDK(s)?`);
    if (!confirmed) return;
    
    setIsDeletingMultiple(true);
    try {
      await Promise.all(selectedIds.map(id => api.sdks.delete(id)));
      fetchSdks();
      setSelectedIds([]);
      toast.success(`Successfully deleted ${selectedIds.length} SDK(s).`);
    } catch (err: any) {
      toast.error(`Failed to delete some SDKs: ${err.message || "Unknown error"}`);
      fetchSdks();
    } finally {
      setIsDeletingMultiple(false);
    }
  };

  const handleUpgrade = async (id: string, name: string) => {
    const confirmed = await toast.confirm(`Are you sure you want to 1-Click Upgrade SDK "${name}" to the latest service versions?\n\nThis will generate a new SDK and leave this one intact in your history.`);
    if (!confirmed) return;
    try {
      await api.sdks.upgradeAsync(id);
      toast.success("Upgrade started! A new SDK will appear in your history shortly.");
      fetchSdks();
    } catch (err: any) {
      toast.error(`Failed to upgrade SDK: ${err.message || "Unknown error"}`);
    }
  };

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-xl font-semibold text-slate-900">SDKs</h1>
          <p className="text-slate-500 text-sm mt-1">Manage and track your generated SDK packages.</p>
        </div>
        <div className="flex items-center gap-3">
          {selectedIds.length > 0 && (
            <button
              onClick={handleDeleteMultiple}
              disabled={isDeletingMultiple}
              className="inline-flex items-center gap-2 px-4 py-2 bg-rose-50 text-rose-600 hover:bg-rose-100 text-sm font-medium rounded-lg transition-all border border-rose-200 cursor-pointer"
            >
              <Trash2 className="w-4 h-4" />
              {isDeletingMultiple ? "Deleting..." : `Delete Selected (${selectedIds.length})`}
            </button>
          )}
          <Link
            to="/integrations/sdk-builder"
            className="inline-flex items-center shadow-blue-200 gap-2 px-4 py-2 bg-blue-600 hover:bg-blue-700 text-white text-sm font-medium rounded-lg transition-colors shadow-sm cursor-pointer"
          >
            <Plus className="w-4 h-4" />
            Create
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
          placeholder="Search by SDK name..."
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

      {loading || searching ? (
        <div className="flex flex-col items-center justify-center py-20 text-slate-400">
          <Loader2 className="w-8 h-8 text-blue-500 animate-spin mb-4" />
          <p className="animate-pulse font-medium text-slate-500">Loading SDKs...</p>
        </div>
      ) : sdks.length === 0 ? (
        <div className="bg-white rounded-xl border border-slate-200 p-12 text-center">
          <Package className="w-12 h-12 text-slate-300 mx-auto mb-4" />
          <h3 className="text-lg font-medium text-slate-900 mb-1">
            {query ? "No SDKs Found" : "No SDKs Generated"}
          </h3>
          <p className="text-slate-500 max-w-md mx-auto">
            {query 
              ? "We couldn't find any SDKs matching your search." 
              : "You haven't generated any SDKs yet. Search for a service to get started."}
          </p>
          {!query && (
            <Link
              to="/integrations/sdk-builder"
              className="mt-5 inline-flex items-center gap-2 px-4 py-2 bg-blue-600 hover:bg-blue-700 text-white text-sm font-medium rounded-lg transition-colors shadow-sm cursor-pointer"
            >
              <Plus className="w-4 h-4" />
              Search services
            </Link>
          )}
        </div>
      ) : (
        <div className="bg-white rounded-xl border border-slate-200 overflow-x-auto">
          <table className="w-full text-left text-sm whitespace-nowrap">
            <thead className="bg-slate-50 border-b border-slate-200 text-slate-500">
              <tr>
                <th className="px-6 py-4 font-medium">
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
                              setSelectedIds(sdks.map(s => s.id));
                            }
                          }}
                        />
                      </div>
                    </div>
                    <span>Package</span>
                  </div>
                </th>
                <th className="px-6 py-4 font-medium">Version</th>
                <th className="px-6 py-4 font-medium">Downloads</th>
                <th className="px-6 py-4 font-medium">Date</th>
                <th className="px-6 py-4 font-medium text-right">Action</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-slate-100">
              {sdks.map((sdk: any) => (
                <tr
                  key={sdk.id}
                  className="hover:bg-slate-50/50 transition-colors cursor-pointer group"
                  onClick={() => navigate(`/integrations/sdks/${sdk.id}`)}
                >
                  <td className="px-6 py-4">
                    <div className="flex items-center gap-3">
                      <div className="relative w-8 h-8 rounded shrink-0">
                        <div className={`absolute inset-0 z-10 bg-white/90 rounded flex items-center justify-center transition-opacity duration-200 ${selectedIds.length > 0 || selectedIds.includes(sdk.id) ? 'opacity-100' : 'opacity-0 group-hover:opacity-100 focus-within:opacity-100'}`} onClick={(e) => e.stopPropagation()}>
                          <input
                            type="checkbox"
                            className="rounded border-slate-300 text-blue-600 focus:ring-blue-500 w-4 h-4 cursor-pointer"
                            checked={selectedIds.includes(sdk.id)}
                            onChange={() => {
                              setSelectedIds(prev => prev.includes(sdk.id) ? prev.filter(i => i !== sdk.id) : [...prev, sdk.id]);
                            }}
                          />
                        </div>
                        <div className="absolute inset-0 w-8 h-8 rounded bg-blue-100 flex items-center justify-center text-blue-600">
                          <Package className="w-4 h-4" />
                        </div>
                      </div>
                      <div className="flex items-center gap-2">
                        <span className="font-semibold text-slate-900">{sdk.name}</span>
                        {sdk.target_type === "mcp" && (
                          <span className="inline-flex items-center px-2 py-0.5 rounded text-[10px] font-bold bg-indigo-100 text-indigo-700 uppercase tracking-wider">
                            MCP
                          </span>
                        )}
                        {sdk.target_type === "sdk" && (
                          <LanguageBadge targetLanguage={sdk.target_language} />
                        )}
                      </div>
                    </div>
                  </td>
                  <td className="px-6 py-4">
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
                  </td>
                  <td className="px-6 py-4 text-slate-500 font-medium">
                    {sdk.downloads || 0}
                  </td>
                  <td className="px-6 py-4 text-slate-500">
                    {new Date(sdk.created_at).toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' })}
                  </td>
                  <td className="px-6 py-4 text-right">
                    <div className="flex justify-end gap-2">
                      {sdk.has_update_available && (
                        <button
                          onClick={(e) => { e.stopPropagation(); handleUpgrade(sdk.id, sdk.name); }}
                          className="inline-flex items-center gap-1.5 px-3 py-1.5 text-sm font-medium text-white bg-green-600 hover:bg-green-700 rounded-lg transition-colors shadow-sm cursor-pointer"
                          title="1-Click Upgrade"
                        >
                          <RefreshCw className="w-4 h-4" />
                          Upgrade
                        </button>
                      )}
                      {sdk.is_downloadable ? (
                        <button
                          onClick={(e) => { e.stopPropagation(); handleDownload(sdk.id, sdk.name, sdk.version); }}
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
                        onClick={(e) => { e.stopPropagation(); handleDelete(sdk.id, sdk.name); }}
                        className="inline-flex items-center justify-center w-8 h-8 text-sm font-medium text-red-600 bg-red-50 hover:bg-red-100 rounded-lg transition-colors cursor-pointer"
                        title="Delete SDK"
                      >
                        <Trash2 className="w-4 h-4" />
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
