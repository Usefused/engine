import { useState, useEffect, type FormEvent } from "react";
import { useSearchParams, useLoaderData, type MetaFunction } from "@remix-run/react";
import { redirect } from "@remix-run/react";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m) => m.id === "root").flatMap((m) => m.meta ?? []);
  return [
    ...parentMeta.filter((m) => !('title' in m)),
    { title: "Create app - Fused" },
  ];
};
import { api, type Service, type IntegrationObject, type WebhookObject, type ServiceVersion, BASE } from "~/lib/api";
import {
  listArtifactBuildSelectors,
  listArtifactOwningTeams,
  planAndApplyArtifact,
} from "~/lib/artifact-builder";
import type { ArtifactBuildSelector, ArtifactOwningTeam } from "~/lib/artifact-builder-contract";
import { getApiKey, openAuthenticatedTab } from "~/lib/session";
import { CREATE_CREDENTIAL_PATH } from "~/lib/credential-navigation";
import { useToast } from "~/components/Toast";
import { fetchEventSource } from "@microsoft/fetch-event-source";
import { CheckSquare, Square, ChevronDown, ChevronRight, Search, Loader2, X, ChevronLeft } from "lucide-react";
import EndpointSelectionList from "~/components/EndpointSelectionList";
import WebhookSelectionList from "~/components/WebhookSelectionList";
import { ConsumerGenerationPanel } from "~/components/consumer/ConsumerGenerationPanel";

export const clientLoader = async ({ request }: { request: Request }) => {
  const token = getApiKey();
  const url = new URL(request.url);
  if (!token) {
    return redirect(`/login?next=${encodeURIComponent(url.pathname + url.search)}`);
  }

  // Team-aware selectors are Engine-owned and depend on the user's chosen
  // owner. Do not broad-load Registry services before that choice exists.
  return { services: [] as Service[], total: 0, isAuth: true };
};

// The service-versions query below fetches header_value (the value clients
// send in the version-pinning header), which the shared ServiceVersion type
// doesn't declare since most callers don't need it.
type SdkServiceVersion = ServiceVersion & { header_value?: string };

type ServiceData = {
  service: Service;
  integrations: IntegrationObject[];
  webhooks: WebhookObject[];
  serviceVersions: SdkServiceVersion[];
};

type ArtifactSelection = {
  service_id: string;
  service_name?: string;
  service_slug?: string;
  select_all: boolean;
  endpoint_ids: string[];
  webhook_ids: string[];
  service_version_id?: string;
};

function artifactServicesConfig(selections: ArtifactSelection[], services: ServiceData[]): Record<string, unknown> {
  return Object.fromEntries(selections.map((selection) => artifactServiceEntry(selection, services)));
}

function artifactServiceEntry(selection: ArtifactSelection, services: ServiceData[]): [string, Record<string, unknown>] {
  const service = services.find((item) => item.service.id === selection.service_id);
  return [artifactSelectionKey(selection), {
    version: service?.serviceVersions.find((version) => version.id === selection.service_version_id)?.name,
    operations: artifactOperations(selection, service),
    webhooks: artifactWebhooks(selection, service),
    select_all: selection.select_all,
  }];
}

function artifactSelectionKey(selection: ArtifactSelection): string {
  return selection.service_slug || selection.service_name || selection.service_id;
}

function artifactOperations(selection: ArtifactSelection, service?: ServiceData): string[] {
  if (selection.select_all) return [];
  const selected = new Set(selection.endpoint_ids);
  return (service?.integrations || []).filter((endpoint) => selected.has(endpoint.id)).map((endpoint) => endpoint.name);
}

function artifactWebhooks(selection: ArtifactSelection, service?: ServiceData): string[] {
  const selected = new Set(selection.webhook_ids);
  return (service?.webhooks || []).filter((webhook) => selected.has(webhook.id)).map((webhook) => webhook.name);
}

const capitalizeFirstLetter = (value?: string | null) => {
  if (!value) return "";
  return value.charAt(0).toUpperCase() + value.slice(1);
};

async function loadRegistryServicesByIDs(serviceIds: string[]): Promise<Service[]> {
  if (serviceIds.length === 0) return [];
  const response = await api.graphql<{ servicesByIds: Service[] }>(`
    query ArtifactBuilderServices($serviceIds: [String!]!) {
      servicesByIds(serviceIds: $serviceIds) {
        id name slug canonical_ref provider { name handle } description base_url endpoint_count webhook_count
        event_extraction_path incoming_webhook_config { auth_type }
        resources { id name }
      }
    }
  `, { serviceIds });
  return response.servicesByIds || [];
}

// AddSelectedServiceToWorkspaceButton is Task 7's inline activation CTA
// (engine_workspace_registration_plan.md): mirrors integrations.$id.tsx's
// own AddToWorkspaceButton (S2), but reports success back to the parent via
// onAdded instead of just showing local "Added" state -- the parent needs
// to know so workspaceServiceIds updates and Generate re-enables once every
// selected service is activated.
function AddSelectedServiceToWorkspaceButton({
  serviceId,
  serviceName,
  versionTag,
  serviceVersionId,
  onAdded,
}: {
  serviceId: string;
  serviceName: string;
  versionTag?: string;
  serviceVersionId?: string;
  onAdded: (serviceId: string) => void;
}) {
  const [adding, setAdding] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const hasExactVersion = Boolean(versionTag && serviceVersionId);

  const handleAdd = async () => {
    if (adding || !hasExactVersion) return;
    setAdding(true);
    setError(null);
    try {
      await api.workspace.addService(serviceId, serviceName, versionTag!, serviceVersionId!);
      onAdded(serviceId);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to add service");
    } finally {
      setAdding(false);
    }
  };

  return (
    <button
      type="button"
      onClick={handleAdd}
      disabled={adding || !hasExactVersion}
      className="inline-flex items-center gap-1.5 px-2.5 py-1 text-xs font-medium bg-blue-600 hover:bg-blue-700 disabled:opacity-50 disabled:cursor-not-allowed text-white rounded-md shadow-sm transition-colors"
      data-track="sdk_builder_add_to_workspace"
      title={hasExactVersion ? `Add ${serviceName} to your workspace` : "Select a service version first"}
    >
      {adding ? "Adding..." : `+ Add ${serviceName}`}
      {error && <span className="ml-2 text-red-200 text-[10px]">{error}</span>}
    </button>
  );
}

export default function SdkBuilder() {
  const toast = useToast();
  const loaderData = useLoaderData<typeof clientLoader>();
  const [searchParams, setSearchParams] = useSearchParams();
  const isAuth = loaderData.isAuth;
  const selectedServiceParam = searchParams.get("serviceId") || searchParams.get("service") || searchParams.get("slug");
  const initialSelectedServiceId = selectedServiceParam && loaderData.services.length === 1
    ? loaderData.services[0]?.id
    : undefined;

  const [data, setData] = useState<ServiceData[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  const [query, setQuery] = useState(searchParams.get("q") ?? "");
  const [expanded, setExpanded] = useState<Record<string, boolean>>(() =>
    initialSelectedServiceId ? { [initialSelectedServiceId]: true } : {}
  );
  const [resourceOffsets, setResourceOffsets] = useState<Record<string, number>>({});
  const [hasMoreResources, setHasMoreResources] = useState<Record<string, boolean>>({});
  const [loadingResource, setLoadingResource] = useState<Record<string, boolean>>({});
  // Per-resource loading state keyed by resource name (for EndpointSelectionList)
  const [loadingResourceByName, setLoadingResourceByName] = useState<Record<string, boolean>>({});
  // Tracks which service IDs have had their integrations fetched
  const [loadedServices, setLoadedServices] = useState<Record<string, boolean>>(() =>
    initialSelectedServiceId ? { [initialSelectedServiceId]: true } : {}
  );
  const [loadingService, setLoadingService] = useState<Record<string, boolean>>({});
  // Per-service sub-section expand state
  const [expandedSections, setExpandedSections] = useState<Record<string, { endpoints: boolean; webhooks: boolean }>>({});
  const toggleSection = (serviceId: string, section: "endpoints" | "webhooks") => {
    setExpandedSections(prev => {
      const current = prev[serviceId] ?? { endpoints: true, webhooks: true };
      return { ...prev, [serviceId]: { ...current, [section]: !current[section] } };
    });
  };
  
  // Selection state: Map of Service ID -> Set of Endpoint IDs (only used when NOT select-all)
  const [selections, setSelections] = useState<Record<string, Set<string>>>({});
  // Services where ALL endpoints are selected — no need to enumerate IDs
  const [selectAllServices, setSelectAllServices] = useState<Set<string>>(new Set());
  // Webhook selection state: Map of Service ID -> Set of Webhook IDs
  const [webhookSelections, setWebhookSelections] = useState<Record<string, Set<string>>>({});
  // Service version selection state: Map of Service ID -> Service Version ID
  const [versionSelections, setVersionSelections] = useState<Record<string, string>>({});
  // Map of Service ID -> the version tag this workspace is currently pinned
  // to (Engine's fused_activations), for services already added to the
  // workspace. Used only to decide which services get their pin synced
  // after a successful generate -- see syncWorkspacePinsAfterGenerate.
  const [workspaceServiceIds, setWorkspaceServiceIds] = useState<Set<string>>(new Set());
  const [workspaceServicesLoaded, setWorkspaceServicesLoaded] = useState(false);

  const [sdkName, setSdkName] = useState("");
  const [artifactVersion, setArtifactVersion] = useState("1.0.0");
  const [ownerTeams, setOwnerTeams] = useState<ArtifactOwningTeam[]>([]);
  const [ownerTeamId, setOwnerTeamId] = useState("");
  const [availableBuckets, setAvailableBuckets] = useState<ArtifactBuildSelector[]>([]);
  const [bucketId, setBucketId] = useState("");
  const [webhookAttachment, setWebhookAttachment] = useState("");
  const [generating, setGenerating] = useState(false);
  const [generateStatus, setGenerateStatus] = useState("");
  const [sdkDeployment, setSdkDeployment] = useState<{ id: string; name: string; version: string; token: string } | null>(null);
  const [sdkTokenCopied, setSdkTokenCopied] = useState(false);
  const [mcpDeployment, setMcpDeployment] = useState<{ id: string; url: string; token: string } | null>(null);
  const [mcpTokenCopied, setMcpTokenCopied] = useState(false);
  const [isDuplicate, setIsDuplicate] = useState(false);
  const [checkingDuplicate, setCheckingDuplicate] = useState(false);
  const upgradeFrom = searchParams.get("upgrade_from");
  const [lockedSelections, setLockedSelections] = useState<Record<string, Set<string>>>({});
  const [lockedWebhookSelections, setLockedWebhookSelections] = useState<Record<string, Set<string>>>({});

  const [generationMode] = useState<"sdk" | "mcp">(() => {
    const tab = searchParams.get("tab");
    return tab === "mcp" ? "mcp" : "sdk";
  });
  const [language, setLanguage] = useState<"typescript" | "python">("typescript");

  const targetType = generationMode === "mcp" ? "mcp" : "sdk";

  useEffect(() => {
    document.title = generationMode === "mcp" ? "Create MCP server - Fused" : "Create app - Fused";
  }, [generationMode]);

  const [searching, setSearching] = useState(false);
  const pageParam = searchParams.get("page");
  const page = pageParam ? parseInt(pageParam, 10) : 1;
  const setPage = (p: number | ((prev: number) => number)) => {
    const newPage = typeof p === 'function' ? p(page) : p;
    setSearchParams(prev => {
      const newParams = new URLSearchParams(prev);
      newParams.set("page", newPage.toString());
      return newParams;
    }, { replace: true });
  };
  const [totalPages, setTotalPages] = useState(1);
  const [totalItems, setTotalItems] = useState(0);

  // Handle upgradeFrom initialization ONCE. MCP servers are excluded: they're
  // Engine-native fused_sdk_scopes rows now, not Registry sdks rows, and the
  // Engine's MCPServer GraphQL type (mcp_graphql.go) doesn't expose past
  // selections to prefill from -- there's currently no data to load an
  // "expand" flow from for MCP, so skip the doomed-to-fail fetch entirely
  // rather than firing a query that can only ever come back empty.
  useEffect(() => {
    if (!upgradeFrom || targetType === "mcp") return;
    async function loadUpgrade() {
      const sdkQuery = `
        query {
          sdks(limit: 1000, offset: 0) {
            items {
              id
              name
              description
              version
              detailed_selections {
                service_id
                endpoint_ids
                webhook_ids
                service_version_id
              }
            }
          }
        }
      `;
      try {
        type SdkSelection = {
          service_id?: string;
          endpoint_ids?: string[];
          webhook_ids?: string[];
          service_version_id?: string;
        };
        type SdkUpgradeTarget = {
          id: string;
          name?: string;
          version?: string;
          detailed_selections?: SdkSelection[];
        };
        const sdkRes = await api.graphql<{ sdks: { items: SdkUpgradeTarget[] } }>(sdkQuery);
        const targetSdk = sdkRes.sdks.items.find((s) => s.id === upgradeFrom);
        if (targetSdk) {
          setSdkName(targetSdk.name || "");
          if (targetSdk.version) {
            const parts = targetSdk.version.split('.');
            if (parts.length >= 2 && !isNaN(Number(parts[1]))) {
              parts[1] = (Number(parts[1]) + 1).toString();
              if (parts.length === 3) parts[2] = "0";
              setArtifactVersion(parts.join('.'));
            } else {
              setArtifactVersion(targetSdk.version + "-expanded");
            }
          }
          
          const ls: Record<string, Set<string>> = {};
          const lw: Record<string, Set<string>> = {};
          const lexp: Record<string, boolean> = {};
          const lv: Record<string, string> = {};
          
          const selectionsList = targetSdk.detailed_selections || [];
          selectionsList.forEach((sel) => {
            const srvId = sel.service_id;
            if (!srvId) return;
            ls[srvId] = new Set(sel.endpoint_ids || []);
            lw[srvId] = new Set(sel.webhook_ids || []);
            lexp[srvId] = true;
            if (sel.service_version_id) lv[srvId] = sel.service_version_id;
          });
          
          setLockedSelections(ls);
          setLockedWebhookSelections(lw);
          setVersionSelections(prev => ({ ...prev, ...lv }));
          setSelections(prev => {
            const next = { ...prev };
            Object.keys(ls).forEach(k => {
               if (!next[k]) next[k] = new Set();
               ls[k].forEach(id => next[k].add(id));
            });
            return next;
          });
          setWebhookSelections(prev => {
            const next = { ...prev };
            Object.keys(lw).forEach(k => {
               if (!next[k]) next[k] = new Set();
               lw[k].forEach(id => next[k].add(id));
            });
            return next;
          });
          setExpanded(prev => ({ ...prev, ...lexp }));
          toast.success("Loaded configuration from previous SDK. You can add new endpoints.");
        } else {
          toast.error("Could not find previous SDK configuration.");
        }
      } catch (e) {
        console.error("Failed to load upgrade SDK", e);
        toast.error("Failed to load SDK configuration: " + (e instanceof Error ? e.message : "Unknown error"));
      }
    }
    loadUpgrade();
  }, [upgradeFrom, targetType]);

  type RawServiceWithExtras = Omit<Service, "resources"> & {
    resources?: (NonNullable<Service["resources"]>[number] & {
      integrations?: Omit<IntegrationObject, "resource" | "resource_id">[];
    })[];
    serviceVersions?: SdkServiceVersion[];
  };

  const processResponse = (servicesData: RawServiceWithExtras[], total: number) => {
    const validResults: ServiceData[] = servicesData.map(s => {
      const integrations: IntegrationObject[] = [];
      if (s.resources) {
        s.resources.forEach(res => {
          if (res.integrations) {
            if (res.integrations.length === 50) {
              setHasMoreResources(prev => ({ ...prev, [res.id]: true }));
              setResourceOffsets(prev => ({ ...prev, [res.id]: 50 }));
            } else {
              setHasMoreResources(prev => ({ ...prev, [res.id]: false }));
            }
            res.integrations.forEach(intg => {
              integrations.push({ ...intg, resource: res.name, resource_id: res.id });
            });
          }
        });
      }
      return { service: s, integrations, webhooks: s.webhooks || [], serviceVersions: s.serviceVersions || [] };
    });

    setData(validResults);
    setTotalItems(total);

    // Make sure we have selection sets initialized for all newly loaded services
    setSelections(prev => {
      const next = { ...prev };
      validResults.forEach(r => {
        if (!next[r.service.id]) next[r.service.id] = new Set();
      });
      return next;
    });
    setWebhookSelections(prev => {
      const next = { ...prev };
      validResults.forEach(r => {
        if (!next[r.service.id]) next[r.service.id] = new Set();
      });
      return next;
    });
  };

  async function loadData(pageNum: number, search = "") {
    setLoading(true);
    setError("");
    try {
      const limit = 20;
      const selectors = await listArtifactBuildSelectors(ownerTeamId, "SERVICE", search, limit, (pageNum - 1) * limit);
      const servicesData = await loadRegistryServicesByIDs(selectors.items.map((item) => item.resource_id));
      const total = selectors.total;
      processResponse(servicesData, total);
      setTotalPages(Math.ceil(total / limit) || 1);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to load services");
    } finally {
      setLoading(false);
    }
  }

  async function runSearch(q: string) {
    setSearchParams(prev => {
      const next = new URLSearchParams(prev);
      next.set("q", q);
      next.set("page", "1");
      next.delete("serviceId");
      next.delete("service");
      next.delete("slug");
      return next;
    }, { replace: true });
    setSearching(true);
    setError("");
    try {
      await loadData(1, q.trim());
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to search services");
    } finally {
      setSearching(false);
    }
  }

  // Debounced search on type, matching the services page behavior.
  useEffect(() => {
    if (!query.trim()) {
      setSearching(false);
      return;
    }
    setLoading(false);
    setSearching(true);
    const id = setTimeout(() => runSearch(query), 400);
    return () => clearTimeout(id);
  }, [query]);

  async function handleSearch(e: FormEvent) {
    e.preventDefault();
    runSearch(query);
  }

  async function handleClear() {
    setQuery("");
    setSearchParams(prev => {
      const next = new URLSearchParams(prev);
      next.delete("q");
      next.delete("serviceId");
      next.delete("service");
      next.delete("slug");
      next.set("page", "1");
      return next;
    }, { replace: true });
    loadData(1, "");
  }

  // Load only teams the actor may choose as an owner. This query intentionally
  // exposes no bindings or roles, so builders do not need access.read.
  useEffect(() => {
    listArtifactOwningTeams()
      .then((page) => {
        setOwnerTeams(page.items);
      })
      .catch((cause: unknown) => setError(cause instanceof Error ? cause.message : "Could not load owning teams."));
  }, []);

  const loadAvailableBuckets = () =>
    listArtifactBuildSelectors(ownerTeamId, "BUCKET", "", 100, 0).then((bucketPage) => {
      setAvailableBuckets(bucketPage.items);
      setBucketId((current) =>
        bucketPage.items.some((bucket) => bucket.resource_id === current)
          ? current
          : bucketPage.items[0]?.resource_id || ""
      );
      return bucketPage;
    });

  useEffect(() => {
    setPage(1);
    Promise.all([
      loadData(1, query.trim()),
      loadAvailableBuckets(),
    ]).catch((cause: unknown) => setError(cause instanceof Error ? cause.message : "Could not load team access."));
  }, [ownerTeamId]);

  useEffect(() => {
    const refreshAfterCredentialTab = () => {
      // A separate tab preserves the in-progress build form. Refreshing on
      // focus makes newly created, authorized credential sets selectable.
      loadAvailableBuckets().catch((cause: unknown) =>
        setError(cause instanceof Error ? cause.message : "Could not refresh credential sets.")
      );
    };
    window.addEventListener("focus", refreshAfterCredentialTab);
    return () => window.removeEventListener("focus", refreshAfterCredentialTab);
  }, [ownerTeamId]);

  const createCredential = () => {
    if (!openAuthenticatedTab(CREATE_CREDENTIAL_PATH)) {
      toast.warning("Allow pop-ups to create a credential without losing this build.");
    }
  };

  // Re-fetch authorized services when page changes for both personal and team ownership.
  useEffect(() => {
    if (page > 1) {
      loadData(page, query.trim());
    }
  }, [page, ownerTeamId]);

  // Which services are already tracked in this account's workspace --
  // loaded once, best-effort. The Engine (not the Registry) owns this, so a
  // failure here just means pin-syncing after generate is skipped for this
  // session; it must never block SDK generation itself.
  useEffect(() => {
    if (!isAuth) return;
    api.workspace.getServices()
      .then(services => setWorkspaceServiceIds(new Set(services.map(s => s.service_id))))
      .catch(() => {})
      .finally(() => setWorkspaceServicesLoaded(true));
  }, [isAuth]);

  // After a successful generate, keep the workspace's pinned version in
  // sync with whatever was just generated -- otherwise a workspace could
  // silently drift from "what SDK code its consumers actually run" (the
  // exact gap the reconciled Service Versions plan's Phase 5 flagged).
  // Scoped to services already in the workspace: generating an SDK for a
  // service that was never added doesn't add it as a side effect -- that
  // stays an explicit action via the existing Add-to-Workspace button.
  // Best-effort and silent: a sync failure shouldn't turn a successful
  // generate into an error the user has to react to.
  const syncWorkspacePinsAfterGenerate = async (
    selectionPayload: { service_id: string; service_version_id?: string }[],
  ) => {
    const tracked = selectionPayload.filter(sel => workspaceServiceIds.has(sel.service_id));
    await Promise.all(tracked.map(async sel => {
      const svcData = data.find(d => d.service.id === sel.service_id);
      if (!svcData) return;
      const selectedVersion = sel.service_version_id
        ? svcData.serviceVersions.find((v) => v.id === sel.service_version_id)
        : undefined;
      // Generated SDK scopes are exact-version only; if a service_version_id
      // is absent, keep the existing workspace pin untouched.
      const versionTag = selectedVersion?.name || "";
      if (!sel.service_version_id || !versionTag) return;
      try {
        await api.workspace.addService(sel.service_id, svcData.service.name, versionTag, sel.service_version_id);
      } catch {
        // Silent: see function comment.
      }
    }));
  };

  const loadMoreResource = async (serviceId: string, resourceName: string) => {
    // Find the resourceId based on the first endpoint's resource_id in this service
    const serviceData = data.find(s => s.service.id === serviceId);
    if (!serviceData) return;
    const sample = serviceData.integrations.find(ep => ep.resource === resourceName);
    if (!sample || !sample.resource_id) return;
    
    const resourceId = sample.resource_id;
    if (loadingResource[resourceId] || !hasMoreResources[resourceId]) return;
    
    setLoadingResource(prev => ({ ...prev, [resourceId]: true }));
    try {
      const currentOffset = resourceOffsets[resourceId] || 0;
      const queryStr = `
        query($resourceId: String!, $serviceId: String, $limit: Int, $offset: Int) {
          resourceIntegrations(resourceId: $resourceId, serviceId: $serviceId, limit: $limit, offset: $offset) {
            id
            service_id
            name
            description
            version
            status
            method
            path
            deprecated
          }
        }
      `;
      const response = await api.graphql<{ resourceIntegrations: IntegrationObject[] }>(queryStr, { resourceId, serviceId: serviceId, limit: 50, offset: currentOffset });
      const enriched = response.resourceIntegrations.map(ep => ({ ...ep, resource: resourceName, resource_id: resourceId }));
      
      setData(prev => {
        return prev.map(s => {
          if (s.service.id === serviceId) {
            return {
              ...s,
              integrations: [...s.integrations, ...enriched]
            };
          }
          return s;
        });
      });
      
      setResourceOffsets(prev => ({ ...prev, [resourceId]: currentOffset + 50 }));
      setHasMoreResources(prev => ({ ...prev, [resourceId]: response.resourceIntegrations.length === 50 }));
    } catch (err) {
      toast.error("Failed to load more endpoints: " + (err instanceof Error ? err.message : String(err)));
    } finally {
      setLoadingResource(prev => ({ ...prev, [resourceId]: false }));
    }
  };

  const loadServiceIntegrations = async (serviceId: string) => {
    const serviceData = data.find(s => s.service.id === serviceId);
    if (!serviceData || loadedServices[serviceId] || loadingService[serviceId]) return;

    setLoadingService(prev => ({ ...prev, [serviceId]: true }));
    try {
      // Only fetch webhooks and service versions on expand — endpoints load lazily per resource
      const webhookRes = await api.graphql<{ service: { webhooks: WebhookObject[] }, serviceVersions: SdkServiceVersion[] }>(`
        query($id: String!) {
          service(id: $id) { webhooks { id name description method } }
          serviceVersions(serviceId: $id) { id name header_value status }
        }
      `, { id: serviceId });

      const webhooks = webhookRes?.service?.webhooks || [];
      const serviceVersions = webhookRes?.serviceVersions || [];
      setData(prev => prev.map(s =>
        s.service.id === serviceId ? { ...s, webhooks, serviceVersions } : s
      ));
      
      const serviceVersion = serviceVersions[0];
      if (serviceVersion) {
        setVersionSelections(prev => ({ ...prev, [serviceId]: serviceVersion.id }));
      }
      
      setLoadedServices(prev => ({ ...prev, [serviceId]: true }));
    } catch (err) {
      toast.error("Failed to load service data: " + (err instanceof Error ? err.message : String(err)));
    } finally {
      setLoadingService(prev => ({ ...prev, [serviceId]: false }));
    }
  };

  const loadResourceEndpoints = async (serviceId: string, resourceId: string, resourceName: string) => {
    if (loadingResourceByName[resourceName]) return;
    setLoadingResourceByName(prev => ({ ...prev, [resourceName]: true }));
    try {
      const queryStr = `
        query($resourceId: String!, $serviceId: String, $limit: Int, $offset: Int) {
          resourceIntegrations(resourceId: $resourceId, serviceId: $serviceId, limit: $limit, offset: $offset) {
            id service_id name description version status method path deprecated deprecation_date
          }
        }
      `;
      const response = await api.graphql<{ resourceIntegrations: IntegrationObject[] }>(
        queryStr, { resourceId, serviceId, limit: 50, offset: 0 }
      );
      const enriched = response.resourceIntegrations.map(ep => ({ ...ep, resource: resourceName, resource_id: resourceId }));
      setData(prev => prev.map(s => {
        if (s.service.id !== serviceId) return s;
        // Append, deduplicating by id
        const existing = new Set(s.integrations.map(e => e.id));
        return { ...s, integrations: [...s.integrations, ...enriched.filter(e => !existing.has(e.id))] };
      }));
      if (response.resourceIntegrations.length === 50) {
        setHasMoreResources(prev => ({ ...prev, [resourceId]: true }));
        setResourceOffsets(prev => ({ ...prev, [resourceId]: 50 }));
      }
    } catch (err) {
      toast.error("Failed to load endpoints for " + resourceName + ": " + (err instanceof Error ? err.message : String(err)));
    } finally {
      setLoadingResourceByName(prev => ({ ...prev, [resourceName]: false }));
    }
  };

  const toggleExpand = (serviceId: string) => {
    const willExpand = !expanded[serviceId];
    setExpanded(prev => ({ ...prev, [serviceId]: willExpand }));
    if (willExpand) {
      loadServiceIntegrations(serviceId);
    }
  };

  const toggleEndpoint = (serviceId: string, endpointId: string) => {
    if (lockedSelections[serviceId]?.has(endpointId)) {
      toast.warning("This endpoint is part of the existing SDK and cannot be removed during expansion.");
      return;
    }
    setSelections(prev => {
      const nextSet = new Set(prev[serviceId]);
      if (nextSet.has(endpointId)) {
        nextSet.delete(endpointId);
      } else {
        nextSet.add(endpointId);
      }
      return { ...prev, [serviceId]: nextSet };
    });
  };

  const toggleWebhook = (serviceId: string, webhookId: string) => {
    if (lockedWebhookSelections[serviceId]?.has(webhookId)) {
      toast.warning("This webhook is part of the existing SDK and cannot be removed during expansion.");
      return;
    }
    setWebhookSelections(prev => {
      const nextSet = new Set(prev[serviceId]);
      if (nextSet.has(webhookId)) {
        nextSet.delete(webhookId);
      } else {
        nextSet.add(webhookId);
      }
      return { ...prev, [serviceId]: nextSet };
    });
  };

  const toggleSelectAllEndpoints = (serviceId: string) => {
    const isSelectAll = selectAllServices.has(serviceId);
    if (isSelectAll) {
      setSelectAllServices(prev => { const s = new Set(prev); s.delete(serviceId); return s; });
      setSelections(prev => ({ ...prev, [serviceId]: new Set(lockedSelections[serviceId] || []) }));
    } else {
      setSelectAllServices(prev => new Set([...prev, serviceId]));
      setSelections(prev => ({ ...prev, [serviceId]: new Set() }));
    }
  };

  const toggleSelectAllWebhooks = (serviceId: string, webhooks: WebhookObject[]) => {
    if (webhooks.length === 0) return;
    const allSelected = (webhookSelections[serviceId]?.size || 0) === webhooks.length;
    if (allSelected) {
      setWebhookSelections(prev => ({ ...prev, [serviceId]: new Set(lockedWebhookSelections[serviceId] || []) }));
    } else {
      setWebhookSelections(prev => ({ ...prev, [serviceId]: new Set(webhooks.map((w) => w.id)) }));
    }
  };

  const totalSelected = data.reduce((acc, { service, integrations }) => {
    const endpointCount = selectAllServices.has(service.id)
      ? (service.endpoint_count || integrations.length || 0)
      : (selections[service.id]?.size || 0);
    return acc + endpointCount + (webhookSelections[service.id]?.size || 0);
  }, 0);
  const totalSelectedWebhooks = Object.values(webhookSelections).reduce((total, selected) => total + selected.size, 0);
  const totalSelectedServices = data.filter(({ service }) =>
    selectAllServices.has(service.id) ||
    (selections[service.id]?.size || 0) > 0 ||
    (webhookSelections[service.id]?.size || 0) > 0
  ).length;

  // Task 7 (engine_workspace_registration_plan.md): the Registry's direct
  // /sdks/generate is now workspace-gated server-side (Task 6), so a
  // service that isn't activated would just fail at generate time with no
  // warning beforehand. Surface that here instead: any currently-selected
  // service missing from workspaceServiceIds blocks Generate and gets an
  // inline "Add to Workspace" button, mirroring the same filter
  // handleGenerate itself uses to decide which services are "selected".
  const selectedServiceIdsForGate = data
    .filter(({ service }) =>
      selectAllServices.has(service.id) ||
      (selections[service.id]?.size || 0) > 0 ||
      (webhookSelections[service.id]?.size || 0) > 0
    )
    .map(({ service }) => service.id);
  const unactivatedSelectedServiceIds = isAuth
    ? selectedServiceIdsForGate.filter(id => !workspaceServiceIds.has(id))
    : [];

  const handleServiceAddedToWorkspace = (serviceId: string) => {
    setWorkspaceServiceIds(prev => new Set(prev).add(serviceId));
  };

  const checkDuplicateSDK = async () => {
    if (!sdkName.trim() || !artifactVersion.trim()) {
      setIsDuplicate(false);
      return;
    }
    setCheckingDuplicate(true);
    try {
      const queryStr = `
        query {
          sdkAnalytics(name: "${sdkName.trim()}") {
            history {
              version
            }
          }
        }
      `;
      const res = await api.graphql<{ sdkAnalytics?: { history: { version: string }[] } }>(queryStr);
      if (res.sdkAnalytics && res.sdkAnalytics.history) {
        const versions = res.sdkAnalytics.history.map(h => h.version);
        setIsDuplicate(versions.includes(artifactVersion.trim()));
      } else {
        setIsDuplicate(false);
      }
    } catch {
      setIsDuplicate(false);
    } finally {
      setCheckingDuplicate(false);
    }
  };

  const handleGenerate = async (e: FormEvent) => {
    e.preventDefault();
    const allServiceIds = new Set([
      ...Object.keys(selections),
      ...selectAllServices,
      ...Object.keys(webhookSelections),
    ]);
    const selectionPayload = Array.from(allServiceIds)
      .filter(serviceId =>
        selectAllServices.has(serviceId) ||
        (selections[serviceId]?.size || 0) > 0 ||
        (webhookSelections[serviceId]?.size || 0) > 0
      )
      .map(serviceId => {
        const svcData = data.find(d => d.service.id === serviceId);
        const selectedVersionID = versionSelections[serviceId] || svcData?.serviceVersions?.[0]?.id;
        return {
          service_id: serviceId,
          service_name: svcData?.service.name,
          service_slug: svcData?.service.slug,
          select_all: selectAllServices.has(serviceId),
          endpoint_ids: selectAllServices.has(serviceId) ? [] : Array.from(selections[serviceId] || new Set<string>()),
          webhook_ids: Array.from(webhookSelections[serviceId] || new Set<string>()),
          service_version_id: selectedVersionID,
        };
      });

    if (selectionPayload.length === 0) {
      toast.warning("Please select at least one endpoint or webhook to generate an SDK.");
      return;
    }
    if (selectionPayload.some(sel => !sel.service_version_id)) {
      toast.error("Each selected service needs a service version before generating an SDK.");
      return;
    }

    // Validate webhook configuration
    for (const sel of selectionPayload) {
      if (sel.webhook_ids.length > 0) {
        const svcData = data.find(d => d.service.id === sel.service_id);
        if (svcData && (!svcData.service.event_extraction_path || !svcData.service.incoming_webhook_config)) {
          toast.error(`Service '${svcData.service.name}' has webhooks selected but is missing proper webhook configuration. Please configure Webhook Setup in the service settings and ensure an event extraction path is set before generating an SDK.`);
          return;
        }
      }
    }

    // Validate API URL configuration
    for (const sel of selectionPayload) {
      if (sel.endpoint_ids.length > 0 || sel.select_all) {
        const svcData = data.find(d => d.service.id === sel.service_id);
        if (svcData && !svcData.service.base_url) {
          toast.error(`Service '${svcData.service.name}' is missing an API URL. Please configure the API Base URL in the service settings before generating an SDK.`);
          return;
        }
      }
    }

    if (!sdkName.trim()) {
      toast.warning(`${generationMode === "mcp" ? "MCP server" : "App"} name is required.`);
      return;
    }

    const selectedBucket = availableBuckets.find((bucket) => bucket.resource_id === bucketId);
    if (!selectedBucket) {
      toast.warning(ownerTeamId ? "Choose a credential set available to both you and the owning team." : "Choose a credential set you can use.");
      return;
    }
    const hasWebhookSelections = selectionPayload.some((selection) => selection.webhook_ids.length > 0);
    if (hasWebhookSelections && !webhookAttachment.trim()) {
      toast.warning("Enter the webhook bundle that supplies the selected events.");
      return;
    }

    if (generationMode === "sdk" && isDuplicate) {
      const confirmed = await toast.confirm(`An SDK with name "${sdkName.trim()}" and version "${artifactVersion.trim()}" already exists. Generating it again will overwrite the existing package file. Are you sure you want to continue?`);
      if (!confirmed) {
        return;
      }
    }

    setGenerating(true);
    setGenerateStatus("Starting generation...");
    setSdkDeployment(null);
    setMcpDeployment(null);
    setSdkTokenCopied(false);
    setMcpTokenCopied(false);
    try {
      const ownerTeamSlug = ownerTeams.find((team) => team.id === ownerTeamId)?.slug || "";
      const services = artifactServicesConfig(selectionPayload, data);
      const config: Record<string, unknown> = {
        apiVersion: "fused/v1",
        kind: generationMode,
        name: sdkName.trim(),
        version: artifactVersion.trim(),
        bucket: selectedBucket.display_name,
        services,
      };
      if (generationMode === "sdk") config.language = language;
      if (hasWebhookSelections) config.webhook_attachment = webhookAttachment.trim();

      if (generationMode === "mcp") {
        setGenerateStatus("Deploying MCP server...");
        const result = await planAndApplyArtifact<{ artifact_id: string; mcp_url: string; execution_token?: string }>("mcp", ownerTeamSlug, config);
        await syncWorkspacePinsAfterGenerate(selectionPayload);
        setMcpDeployment({ id: result.artifact_id, url: result.mcp_url, token: result.execution_token || "" });
        setGenerateStatus("MCP server deployed");
        return;
      }

      setGenerateStatus("Planning and generating SDK...");
      const res = await planAndApplyArtifact<{ artifact_id: string; job_id: string; execution_token?: string }>("sdk", ownerTeamSlug, config);

      const ctrl = new AbortController();
      type GenerationStreamEvent =
        | { type: "thinking"; message?: string }
        | { type: "complete"; integration_id: string }
        | { type: "error"; message?: string };
      const processGenerationStreamEvent = (
        event: GenerationStreamEvent,
        resolve: () => void,
        reject: (reason?: unknown) => void,
      ) => {
        if (event.type === "thinking") {
          setGenerateStatus(event.message || "Generating...");
          return;
        }

        if (event.type === "complete") {
          ctrl.abort();
          setGenerateStatus("Downloading...");
          api.sdks.download(event.integration_id, sdkName, artifactVersion).then(() => {
            setSdkDeployment({
              id: res.artifact_id,
              name: sdkName.trim(),
              version: artifactVersion.trim(),
              token: res.execution_token || "",
            });
            setGenerateStatus("App ready");
            resolve();
          }).catch(reject);
          return;
        }

        if (event.type === "error") {
          ctrl.abort();
          reject(new Error(event.message || "Unknown generation error"));
        }
      };

      await new Promise<void>((resolve, reject) => {
        fetchEventSource(`${BASE}/sdks/job/${res.job_id}/stream`, {
          headers: {
            "X-API-Key": getApiKey() ?? "",
          },
          signal: ctrl.signal,
          onmessage(ev) {
            try {
              const event = JSON.parse(ev.data);
              processGenerationStreamEvent(event, resolve, reject);
            } catch {
              console.error("Failed to parse SSE event", ev.data);
            }
          },
          onerror(err) {
            if (ctrl.signal.aborted) {
              throw err;
            }
            ctrl.abort();
            reject(new Error("Connection to server lost during generation"));
            throw err; // Prevent fetchEventSource from retrying
          },
          onclose() {
            // Prevent fetchEventSource from retrying when server gracefully closes
            throw new Error("Server closed connection gracefully");
          }
        });
      });

      // Reaching here means every step above resolved without throwing --
      // generation genuinely succeeded, so this is the one place to sync
      // workspace pins for it, regardless of which success branch got hit.
      await syncWorkspacePinsAfterGenerate(selectionPayload);

    } catch (err) {
      toast.error(`${generationMode === "mcp" ? "Failed to deploy MCP server" : "Failed to generate SDK"}: ${err instanceof Error ? err.message : "Unknown error"}`, 0);
    } finally {
      setGenerating(false);
      setGenerateStatus("");
    }
  };

  // No local filtering needed anymore since backend handles search and pagination
  const filteredData = data;

  return (
    <div className="max-w-6xl mx-auto py-8 px-4 h-full flex flex-col">
      <div className="flex items-center justify-between mb-8">
        <div>
          <h1 className="text-3xl font-bold text-slate-900 tracking-tight mb-1">{generationMode === "mcp" ? "Create MCP server" : "Create app"}</h1>
          <p className="text-slate-500">{generationMode === "mcp" ? "Choose the services and operations to make available through MCP." : "Choose the services and operations this app can use."}</p>
        </div>
      </div>

      {error && (
        <div className="mb-6 p-4 bg-red-50 border border-red-200 rounded-xl text-sm text-red-700 font-medium">
          {error}
        </div>
      )}

      {loading ? (
        <div className="flex-1 flex flex-col items-center justify-center min-h-[400px] text-slate-400">
          <Loader2 className="w-8 h-8 text-blue-500 animate-spin mb-4" />
          <p className="animate-pulse font-medium text-slate-500">Loading available integrations...</p>
        </div>
      ) : (
        <div className="flex flex-col lg:flex-row gap-8 flex-1 min-h-0 min-w-0">
          {/* Left Column: Selection */}
          <div className="flex-1 flex flex-col min-h-0 min-w-0">
            <form 
              onSubmit={handleSearch} 
              className="relative w-full mb-4"
              toolname="search_services_sdk"
              tooldescription="Search for services or endpoints to include in the SDK."
            >
              <button
                data-track="search_services"
                type="submit"
                disabled={searching}
                className="absolute left-3 top-1/2 -translate-y-1/2 text-slate-400 hover:text-slate-600 disabled:opacity-50 cursor-pointer"
                title="Search"
              >
                {searching ? (
                  <Loader2 className="w-5 h-5 animate-spin" />
                ) : (
                  <Search className="w-5 h-5" />
                )}
              </button>
              <input
                type="text"
                placeholder="Search for a service (e.g. Stripe, Shopify...)"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                className="w-full pl-10 pr-10 py-3 rounded-xl border border-slate-200 text-sm focus:outline-none focus:border-blue-500 focus:ring-2 focus:ring-blue-500/20 shadow-sm transition-all bg-white"
              />
              {query && (
                <button
                  data-track="clear_service_search"
                  type="button"
                  onClick={handleClear}
                  className="absolute right-3 top-1/2 -translate-y-1/2 text-slate-400 hover:text-slate-600 cursor-pointer"
                  title="Clear search"
                >
                  <X className="w-5 h-5" />
                </button>
              )}
            </form>

            <div className="flex-1 overflow-y-auto pr-2 pb-8 space-y-4">
              {filteredData.length === 0 ? (
                <div className="text-center py-12 text-slate-400 bg-white rounded-xl border border-dashed border-slate-200">
                  <p className="font-medium text-slate-600">
                    {query
                      ? "No services in your workspace match your search."
                      : workspaceServicesLoaded && workspaceServiceIds.size === 0
                        ? "No services added yet."
                        : ownerTeamId
                          ? "No shared services are available."
                          : "No services are available with your access."}
                  </p>
                  <p className="mt-1 text-sm">
                    {query
                      ? "Try another service name or activate one from the service catalog."
                      : workspaceServicesLoaded && workspaceServiceIds.size === 0
                        ? "Define and activate a service before creating an app or MCP server."
                        : ownerTeamId
                          ? "Ask an access administrator to give both you and the team access."
                          : "Ask a workspace administrator for access to the services and credential sets you need."}
                  </p>
                </div>
              ) : (
                filteredData.map(({ service, integrations, webhooks, serviceVersions }) => {
                  const isExpanded = expanded[service.id];
                  const isLoaded = loadedServices[service.id];
                  const selectedSet = selections[service.id];
                  const selectedWebhookSet = webhookSelections[service.id] || new Set();
                  const isSelectAll = selectAllServices.has(service.id);
                  const serviceEndpointCount = service.endpoint_count || integrations.length || 0;
                  const serviceWebhookCount = service.webhook_count || webhooks.length || 0;
                  const totalSelectedItems = (isSelectAll ? serviceEndpointCount : (selectedSet?.size || 0)) + selectedWebhookSet.size;
                  return (
                    <div key={service.id} className="min-w-0 overflow-hidden bg-white rounded-xl border border-slate-200 shadow-sm hover:shadow-md transition-shadow">
                      <div 
                        className={`flex items-center justify-between p-4 cursor-pointer hover:bg-slate-50 transition-colors ${isExpanded ? 'rounded-t-xl' : 'rounded-xl'}`}
                        onClick={() => toggleExpand(service.id)}
                      >
                        <div className="flex min-w-0 items-center gap-3">
                          <button className="shrink-0 text-slate-400 hover:text-slate-600 transition-colors">
                            {isExpanded ? <ChevronDown className="w-5 h-5" /> : <ChevronRight className="w-5 h-5" />}
                          </button>
                          <div className="min-w-0">
                            <h3 className="font-semibold text-slate-900 flex items-center gap-2">
                              <span className="truncate">{capitalizeFirstLetter(service.name)}</span>
                            </h3>
                            <p className="truncate text-xs text-slate-400">
                              {service.canonical_ref || (service.provider ? `@${service.provider.handle}/${service.slug}` : "")}
                              {(service.canonical_ref || service.provider) && " · "}
                              {isLoaded && totalSelectedItems > 0 ? `${totalSelectedItems} selected` : "Expand to view endpoints and webhooks"}
                            </p>
                          </div>
                        </div>
                      </div>
                      
                      {isExpanded && (
                        <div className="min-w-0 overflow-x-hidden border-t border-slate-100 bg-slate-50/50 max-h-[500px] overflow-y-auto">
                          {loadingService[service.id] ? (
                            <div className="flex items-center justify-center gap-2 py-6 text-slate-400">
                              <Loader2 className="w-4 h-4 animate-spin" />
                              <span className="text-sm">Loading...</span>
                            </div>
                          ) : (() => {
                            const sections = expandedSections[service.id] ?? { endpoints: true, webhooks: true };
                            const hasWebhooks = (service.webhook_count || webhooks.length) > 0;
                            const versions = serviceVersions || [];
                            const selectedVersionId = versionSelections[service.id] || versions[0]?.id || "";
                            const selectedVersion = versions.find((version) => version.id === selectedVersionId) || versions[0];
                            return (
                              <div>
                                {versions.length > 0 && (
                                  <div className="flex min-w-0 flex-col gap-3 border-b border-slate-100 bg-slate-50 px-4 py-4 sm:flex-row sm:items-center sm:justify-between sm:px-5">
                                    <div className="flex min-w-0 flex-col">
                                      <span className="text-sm font-semibold text-slate-800">Service Version</span>
                                      <span className="text-xs text-slate-500">Select the version this {generationMode === "mcp" ? "MCP server" : "app"} will use</span>
                                    </div>
                                    <select
                                      value={selectedVersionId}
                                      onChange={(e) => setVersionSelections(prev => ({ ...prev, [service.id]: e.target.value }))}
                                      title={selectedVersion?.header_value || selectedVersion?.name || ""}
                                      className="block w-full min-w-0 truncate rounded-lg border border-slate-300 bg-white px-3 py-1.5 text-sm shadow-sm outline-none focus:border-blue-500 focus:ring-2 focus:ring-blue-500 sm:w-[min(55%,22rem)]"
                                    >
                                      {versions.map((v) => (
                                        <option key={v.id} value={v.id}>{v.header_value || v.name}</option>
                                      ))}
                                    </select>
                                  </div>
                                )}
                                {/* Endpoints section */}
                                <div
                                  onClick={() => toggleSection(service.id, "endpoints")}
                                  className="w-full flex items-center justify-between px-3 py-2 hover:bg-slate-100 transition-colors cursor-pointer group"
                                >
                                  <div className="flex items-center gap-2">
                                    {sections.endpoints ? <ChevronDown className="w-3.5 h-3.5 text-slate-400" /> : <ChevronRight className="w-3.5 h-3.5 text-slate-400" />}
                                    <span className="text-xs font-bold uppercase tracking-wider text-slate-500">
                                      Endpoints {serviceEndpointCount > 0 && `(${serviceEndpointCount})`}
                                    </span>
                                  </div>
                                  {serviceEndpointCount > 0 && (
                                    <button
                                      type="button"
                                      onClick={(e) => {
                                        e.stopPropagation();
                                        toggleSelectAllEndpoints(service.id);
                                      }}
                                      className="text-[10px] font-medium px-2 py-1 rounded text-slate-500 hover:text-slate-700 hover:bg-slate-200 transition-colors flex items-center gap-1.5"
                                    >
                                      {selectAllServices.has(service.id) ? (
                                        <>
                                          <CheckSquare className="w-3.5 h-3.5 text-blue-600" />
                                          Deselect All
                                        </>
                                      ) : (
                                        <>
                                          {((selectedSet?.size || 0) > 0) ? <Square className="w-3.5 h-3.5 text-slate-400 fill-slate-200" /> : <Square className="w-3.5 h-3.5 text-slate-400" />}
                                          Select All {serviceEndpointCount > 0 && `(${serviceEndpointCount})`}
                                        </>
                                      )}
                                    </button>
                                  )}
                                </div>
                                {sections.endpoints && (
                                  <div className="px-2 pb-2">
                                    <EndpointSelectionList
                                      endpoints={integrations}
                                      selectedIds={selectedSet || new Set()}
                                      isSelectAll={isSelectAll}
                                      onToggle={(id) => toggleEndpoint(service.id, id)}
                                      getId={(ep) => ep.id}
                                      maxHeightClass=""
                                      hasMoreResources={hasMoreResources}
                                      onLoadMoreResource={(resourceName) => loadMoreResource(service.id, resourceName)}
                                      resourceMetadata={service.resources || []}
                                      loadingResources={loadingResourceByName}
                                      onResourceExpand={(resourceId, resourceName) => loadResourceEndpoints(service.id, resourceId, resourceName)}
                                    />
                                  </div>
                                )}

                                {/* SDK and MCP now share the same Engine plan
                                    contract, including webhook bundle selections. */}
                                  <>
                                    <div
                                      onClick={() => hasWebhooks && toggleSection(service.id, "webhooks")}
                                      className={`w-full flex items-center justify-between px-3 py-2 transition-colors border-t border-slate-100 group ${hasWebhooks ? "hover:bg-slate-100 cursor-pointer" : "cursor-default opacity-40"}`}
                                    >
                                      <div className="flex items-center gap-2">
                                        {sections.webhooks ? <ChevronDown className="w-3.5 h-3.5 text-slate-400" /> : <ChevronRight className="w-3.5 h-3.5 text-slate-400" />}
                                        <span className="text-xs font-bold uppercase tracking-wider text-slate-500">
                                          Webhooks {serviceWebhookCount > 0 && `(${serviceWebhookCount})`}
                                        </span>
                                        {!hasWebhooks && (
                                          <span className="ml-2 text-[10px] text-slate-400 font-normal normal-case tracking-normal">Not configured</span>
                                        )}
                                      </div>
                                      {hasWebhooks && isLoaded && (
                                        <button
                                          type="button"
                                          onClick={(e) => {
                                            e.stopPropagation();
                                            toggleSelectAllWebhooks(service.id, webhooks);
                                          }}
                                          className="text-[10px] font-medium px-2 py-1 rounded text-slate-500 hover:text-slate-700 hover:bg-slate-200 transition-colors flex items-center gap-1.5"
                                        >
                                          {((webhookSelections[service.id]?.size || 0) === webhooks.length) ? (
                                            <>
                                              <CheckSquare className="w-3.5 h-3.5 text-blue-600" />
                                              Deselect All
                                            </>
                                          ) : (
                                            <>
                                              {((webhookSelections[service.id]?.size || 0) > 0) ? <Square className="w-3.5 h-3.5 text-slate-400 fill-slate-200" /> : <Square className="w-3.5 h-3.5 text-slate-400" />}
                                              Select All {serviceWebhookCount > 0 && `(${serviceWebhookCount})`}
                                            </>
                                          )}
                                        </button>
                                      )}
                                    </div>
                                    {sections.webhooks && hasWebhooks && (
                                      <div className="px-2 pb-2">
                                        <WebhookSelectionList
                                          webhooks={webhooks}
                                          selectedIds={selectedWebhookSet}
                                          onToggle={(id) => toggleWebhook(service.id, id)}
                                          getId={(wh) => wh.id}
                                          maxHeightClass=""
                                        />
                                      </div>
                                    )}
                                  </>
                              </div>
                            );
                          })()}
                        </div>
                      )}
                    </div>
                  );
                })
              )}

              {!loading && !query && totalPages > 1 && (
                <div className="flex items-center justify-between gap-3 border border-slate-200 px-4 py-3 bg-white rounded-xl shadow-sm mt-4">
                  <p className="text-xs text-slate-500">
                    {totalItems === 0 ? 0 : (page - 1) * 20 + 1}-{Math.min(totalItems, page * 20)} of {totalItems}
                  </p>
                  <div className="flex items-center gap-1">
                    <button
                      type="button"
                      data-track="paginate_previous"
                      onClick={() => setPage(p => Math.max(1, p - 1))}
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
                      onChange={(e) => setPage(parseInt(e.target.value, 10))}
                    >
                      {Array.from({ length: totalPages }, (_, i) => i + 1).map(p => (
                        <option key={p} value={p}>{p}</option>
                      ))}
                    </select>
                    <span className="text-xs font-medium text-slate-500 pr-2">of {totalPages}</span>
                    <button
                      type="button"
                      data-track="paginate_next"
                      onClick={() => setPage(p => p + 1)}
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
          </div>

          {/* Right Column: Generation Form */}
          <ConsumerGenerationPanel
            generationMode={generationMode}
            ownerTeams={ownerTeams}
            ownerTeamId={ownerTeamId}
            setOwnerTeamId={setOwnerTeamId}
            availableBuckets={availableBuckets}
            bucketId={bucketId}
            setBucketId={setBucketId}
            onCreateCredential={createCredential}
            sdkName={sdkName}
            setSdkName={setSdkName}
            setIsDuplicate={setIsDuplicate}
            checkDuplicateSDK={checkDuplicateSDK}
            totalSelectedWebhooks={totalSelectedWebhooks}
            webhookAttachment={webhookAttachment}
            setWebhookAttachment={setWebhookAttachment}
            artifactVersion={artifactVersion}
            setArtifactVersion={setArtifactVersion}
            checkingDuplicate={checkingDuplicate}
            isDuplicate={isDuplicate}
            language={language}
            setLanguage={setLanguage}
            totalSelectedServices={totalSelectedServices}
            totalSelected={totalSelected}
            unactivatedSelectedServiceIds={unactivatedSelectedServiceIds}
            data={data}
            versionSelections={versionSelections}
            handleServiceAddedToWorkspace={handleServiceAddedToWorkspace}
            handleGenerate={handleGenerate}
            generating={generating}
            generateStatus={generateStatus}
            sdkDeployment={sdkDeployment}
            sdkTokenCopied={sdkTokenCopied}
            setSdkTokenCopied={setSdkTokenCopied}
            mcpDeployment={mcpDeployment}
            mcpTokenCopied={mcpTokenCopied}
            setMcpTokenCopied={setMcpTokenCopied}
            AddSelectedServiceToWorkspaceButton={AddSelectedServiceToWorkspaceButton}
          />
        </div>
      )}

    </div>
  );
}
