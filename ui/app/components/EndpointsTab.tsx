import { filterOperationMethods, operationMethods } from "~/lib/operation-filter";
import { ServiceCatalogHeader, ServiceCatalogOptions } from "~/components/integration-details/ServiceCatalogHeader";
import { Select } from "./forms/Select.ts";
import { ChevronDown, ChevronRight, Loader2, Search } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import type { IntegrationObject, ServiceGenerationResult } from "~/lib/api";
import { EndpointRow } from "~/components/EndpointRow";

const ObserverTarget = ({ onIntersect, disabled }: { onIntersect: () => void, disabled: boolean }) => {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (disabled || !ref.current) return;
    const observer = new IntersectionObserver(
      ([entry]) => {
        if (entry.isIntersecting) {
          onIntersect();
        }
      },
      { rootMargin: "100px" }
    );
    observer.observe(ref.current);
    return () => observer.disconnect();
  }, [onIntersect, disabled]);

  return <div ref={ref} className="h-4 w-full flex justify-center items-center py-2">{!disabled && <Loader2 className="w-4 h-4 animate-spin text-slate-400" />}</div>;
};

interface EndpointsTabProps {
  res: ServiceGenerationResult;
  onImport?: () => void;
  searchQuery: string;
  setSearchQuery: (q: string) => void;
  searchResults: IntegrationObject[] | null;
  isSearching: boolean;
  handleSearch: () => void;
  handleClearSearch: () => void;
  resourceVersions: Record<string, string>;
  setResourceVersions: React.Dispatch<React.SetStateAction<Record<string, string>>>;
  expandedResources: Record<string, boolean>;
  integrationsByResource: Record<string, IntegrationObject[]>;
  loadingResources: Record<string, boolean>;
  toggleResource: (resourceId: string, resourceName: string) => void;
  hasMoreResources: Record<string, boolean>;
  loadMoreEndpoints: (resourceId: string, resourceName: string) => void;
  hasMoreSearch: boolean;
  loadMoreSearchResults: () => void;
  selectedEndpoint: IntegrationObject | null;
  setSelectedEndpoint: (ep: IntegrationObject | null) => void;
}

/** Combines method filtering with version-pinned resource browsing and owner imports. */
export default function EndpointsTab({
  res,
  onImport,
  searchQuery,
  setSearchQuery,
  searchResults,
  isSearching,
  handleSearch,
  handleClearSearch,
  resourceVersions,
  setResourceVersions,
  expandedResources,
  integrationsByResource,
  loadingResources,
  toggleResource,
  hasMoreResources,
  loadMoreEndpoints,
  hasMoreSearch,
  loadMoreSearchResults,
  setSelectedEndpoint,
}: EndpointsTabProps) {
  const [method, setMethod] = useState("all");
  // A method selected for one service version must not silently constrain another contract.
  useEffect(() => setMethod("all"), [res.service.id, res.service.current_service_version]);
  const loadedMethods = Object.values(integrationsByResource).flat().map((endpoint) => endpoint.method);
  // Preserve uncommon protocol labels already present in the catalog alongside standard HTTP methods.
  const methodOptions = Array.from(new Set([...operationMethods, ...loadedMethods, ...(searchResults ?? []).map((endpoint) => endpoint.method)].filter(Boolean).map((value) => value!.toUpperCase())));

  return (
    <div className="min-w-0 rounded-xl border border-slate-200 bg-white">
      <ServiceCatalogHeader title="Explore operations" description="Browse API endpoints available for this service.">
        {/* Only the owning service route can provide the endpoint import command. */}
        {onImport && <ServiceCatalogOptions actions={[{ label: "Import endpoints", onSelect: onImport }]} />}
      </ServiceCatalogHeader>
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-slate-100 px-4 py-3 sm:px-5">
        <div className="relative w-full min-w-0 sm:w-auto sm:min-w-[250px] sm:flex-1">
            <button
              data-track="search_endpoints"
              onClick={handleSearch}
              disabled={isSearching}
              className="absolute left-2.5 top-1/2 -translate-y-1/2 text-slate-400 hover:text-slate-600 disabled:opacity-50 cursor-pointer"
              title="Search"
            >
              {isSearching ? (
                <Loader2 className="w-3.5 h-3.5 animate-spin" />
              ) : (
                <Search className="w-3.5 h-3.5" />
              )}
            </button>
            <input
              type="text"
              aria-label="Search operations"
              placeholder="Search operations..."
              value={searchQuery}
              onChange={(e) => {
                setSearchQuery(e.target.value);
                if (e.target.value === "") handleClearSearch();
              }}
              onKeyDown={(e) => {
                if (e.key === "Enter") handleSearch();
              }}
              className="w-full text-sm border border-slate-300 rounded-md pl-9 pr-8 py-1.5 focus:outline-none focus:border-slate-500 focus:ring-1 focus:ring-gray-500"
            />
            {searchQuery && (
              <button
                data-track="clear_endpoint_search"
                onClick={handleClearSearch}
                className="absolute right-2.5 top-1/2 -translate-y-1/2 text-slate-400 hover:text-slate-600 cursor-pointer"
              >
                ✕
              </button>
            )}
          </div>
        <Select aria-label="Operation type" density="compact" value={method} onChange={(event) => setMethod(event.target.value)} className="w-full text-xs text-slate-600 sm:w-auto"><option value="all">All types</option>{methodOptions.map((value) => <option key={value} value={value}>{value}</option>)}</Select>
      </div>
      <div className="divide-y divide-slate-100">
        {/* Resource and search views share controls while retaining their distinct loading states. */}
        {(() => {
          if (searchResults !== null) {
            const matchingResults = filterOperationMethods(searchResults, method);
            // An empty filtered page must still expose pagination so later matching operations can load.
            if (matchingResults.length === 0) {
              return <><div className="p-8 text-center text-slate-500 text-sm">No matching operations in the loaded results.</div><ObserverTarget disabled={!hasMoreSearch || isSearching} onIntersect={loadMoreSearchResults} /></>;
            }
            const grouped = matchingResults.reduce((acc, ep) => {
              const res = ep.resource || "General";
              if (!acc[res]) acc[res] = [];
              acc[res].push(ep);
              return acc;
            }, {} as Record<string, IntegrationObject[]>);

            // Each resource keeps its version selection while sharing the dropdown presentation.
            const elements = Object.entries(grouped).map(([resource, eps]) => {
              const availableVersions = Array.from(new Set(eps.map(ep => ep.version || "v1"))).sort().reverse();
              const currentVersion = resourceVersions[resource] || availableVersions[0];
              // Method filtering preserves each resource's independently selected version.
              const filteredEps = filterOperationMethods(eps.filter(ep => (ep.version || "v1") === currentVersion), method);

              return (
                <div key={resource} className="mb-2">
                  <div className="mt-2 flex min-w-0 items-center justify-between gap-2 border-y border-slate-100 bg-slate-50 px-4 py-2.5 select-none sm:px-5">
                    <div className="flex min-w-0 flex-wrap items-center gap-2">
                      <ChevronDown className="w-4 h-4 text-slate-400" />
                      <h3 className="min-w-0 break-words text-xs font-semibold text-slate-600 uppercase tracking-wider">{resource}</h3>
                    </div>
                    {availableVersions.length > 1 && (
                      <Select
                        value={currentVersion}
                        onChange={(e) => setResourceVersions(prev => ({ ...prev, [resource]: e.target.value }))}
                        className="text-xs border-slate-200 rounded-md py-1 pl-2 pr-6 text-slate-600 focus:ring-blue-500 focus:border-blue-500 bg-white shadow-sm cursor-default"
                      >
                        {availableVersions.map(v => (
                          <option key={v} value={v}>{v}</option>
                        ))}
                      </Select>
                    )}
                  </div>
                  <div className="divide-y divide-slate-50">
                    {filteredEps.map((ep) => (
                        <EndpointRow 
                        key={ep.id} 
                        ep={ep} 
                        onClick={() => setSelectedEndpoint(ep)} 
                        selectable={false}
                      />
                    ))}
                  </div>
                </div>
              );
            });
            return (
              <>
                {elements}
                <ObserverTarget 
                  disabled={!hasMoreSearch || isSearching} 
                  onIntersect={loadMoreSearchResults} 
                />
              </>
            );
          } else {
            const resources = res.service.resources || [];
            if (resources.length === 0) {
              return <div className="p-8 text-center text-slate-500 text-sm">No resources found.</div>;
            }
            // Version changes stay local to each resource so other groups remain undisturbed.
            return resources.map(resource => {
              const isCollapsed = !expandedResources[resource.name];
              const eps = integrationsByResource[resource.id] || [];
              const isLoading = loadingResources[resource.id];

              const availableVersions = Array.from(new Set(eps.map(ep => ep.version || "v1"))).sort().reverse();
              const currentVersion = resourceVersions[resource.name] || availableVersions[0];
              // Method filtering preserves each resource's independently selected version.
              const filteredEps = filterOperationMethods(eps.filter(ep => (ep.version || "v1") === currentVersion), method);

              return (
                <div key={resource.id} className="mb-2">
                  <div
                    className="mt-2 flex min-w-0 items-center justify-between gap-2 border-y border-slate-100 bg-slate-50 px-4 py-2.5 cursor-pointer transition-colors select-none hover:bg-slate-100 sm:px-5"
                    onClick={() => toggleResource(resource.id, resource.name)}
                  >
                    <div className="flex min-w-0 flex-wrap items-center gap-2">
                      {isCollapsed ? <ChevronRight className="w-4 h-4 text-slate-400" /> : <ChevronDown className="w-4 h-4 text-slate-400" />}
                      <h3 className="min-w-0 break-words text-xs font-semibold text-slate-600 uppercase tracking-wider">{resource.name}</h3>
                      {isLoading && <span className="text-xs text-blue-500 ml-2 animate-pulse">Fetching...</span>}
                      {!isLoading && (
                        <span className="text-xs text-slate-400 ml-2">
                          ({resourceCountLabel(isCollapsed, availableVersions.length, filteredEps.length, resource.endpointCount, method)})
                        </span>
                      )}
                      
                      {!isCollapsed && availableVersions.length > 1 && (
                        <Select
                          value={currentVersion}
                          onClick={(e) => e.stopPropagation()}
                          onChange={(e) => setResourceVersions(prev => ({ ...prev, [resource.name]: e.target.value }))}
                          className="ml-2 text-xs border border-slate-200 rounded-md py-1 pl-2 pr-6 text-slate-600 focus:ring-blue-500 focus:border-blue-500 bg-white shadow-sm cursor-default"
                        >
                          {availableVersions.map(v => (
                            <option key={v} value={v}>{v}</option>
                          ))}
                        </Select>
                      )}
                    </div>
                    
                  </div>
                  <ResourceEndpointRows expanded={!isCollapsed} endpoints={filteredEps} loading={isLoading} hasMore={hasMoreResources[resource.id]} onSelect={setSelectedEndpoint} onLoadMore={() => loadMoreEndpoints(resource.id, resource.name)} />
                </div>
              );
            });
          }
        })()}
      </div>
    </div>
  );
}

/** Keeps filtered counts distinct from the provider's total without implying unloaded rows were inspected. */
function resourceCountLabel(collapsed: boolean, versionCount: number, visibleCount: number, total: number | undefined, method: string) {
  // A collapsed group has not necessarily loaded its first page, so its authoritative total remains useful.
  if (collapsed) return total || 0;
  // Method filtering reports only matching rows already available in this resource's current page set.
  if (method !== "all") return `${visibleCount} shown`;
  // Multiple endpoint versions retain the existing visible-versus-total explanation.
  return versionCount > 1 ? `${visibleCount} of ${total || 0}` : (total || 0);
}

/** Preserves pagination even when a method filter hides every operation in the currently loaded pages. */
function ResourceEndpointRows({ expanded, endpoints, loading, hasMore, onSelect, onLoadMore }: { expanded: boolean; endpoints: IntegrationObject[]; loading: boolean; hasMore: boolean; onSelect: (endpoint: IntegrationObject) => void; onLoadMore: () => void }) {
  // Collapsed groups retain their loaded state without mounting row or pagination observers.
  if (!expanded) return null;
  return <div className="divide-y divide-slate-50">
    {endpoints.map((endpoint) => <EndpointRow key={endpoint.id} ep={endpoint} onClick={() => onSelect(endpoint)} selectable={false} />)}
    {/* A loading page must not temporarily claim the resource has no matches. */}
    {loading && endpoints.length === 0 && <div className="flex items-center justify-center px-5 py-8 text-center text-xs text-slate-400"><Loader2 className="mr-2 h-4 w-4 animate-spin" />Loading endpoints...</div>}
    {/* Empty copy describes loaded rows only; later pages remain reachable below. */}
    {!loading && endpoints.length === 0 && <div className="px-5 py-4 text-xs text-slate-400">No matching operations loaded for this resource.</div>}
    {/* Filtering must never stop the existing loader before unseen pages are exhausted. */}
    {hasMore && <ObserverTarget disabled={loading} onIntersect={onLoadMore} />}
  </div>;
}
