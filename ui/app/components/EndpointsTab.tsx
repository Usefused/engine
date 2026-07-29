import { ChevronDown, ChevronRight, Loader2, Search } from "lucide-react";
import { useEffect, useRef } from "react";
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
  srv: any;
  res: ServiceGenerationResult;
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

export default function EndpointsTab({
  srv,
  res,
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
  const renderedEps: IntegrationObject[] = [];
  if (searchResults !== null) {
    searchResults.forEach(ep => renderedEps.push(ep));
  } else {
    res.service.resources?.forEach(resource => {
      const isCollapsed = !expandedResources[resource.name];
      if (!isCollapsed) {
        const eps = integrationsByResource[resource.id] || [];
        eps.forEach(ep => renderedEps.push(ep));
      }
    });
  }
  const totalEndpoints = searchResults !== null
    ? searchResults.length
    : (srv.resources?.reduce((sum: number, r: any) => sum + (r.endpointCount || 0), 0) || 0);

  return (
    <div className="bg-white rounded-xl border border-slate-200">
      <div className="px-5 py-3 border-b border-slate-100 flex items-center justify-between gap-3 flex-wrap">
        <div className="relative flex-1 sm:flex-initial sm:min-w-[250px] max-w-md">
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
              placeholder="Search endpoints..."
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
      </div>
      <div className="divide-y divide-slate-100">
        {(() => {
          if (searchResults !== null) {
            if (searchResults.length === 0) {
              return <div className="p-8 text-center text-slate-500 text-sm">No endpoints found.</div>;
            }
            const grouped = searchResults.reduce((acc, ep) => {
              const res = ep.resource || "General";
              if (!acc[res]) acc[res] = [];
              acc[res].push(ep);
              return acc;
            }, {} as Record<string, IntegrationObject[]>);

            const elements = Object.entries(grouped).map(([resource, eps]) => {
              const availableVersions = Array.from(new Set(eps.map(ep => ep.version || "v1"))).sort().reverse();
              const currentVersion = resourceVersions[resource] || availableVersions[0];
              const filteredEps = eps.filter(ep => (ep.version || "v1") === currentVersion);

              return (
                <div key={resource} className="mb-2">
                  <div className="bg-slate-50 px-5 py-2.5 border-y border-slate-100 mt-2 flex justify-between items-center select-none">
                    <div className="flex items-center gap-2 flex-wrap">
                      <ChevronDown className="w-4 h-4 text-slate-400" />
                      <h3 className="text-xs font-semibold text-slate-600 uppercase tracking-wider">{resource}</h3>
                    </div>
                    {availableVersions.length > 1 && (
                      <select
                        value={currentVersion}
                        onChange={(e) => setResourceVersions(prev => ({ ...prev, [resource]: e.target.value }))}
                        className="text-xs border-slate-200 rounded-md py-1 pl-2 pr-6 text-slate-600 focus:ring-blue-500 focus:border-blue-500 bg-white shadow-sm cursor-default"
                      >
                        {availableVersions.map(v => (
                          <option key={v} value={v}>{v}</option>
                        ))}
                      </select>
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
            return resources.map(resource => {
              const isCollapsed = !expandedResources[resource.name];
              const eps = integrationsByResource[resource.id] || [];
              const isLoading = loadingResources[resource.id];

              const availableVersions = Array.from(new Set(eps.map(ep => ep.version || "v1"))).sort().reverse();
              const currentVersion = resourceVersions[resource.name] || availableVersions[0];
              const filteredEps = eps.filter(ep => (ep.version || "v1") === currentVersion);

              return (
                <div key={resource.id} className="mb-2">
                  <div
                    className="bg-slate-50 px-5 py-2.5 border-y border-slate-100 mt-2 flex justify-between items-center cursor-pointer hover:bg-slate-100 transition-colors select-none"
                    onClick={() => toggleResource(resource.id, resource.name)}
                  >
                    <div className="flex items-center gap-2 flex-wrap">
                      {isCollapsed ? <ChevronRight className="w-4 h-4 text-slate-400" /> : <ChevronDown className="w-4 h-4 text-slate-400" />}
                      <h3 className="text-xs font-semibold text-slate-600 uppercase tracking-wider">{resource.name}</h3>
                      {isLoading && <span className="text-xs text-blue-500 ml-2 animate-pulse">Fetching...</span>}
                      {!isLoading && (
                        <span className="text-xs text-slate-400 ml-2">
                          ({!isCollapsed && availableVersions.length > 1 ? `${filteredEps.length} of ${resource.endpointCount || 0}` : (resource.endpointCount || 0)})
                        </span>
                      )}
                      
                      {!isCollapsed && availableVersions.length > 1 && (
                        <select
                          value={currentVersion}
                          onClick={(e) => e.stopPropagation()}
                          onChange={(e) => setResourceVersions(prev => ({ ...prev, [resource.name]: e.target.value }))}
                          className="ml-2 text-xs border border-slate-200 rounded-md py-1 pl-2 pr-6 text-slate-600 focus:ring-blue-500 focus:border-blue-500 bg-white shadow-sm cursor-default"
                        >
                          {availableVersions.map(v => (
                            <option key={v} value={v}>{v}</option>
                          ))}
                        </select>
                      )}
                    </div>
                    
                  </div>
                  {!isCollapsed && (
                    <div className="divide-y divide-slate-50">
                      {filteredEps.map(ep => (
                        <EndpointRow 
                          key={ep.name} 
                          ep={ep} 
                          onClick={() => setSelectedEndpoint(ep)} 
                          selectable={false}
                        />
                      ))}
                      {isLoading && filteredEps.length === 0 && (
                        <div className="px-5 py-8 text-center text-xs text-slate-400 flex items-center justify-center">
                          <Loader2 className="w-4 h-4 animate-spin mr-2" />
                          Loading endpoints...
                        </div>
                      )}
                      {!isLoading && filteredEps.length === 0 && <div className="px-5 py-4 text-xs text-slate-400">No endpoints found for this resource.</div>}
                      {hasMoreResources[resource.id] && (
                        <ObserverTarget 
                          disabled={loadingResources[resource.id]} 
                          onIntersect={() => loadMoreEndpoints(resource.id, resource.name)} 
                        />
                      )}
                    </div>
                  )}
                </div>
              );
            });
          }
        })()}
      </div>
    </div>
  );
}
