import { useState, useEffect } from "react";
import { api, type IntegrationObject } from "~/lib/api";
import { useToast } from "~/components/Toast";

const RESOURCE_GQL = `
  query($resourceId: String!, $serviceId: String!, $serviceVersionId: String!, $limit: Int, $offset: Int) {
    resourceIntegrations(resourceId: $resourceId, serviceId: $serviceId, service_version_id: $serviceVersionId, limit: $limit, offset: $offset) {
      id service_id name description version status method path deprecated
    }
  }
`;

// useResourceLoader pages operations for one immutable service version.
export function useResourceLoader(
  serviceId: string | undefined,
  serviceVersionId?: string
) {
  const toast = useToast();
  const [resourceVersions, setResourceVersions] = useState<Record<string, string>>({});
  const [expandedResources, setExpandedResources] = useState<Record<string, boolean>>({});
  const [integrationsByResource, setIntegrationsByResource] = useState<Record<string, IntegrationObject[]>>({});
  const [loadingResources, setLoadingResources] = useState<Record<string, boolean>>({});
  const [resourceOffsets, setResourceOffsets] = useState<Record<string, number>>({});
  const [hasMoreResources, setHasMoreResources] = useState<Record<string, boolean>>({});

  // Reset accordion state whenever the service or version changes so stale
  // expansion state from a previous service/version doesn't linger.
  useEffect(() => {
    setIntegrationsByResource({});
    setExpandedResources({});
  }, [serviceId, serviceVersionId]);

  // toggleResource expands a resource and loads its first exact-version page.
  async function toggleResource(resourceId: string, resourceName: string) {
    const isExpanding = !expandedResources[resourceName];
    setExpandedResources(prev => ({ ...prev, [resourceName]: isExpanding }));

    if (isExpanding && serviceVersionId && !integrationsByResource[resourceId] && !loadingResources[resourceId]) {
      setLoadingResources(prev => ({ ...prev, [resourceId]: true }));
      try {
        const data = await api.graphql<{ resourceIntegrations: IntegrationObject[] }>(
          RESOURCE_GQL, { resourceId, serviceId, serviceVersionId, limit: 500, offset: 0 }
        );
        const enriched = data.resourceIntegrations.map(ep => ({ ...ep, resource: resourceName }));
        setIntegrationsByResource(prev => ({ ...prev, [resourceId]: enriched }));
        setResourceOffsets(prev => ({ ...prev, [resourceId]: 500 }));
        setHasMoreResources(prev => ({ ...prev, [resourceId]: data.resourceIntegrations.length === 500 }));
      } catch (e) {
        toast.error("Failed to load integrations for resource: " + (e instanceof Error ? e.message : "Unknown error"));
      } finally {
        setLoadingResources(prev => ({ ...prev, [resourceId]: false }));
      }
    }
  }

  // loadMoreEndpoints appends the next exact-version resource page.
  async function loadMoreEndpoints(resourceId: string, resourceName: string) {
    if (!serviceVersionId || loadingResources[resourceId] || !hasMoreResources[resourceId]) return;
    setLoadingResources(prev => ({ ...prev, [resourceId]: true }));
    try {
      const currentOffset = resourceOffsets[resourceId] || 0;
      const data = await api.graphql<{ resourceIntegrations: IntegrationObject[] }>(
        RESOURCE_GQL, { resourceId, serviceId, serviceVersionId, limit: 500, offset: currentOffset }
      );
      const enriched = data.resourceIntegrations.map(ep => ({ ...ep, resource: resourceName }));
      setIntegrationsByResource(prev => ({ ...prev, [resourceId]: [...(prev[resourceId] || []), ...enriched] }));
      setResourceOffsets(prev => ({ ...prev, [resourceId]: currentOffset + 500 }));
      setHasMoreResources(prev => ({ ...prev, [resourceId]: data.resourceIntegrations.length === 500 }));
    } catch (e) {
      toast.error("Failed to load more endpoints: " + (e instanceof Error ? e.message : "Unknown error"));
    } finally {
      setLoadingResources(prev => ({ ...prev, [resourceId]: false }));
    }
  }

  // resetResources clears loaded operations while preserving hook identity.
  function resetResources() {
    setIntegrationsByResource({});
    setExpandedResources({});
  }

  return {
    resourceVersions, setResourceVersions,
    expandedResources,
    integrationsByResource, setIntegrationsByResource,
    loadingResources,
    hasMoreResources,
    toggleResource,
    loadMoreEndpoints,
    resetResources,
  };
}
