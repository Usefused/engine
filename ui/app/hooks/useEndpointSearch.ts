import { useState, useEffect, useRef } from "react";
import { useSearchParams } from "@remix-run/react";
import { api, type IntegrationObject } from "~/lib/api";
import { useToast } from "~/components/Toast";

export function useEndpointSearch(
  serviceId: string | undefined,
  res: any,
  integrationsByResource: Record<string, IntegrationObject[]>
) {
  const toast = useToast();
  const [searchParams, setSearchParams] = useSearchParams();
  const [searchQuery, setSearchQuery] = useState(searchParams.get("q") || "");
  const [isSearching, setIsSearching] = useState(false);
  const [searchResults, setSearchResults] = useState<IntegrationObject[] | null>(null);
  const [searchOffset, setSearchOffset] = useState(0);
  const [hasMoreSearch, setHasMoreSearch] = useState(false);

  const hasTriggeredSearch = useRef(false);

  useEffect(() => {
    const qParam = searchParams.get("q");
    if (res && !hasTriggeredSearch.current) {
      hasTriggeredSearch.current = true;
      if (qParam) {
        handleSearch(qParam);
      }
    }
  }, [res, searchParams]);

  useEffect(() => {
    if (!res || (searchQuery === (searchParams.get("q") || "") && hasTriggeredSearch.current)) return;
    const timeoutId = setTimeout(() => handleSearch(searchQuery), 400);
    return () => clearTimeout(timeoutId);
  }, [searchQuery, res]);

  async function handleSearch(overrideQuery?: string) {
    const q = overrideQuery !== undefined ? overrideQuery : searchQuery;
    if (!serviceId || !res) return;
    if (!q.trim()) {
      setSearchResults(null);
      if (searchParams.has("q")) {
        setSearchParams(prev => { prev.delete("q"); return prev; }, { replace: true, preventScrollReset: true });
      }
      return;
    }
    if (searchParams.get("q") !== q) {
      setSearchParams(prev => { prev.set("q", q); return prev; }, { replace: true, preventScrollReset: true });
    }
    setIsSearching(true);
    setSearchOffset(0);

    const lowerQ = q.toLowerCase();
    const allLoaded = Object.values(integrationsByResource).flat();
    const localMatches = allLoaded.filter(ep =>
      (ep.name && ep.name.toLowerCase().includes(lowerQ)) ||
      (ep.path && ep.path.toLowerCase().includes(lowerQ)) ||
      (ep.method && ep.method.toLowerCase().includes(lowerQ)) ||
      (ep.description && ep.description.toLowerCase().includes(lowerQ)) ||
      (ep.resource && ep.resource.toLowerCase().includes(lowerQ))
    );

    if (localMatches.length > 0) {
      setSearchResults(localMatches);
      setHasMoreSearch(false);
      setIsSearching(false);
      return;
    }

    try {
      const data = await api.graphql<{ searchEndpoints: any[] }>(`
        query($serviceId: String!, $q: String!, $limit: Int, $offset: Int) {
          searchEndpoints(serviceId: $serviceId, q: $q, limit: $limit, offset: $offset) {
            id service_id name description version status method path deprecated
          }
        }
      `, { serviceId, q, limit: 50, offset: 0 });
      const enriched = data.searchEndpoints.map(ep => {
        const existing = allLoaded.find(i => i.id === ep.id);
        return existing ? { ...ep, resource: existing.resource } : { ...ep, resource: "Search Results" };
      });
      setSearchResults(enriched);
      setSearchOffset(50);
      setHasMoreSearch(data.searchEndpoints.length === 50);
    } catch (e: any) {
      toast.error(e.message || "Search failed");
    } finally {
      setIsSearching(false);
    }
  }

  async function loadMoreSearchResults() {
    if (!serviceId || !res || isSearching || !hasMoreSearch) return;
    setIsSearching(true);
    try {
      const allLoaded = Object.values(integrationsByResource).flat();
      const data = await api.graphql<{ searchEndpoints: any[] }>(`
        query($serviceId: String!, $q: String!, $limit: Int, $offset: Int) {
          searchEndpoints(serviceId: $serviceId, q: $q, limit: $limit, offset: $offset) {
            id service_id name description version status method path deprecated
          }
        }
      `, { serviceId, q: searchQuery, limit: 50, offset: searchOffset });
      const enriched = data.searchEndpoints.map(ep => {
        const existing = allLoaded.find(i => i.id === ep.id);
        return existing ? { ...ep, resource: existing.resource } : { ...ep, resource: "Search Results" };
      });
      setSearchResults(prev => [...(prev || []), ...enriched]);
      setSearchOffset(prev => prev + 50);
      setHasMoreSearch(data.searchEndpoints.length === 50);
    } catch (e: any) {
      toast.error(e.message || "Load more search failed");
    } finally {
      setIsSearching(false);
    }
  }

  function handleClearSearch() {
    setSearchQuery("");
    setSearchResults(null);
    setSearchOffset(0);
    setHasMoreSearch(false);
    setSearchParams(prev => { prev.delete("q"); return prev; }, { replace: true, preventScrollReset: true });
  }

  return {
    searchQuery,
    setSearchQuery,
    isSearching,
    searchResults,
    hasMoreSearch,
    handleSearch,
    loadMoreSearchResults,
    handleClearSearch,
  };
}
