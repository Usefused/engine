import { useState, useRef, useEffect } from "react";
import { ChevronDown, ChevronRight, Search, X, Loader2 } from "lucide-react";
import { type IntegrationObject } from "~/lib/api";
import { stripLinks } from "~/lib/format";

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

export interface EndpointSelectionListProps {
  endpoints: IntegrationObject[];
  selectedIds: Set<string>;
  isSelectAll?: boolean;
  onToggle: (id: string, selected: boolean) => void;
  getId: (ep: IntegrationObject) => string;
  maxHeightClass?: string;
  disabled?: boolean;
  hasMoreResources?: Record<string, boolean>;
  onLoadMoreResource?: (resourceName: string) => void;
  // For lazy loading: resource stubs to show before any endpoints are loaded
  resourceMetadata?: Array<{ id: string; name: string }>;
  loadingResources?: Record<string, boolean>;
  onResourceExpand?: (resourceId: string, resourceName: string) => void;
}

export default function EndpointSelectionList({
  endpoints,
  selectedIds,
  isSelectAll = false,
  onToggle,
  getId,
  maxHeightClass = "max-h-[300px]",
  disabled = false,
  hasMoreResources = {},
  onLoadMoreResource,
  resourceMetadata = [],
  loadingResources = {},
  onResourceExpand,
}: EndpointSelectionListProps) {
  const [expandedResources, setExpandedResources] = useState<Record<string, boolean>>({});
  const [searchTerm, setSearchTerm] = useState("");

  const deriveResourceLabel = (resource: string | undefined, path: string | undefined) => {
    const trimmed = resource?.trim();
    if (trimmed) return trimmed;

    const segments = (path || "").split("/").filter(Boolean);
    const candidate = segments.find((segment) => !segment.startsWith("{") && !segment.startsWith(":")) || segments[0];
    if (!candidate) return "Default";

    return candidate.replace(/[-_]/g, " ");
  };

  const filtered = endpoints.filter(ep => {
    if (!searchTerm.trim()) return true;
    const term = searchTerm.toLowerCase();
    return (
      ep.path?.toLowerCase().includes(term) ||
      ep.name?.toLowerCase().includes(term) ||
      ep.description?.toLowerCase().includes(term) ||
      ep.method?.toLowerCase().includes(term) ||
      ep.resource?.toLowerCase().includes(term)
    );
  });

  const grouped = filtered.reduce((acc, ep) => {
    const res = deriveResourceLabel(ep.resource, ep.path);
    if (!acc[res]) acc[res] = [];
    acc[res].push(ep);
    return acc;
  }, {} as Record<string, IntegrationObject[]>);

  const baseOrder = Array.from(new Set([
    ...resourceMetadata.map(r => r.name),
    ...Object.keys(grouped)
  ]));

  const allResourceNames = baseOrder.filter(resource => {
    if (!searchTerm.trim()) return true;
    const term = searchTerm.toLowerCase();
    const hasMatchingEndpoints = grouped[resource] && grouped[resource].length > 0;
    const nameMatches = resource.toLowerCase().includes(term);
    return hasMatchingEndpoints || nameMatches;
  });

  const handleResourceClick = (resourceName: string) => {
    const willExpand = !expandedResources[resourceName];
    setExpandedResources(prev => ({ ...prev, [resourceName]: willExpand }));
    if (willExpand && !grouped[resourceName] && onResourceExpand) {
      const meta = resourceMetadata.find(r => r.name === resourceName);
      if (meta) onResourceExpand(meta.id, meta.name);
    }
  };

  return (
    <div className="border border-slate-200 rounded-lg bg-white overflow-hidden flex flex-col shadow-sm">
      {/* Search Input */}
      <div className="relative border-b border-slate-100 bg-slate-50/50 p-2 flex items-center">
        <Search className="absolute left-4 w-4 h-4 text-slate-400 pointer-events-none" />
        <input
          type="text"
          value={searchTerm}
          onChange={(e) => setSearchTerm(e.target.value)}
          placeholder="Search operations..."
          className="w-full pl-9 pr-8 py-1.5 bg-white border border-slate-200 rounded-md text-xs placeholder:text-slate-400 focus:outline-none focus:ring-1 focus:ring-blue-500 focus:border-blue-500 transition-all text-slate-800"
        />
        {searchTerm && (
          <button
            data-track="clear_endpoint_search"
            type="button"
            onClick={() => setSearchTerm("")}
            className="absolute right-4 text-slate-400 hover:text-slate-600 focus:outline-none cursor-pointer flex items-center"
          >
            <X className="w-3.5 h-3.5" />
          </button>
        )}
      </div>

      {/* List */}
      <div className={`overflow-y-auto divide-y divide-slate-100 flex-1 ${maxHeightClass}`}>
        {allResourceNames.length === 0 ? (
          <div className="p-8 text-center text-sm text-slate-400">
            No matching endpoints found
          </div>
        ) : (
          allResourceNames.map((resource) => {
            const eps = grouped[resource] || [];
            const isCollapsed = searchTerm ? false : !expandedResources[resource];
            const isLoading = loadingResources[resource];
            const isStub = eps.length === 0 && !isLoading;

            return (
              <div key={resource} className="bg-white">
                <div
                  className="px-3 py-2 bg-slate-100 border-y border-slate-200 first:border-t-0 flex items-center justify-between sticky top-0 cursor-pointer hover:bg-slate-200/70 transition-colors select-none z-10"
                  onClick={() => handleResourceClick(resource)}
                >
                  <div className="flex items-center gap-1.5">
                    {isCollapsed ? <ChevronRight className="w-3.5 h-3.5 text-slate-400" /> : <ChevronDown className="w-3.5 h-3.5 text-slate-400" />}
                    <span className="text-xs font-bold text-slate-600 uppercase tracking-wider">{resource}</span>
                  </div>
                  {isLoading && <Loader2 className="w-3.5 h-3.5 animate-spin text-slate-400" />}
                </div>
                {!isCollapsed && (
                  <div className="divide-y divide-slate-100">
                    {isLoading ? (
                      <div className="flex items-center justify-center gap-2 py-4 text-slate-400">
                        <Loader2 className="w-3.5 h-3.5 animate-spin" />
                        <span className="text-xs">Loading...</span>
                      </div>
                    ) : isStub ? (
                      <div className="px-4 py-3 text-xs text-slate-400">Expand to load endpoints</div>
                    ) : (
                      eps.map((ep, idx) => {
                        const id = getId(ep);
                        const isSelected = isSelectAll || selectedIds.has(id);
                        return (
                          <label
                            key={`${id}-${idx}`}
                            className={`flex items-start gap-3 p-3 cursor-pointer transition-colors ${isSelected ? 'bg-blue-50/40' : 'hover:bg-slate-50'}`}
                          >
                            <input
                              type="checkbox"
                              checked={isSelected}
                              disabled={disabled || isSelectAll}
                              onChange={(e) => onToggle(id, e.target.checked)}
                              className="mt-1 flex-shrink-0 w-4 h-4 text-blue-600 rounded border-slate-300 focus:ring-blue-500 disabled:opacity-50 cursor-pointer disabled:cursor-not-allowed"
                            />
                            <div className="flex-1 min-w-0">
                              <div className="flex items-center gap-2 mb-1 font-sans">
                                <span className={`text-[10px] font-bold px-1.5 py-0.5 rounded shadow-sm ${
                                  ep.method === "GET" ? "bg-green-100 text-green-700 border border-green-200" :
                                  ep.method === "POST" ? "bg-blue-100 text-blue-700 border border-blue-200" :
                                  ep.method === "DELETE" ? "bg-red-100 text-red-700 border border-red-200" :
                                  ep.method === "PUT" || ep.method === "PATCH" ? "bg-amber-100 text-amber-700 border border-amber-200" :
                                  "bg-slate-100 text-slate-700 border border-slate-200"
                                }`}>
                                  {ep.method}
                                </span>
                                <span className={`text-sm font-medium truncate ${isSelected ? 'text-blue-900' : 'text-slate-900'}`}>
                                  {ep.path}
                                </span>
                                {ep.deprecated || ep.deprecation_date ? (
                                  <span title="Deprecated" className="ml-1 text-red-500">
                                    <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3Z"></path><line x1="12" y1="9" x2="12" y2="13"></line><line x1="12" y1="17" x2="12.01" y2="17"></line></svg>
                                  </span>
                                ) : null}
                              </div>
                              <div className="text-xs text-slate-500 truncate [&_p]:inline [&_code]:bg-slate-100 [&_code]:px-1 [&_code]:py-0.5 [&_code]:rounded">
                                {ep.name} {ep.description ? <span> - <span dangerouslySetInnerHTML={{ __html: stripLinks(ep.description) }} /></span> : ""}
                              </div>
                              {ep.responses && Object.keys(ep.responses).length > 0 && (
                                <div className="mt-1.5 flex flex-wrap gap-1">
                                  {Object.keys(ep.responses).map(code => (
                                    <span key={code} title={`Response schema captured for HTTP ${code}`} className={`text-[9px] font-bold px-1.5 py-0.5 rounded flex items-center gap-1 ${
                                        code.startsWith('2') ? 'bg-emerald-50 text-emerald-600 border border-emerald-100' :
                                        code.startsWith('4') || code.startsWith('5') ? 'bg-rose-50 text-rose-600 border border-rose-100' :
                                        'bg-slate-50 text-slate-600 border border-slate-200'
                                    }`}>
                                      <div className={`w-1.5 h-1.5 rounded-full ${
                                        code.startsWith('2') ? 'bg-emerald-400' :
                                        code.startsWith('4') || code.startsWith('5') ? 'bg-rose-400' :
                                        'bg-slate-400'
                                      }`}></div>
                                      {code}
                                    </span>
                                  ))}
                                </div>
                              )}
                            </div>
                          </label>
                        );
                      })
                    )}
                    {!isLoading && !isStub && hasMoreResources[resource] && onLoadMoreResource && (
                      <ObserverTarget
                        disabled={false}
                        onIntersect={() => onLoadMoreResource(resource)}
                      />
                    )}
                  </div>
                )}
              </div>
            );
          })
        )}
      </div>
    </div>
  );
}
