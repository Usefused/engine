import { FormEvent } from "react";
import { Link } from "@remix-run/react";
import { Loader2, Search, X , ChevronLeft, ChevronRight } from "lucide-react";
import { Service, ActivatedService, serviceHref } from "~/lib/api";
import { formatServiceName } from "~/lib/format";
import { openAuthenticatedTab } from "~/lib/session";

// ListableService is the minimal normalised shape that IntegrationsListTab
// reads from. Both Service (catalog) and ActivatedService (workspace) satisfy
// it via the helpers below, so the component never needs an unsafe cast.
export type ListableService = {
  // Stable registry ID used for key and action calls
  id: string;
  // Display name (Service.name or ActivatedService.service_name)
  name: string;
  // Slug and provider used to build the detail-page href
  slug?: string;
  provider?: { name: string; handle: string } | null;
  is_owner?: boolean;
  is_public?: boolean;
  base_url?: string;
  servers?: { url: string; description?: string }[];
  // Workspace-specific fields, only present for ActivatedService rows
  service_id?: string;
  service_slug?: string;
  version?: string;
};

/** Normalise a catalog Service into a ListableService. */
export function fromService(s: Service): ListableService {
  return {
    id: s.id,
    name: s.name,
    slug: s.slug,
    provider: s.provider,
    is_owner: s.is_owner,
    is_public: s.is_public,
    base_url: s.base_url,
    servers: s.servers,
  };
}

/** Normalise a workspace ActivatedService into a ListableService. */
export function fromActivatedService(s: ActivatedService): ListableService {
  return {
    id: s.service_id, // Use the Registry service_id as the stable ID
    name: s.service_name,
    slug: s.service_slug,
    service_id: s.service_id,
    service_slug: s.service_slug,
    version: s.version,
  };
}

function detailHref(service: ListableService): string {
  return serviceHref({
    id: service.service_id || service.id,
    slug: service.service_slug || service.slug,
    provider: service.provider,
    is_owner: service.is_owner ?? true,
  });
}

interface IntegrationsListTabProps {
  integrations: ListableService[];
  loading: boolean;
  error: string;
  query: string;
  setQuery: (q: string) => void;
  handleSearch: (e: FormEvent) => void;
  handleClear: () => void;
  searching: boolean;
  handleDelete: (e: React.MouseEvent, id: string) => void;
  setShowNewPanel: (show: boolean) => void;
  page: number;
  totalPages: number;
  totalItems?: number;
  onPageChange: (page: number) => void;
  viewType?: "workspace" | "catalog";
  handleAddWorkspace?: (e: React.MouseEvent, id: string, name: string) => void;
  handleRemoveWorkspace?: (e: React.MouseEvent, id: string) => void;
  activeServiceIds?: string[];
  isAuth?: boolean;
}

export default function IntegrationsListTab({
  integrations,
  loading,
  error,
  query,
  setQuery,
  handleSearch,
  handleClear,
  searching,
  handleDelete,
  setShowNewPanel,
  page,
  totalPages,
  totalItems,
  onPageChange,
  isAuth,
  viewType = "catalog",
  handleAddWorkspace,
  handleRemoveWorkspace,
  activeServiceIds = [],
}: IntegrationsListTabProps) {
  return (
    <>
      <form 
        onSubmit={handleSearch} 
        className="relative w-full mb-6"
        toolname="search_integrations"
        tooldescription="Search for existing integrations or services by name."
      >
        <button
          data-track="search_integrations"
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
          placeholder={isAuth
            ? viewType === "workspace" ? "Search your services" : "Search service catalog"
            : "Search for a service (e.g. Stripe, Shopify...)"}
          className="w-full text-sm border border-slate-300 rounded-lg pl-9 pr-8 py-2 focus:outline-none focus:ring-2 focus:ring-blue-500"
        />
        {query && (
          <button
            data-track="clear_search"
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
          <p className="animate-pulse font-medium text-slate-500">Loading services...</p>
        </div>
      ) : integrations.length === 0 ? (
        <div className="text-center py-16 text-slate-400">
          {query ? (
            <>
              <p className="text-base font-medium text-slate-600 mb-1">Service not found</p>
              <p className="text-sm text-slate-400 mb-4">
                Define it from an OpenAPI or GraphQL spec, or point Fused to its docs.
              </p>
              <button
                data-track="submit_schema_or_docs_url"
                onClick={() => setShowNewPanel(true)}
                className="px-4 py-2 bg-blue-500 hover:bg-blue-600 text-white text-sm font-medium rounded-lg transition-colors cursor-pointer"
              >
                Define this service
              </button>
            </>
          ) : isAuth ? (
            <>
              <p className="text-lg mb-2">{viewType === "workspace" ? "No services added yet" : "No services found"}</p>
              {viewType === "workspace" && (
                <p className="text-sm text-slate-400 mb-3">Add one from the catalog, or define a service from a spec or docs.</p>
              )}
              <button data-track="create_first_integration" onClick={() => setShowNewPanel(true)} className="text-blue-500 hover:text-blue-600 text-sm underline cursor-pointer">
                Define a service
              </button>
            </>
          ) : (
            <p className="text-sm text-slate-400">Search for any API service on Fused.</p>
          )}
        </div>
      ) : (
        <div className="bg-white border-y border-slate-200 divide-y divide-slate-100">
          {integrations.map((obj) => (
            <Link
              key={obj.id}
              to={detailHref(obj)}
              target="_blank"
              rel="noopener noreferrer"
              onClick={(event) => {
                if (
                  event.button !== 0 ||
                  event.metaKey ||
                  event.ctrlKey ||
                  event.shiftKey ||
                  event.altKey
                ) {
                  return;
                }
                if (openAuthenticatedTab(detailHref(obj))) {
                  event.preventDefault();
                }
              }}
              className="flex items-center justify-between px-5 py-4 hover:bg-slate-50 transition-colors group"
            >
              <div>
                <div className="flex items-center gap-2">
                  <p className="text-sm font-medium text-slate-900">{formatServiceName(obj.name)}</p>
                  {obj.is_public && (
                    <span className="px-2 py-0.5 bg-blue-100 text-blue-700 text-[10px] font-semibold rounded-full tracking-wide">
                      PUBLIC
                    </span>
                  )}
                  {viewType === "workspace" && obj.version && (
                    <span className="px-2 py-0.5 bg-slate-100 text-slate-600 text-[10px] font-medium rounded-md">
                      v{obj.version}
                    </span>
                  )}
                </div>
                <p className="text-xs text-slate-400 mt-0.5 truncate max-w-xs">
                  {(() => {
                    if (obj.servers && obj.servers.length > 0) {
                      const prodIdx = obj.servers.findIndex((s) => 
                        s.description?.toLowerCase().includes("prod") || 
                        s.description?.toLowerCase().includes("production")
                      );
                      const primaryServer = prodIdx !== -1 ? obj.servers[prodIdx] : obj.servers[0];
                      return obj.servers.length > 1 
                        ? `${obj.servers.length} Environments (Primary: ${primaryServer.url})` 
                        : primaryServer.url;
                    }
                    return obj.base_url ?? (viewType === "workspace" ? "Connected Workspace Service" : "");
                  })()}
                </p>
              </div>
              {/* Actions based on View Type */}
              <div className="flex items-center gap-2">
                {viewType === "workspace" && (
                  <button
                    data-track={obj.is_owner ? "delete_workspace_service" : "remove_workspace_service"}
                    onClick={(e) => {
                      if (obj.is_owner && handleDelete) {
                        handleDelete(e, obj.service_id || obj.id);
                      } else if (handleRemoveWorkspace) {
                        handleRemoveWorkspace(e, obj.service_id || obj.id);
                      }
                    }}
                    className={`opacity-0 group-hover:opacity-100 px-3 py-1 text-xs font-medium bg-white border rounded-lg shadow-sm transition-all ${
                      obj.is_owner 
                        ? "text-red-600 border-red-200 hover:bg-red-50" 
                        : "text-slate-600 hover:text-red-600 border-slate-200 hover:border-red-200 hover:bg-red-50"
                    }`}
                    title={obj.is_owner ? "Delete service from Registry" : "Remove from workspace"}
                  >
                    {obj.is_owner ? "Delete" : "Remove"}
                  </button>
                )}

                {viewType === "catalog" && isAuth && handleAddWorkspace && !activeServiceIds.includes(obj.id) && (
                  <button
                    data-track="add_workspace_service"
                    onClick={(e) => handleAddWorkspace(e, obj.id, obj.name)}
                    className="px-3 py-1 text-xs font-medium text-blue-700 bg-blue-50 hover:bg-blue-100 rounded-lg shadow-sm transition-all"
                    title="Add to workspace"
                  >
                    Add to workspace
                  </button>
                )}
              </div>
            </Link>
          ))}
          {/* Pagination Controls */}
          {!loading && !query && totalPages > 0 && (
            <div className="flex items-center justify-between gap-3 border-t border-slate-100 px-5 py-3 bg-white">
              <p className="text-xs text-slate-500">
                {totalItems !== undefined ? `${totalItems === 0 ? 0 : (page - 1) * 10 + 1}-${Math.min(totalItems, page * 10)} of ${totalItems}` : ""}
              </p>
              <div className="flex items-center gap-1">
                <button
                  type="button"
                  data-track="paginate_previous"
                  onClick={() => onPageChange(page - 1)}
                  disabled={page === 1}
                  className="rounded-md p-1.5 text-slate-500 hover:bg-slate-100 disabled:opacity-40 disabled:hover:bg-transparent"
                  aria-label="Previous page"
                  title="Previous"
                >
                  <ChevronLeft className="w-4 h-4" />
                </button>
                <span className="text-xs text-slate-500 pl-2">Page</span>
                <select
                  className="bg-white border border-slate-200 rounded px-2 py-1 text-xs focus:outline-none focus:ring-2 focus:ring-blue-500 mx-1 cursor-pointer"
                  value={page}
                  onChange={(e) => onPageChange(parseInt(e.target.value, 10))}
                >
                  {Array.from({ length: totalPages }, (_, i) => i + 1).map(p => (
                    <option key={p} value={p}>{p}</option>
                  ))}
                </select>
                <span className="text-xs font-medium text-slate-500 pr-2">of {totalPages}</span>
                <button
                  type="button"
                  data-track="paginate_next"
                  onClick={() => onPageChange(page + 1)}
                  disabled={page >= totalPages}
                  className="rounded-md p-1.5 text-slate-500 hover:bg-slate-100 disabled:opacity-40 disabled:hover:bg-transparent"
                  aria-label="Next page"
                  title="Next"
                >
                  <ChevronRight className="w-4 h-4" />
                </button>
              </div>
            </div>
          )}
        </div>
      )}
    </>
  );
}
