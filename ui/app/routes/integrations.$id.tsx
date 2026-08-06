import { useState, useEffect, useRef } from "react";
import {
  useParams,
  Link,
  useSearchParams,
  useLoaderData,
  useRouteLoaderData,
  useLocation,
  type MetaFunction,
} from "@remix-run/react";

export const meta: MetaFunction<typeof clientLoader> = ({
  data,
  matches,
  params,
}) => {
  const service = data?.initialServiceData?.service;
  const title = service?.name
    ? `${
        service.name.charAt(0).toUpperCase() + service.name.slice(1)
      } - Fused`
    : "Service details - Fused";
  const description =
    service?.description ||
    `Explore the ${
      service?.name
        ? service.name.charAt(0).toUpperCase() + service.name.slice(1)
        : ""
    } API integration on Fused — generate typed SDKs, webhook receivers, and MCP servers.`;
  // params.provider is only present when this meta function is reused by
  // integrations.$provider.$id.tsx (the cross-account public-browsing route).
  const providerSegment = params.provider ? `${params.provider}/` : "";
  const url = `https://usefused.com/integrations/${providerSegment}${
    service?.slug ?? service?.id ?? ""
  }`;

  const parentMeta = matches
    .filter((m) => m.id === "root")
    .flatMap((m) => m.meta ?? []);
  const inheritedMeta = parentMeta.filter((m) => {
    const name = "name" in m ? m.name : undefined;
    const property = "property" in m ? m.property : undefined;
    return (
      !("title" in m) &&
      name !== "description" &&
      !String(property ?? "").startsWith("og:") &&
      !String(name ?? "").startsWith("twitter:")
    );
  });

  return [
    ...inheritedMeta,
    { title },
    { name: "description", content: description },
    { property: "og:type", content: "website" },
    { property: "og:site_name", content: "Fused" },
    { property: "og:title", content: title },
    { property: "og:description", content: description },
    { property: "og:url", content: url },
    { name: "twitter:card", content: "summary" },
    { name: "twitter:title", content: title },
    { name: "twitter:description", content: description },
  ];
};
import {
  api,
  type ServiceGenerationResult,
  type DriftSnapshot,
  type IntegrationObject,
  type Service,
  type ServiceVersion,
  type SpecificationImportPlan,
  type WebhookEventEntry,
  type WebhookAnalyticsSummary,
  type EngineExecutionEventEntry,
  type EngineExecutionAnalyticsSummary,
  type ServiceConsumerEntry,
} from "~/lib/api";
import EndpointsTab from "~/components/EndpointsTab";
import WebhooksTab from "~/components/WebhooksTab";
import ActivityTab from "~/components/AnalyticsTab";
import EndpointDetailsSidebar from "~/components/EndpointDetailsSidebar";
import { ServerDisplay } from "~/components/integration-details/ServerDisplay";
import { VersionSelector } from "~/components/integration-details/VersionSelector";
import { WorkspaceConnectionProfileSection } from "~/components/integration-details/WorkspaceConnectionProfileSection";
import {
  Check,
  Copy,
  AlertTriangle,
  Loader2,
  MessageSquare,
  Share2,
  Briefcase,
  ChevronDown,
  Globe2,
  Lock,
} from "lucide-react";
import {
  ImportWarningPanel,
  ServiceHeaderWarningIcon,
} from "~/components/ImportWarnings";
import { NotificationBanner } from "~/components/notifications/NotificationBanner";
import { useWorkspaceNotifications } from "~/components/notifications/useWorkspaceNotifications";
import { isPending, matchesService } from "~/components/notifications/notificationHelpers";
import { useToast } from "~/components/Toast";
import { formatServiceName } from "~/lib/format";
import { useEndpointSearch } from "~/hooks/useEndpointSearch";
import { useImportSession } from "~/hooks/useImportSession";
import { useResourceLoader } from "~/hooks/useResourceLoader";

import { redirect } from "@remix-run/react";
import { APIRequestError } from "~/lib/authorization-error";

function requireRemoteSource(sourceURL?: string): string {
  const value = sourceURL || "";
  if (!value.startsWith("http://") && !value.startsWith("https://")) {
    throw new Error(
      "This source cannot be refreshed from a URL. Import the corrected specification instead."
    );
  }
  return value;
}

function resolveProviderVersion(
  selected: string | null,
  versions: ServiceVersion[]
): string {
  const target =
    selected ||
    versions.find((item) => item.status === "public")?.name ||
    versions[0]?.name;
  if (!target) throw new Error("The provider version could not be resolved.");
  return target;
}

function importPlanConfirmation(plan: SpecificationImportPlan): string {
  return `${plan.action} ${plan.target_version}: ${plan.diff.added} added, ${plan.diff.changed} changed, ${plan.diff.removed} removed. Apply this import?`;
}

function isUUID(value?: string): boolean {
  return (
    !!value &&
    /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(
      value
    )
  );
}

function canManageService(
  isAuth: boolean,
  isOwner: boolean | undefined,
  routeId?: string,
  provider?: string
): boolean {
  if (!isAuth) return false;
  if (isOwner !== false) return true;
  // A provider-less slug lookup is scoped to the caller's own account in the
  // Registry resolver, so it is owner-capable even if a stale GraphQL payload
  // reports the historical is_owner column as false.
  return !provider && !!routeId && !isUUID(routeId);
}

export const clientLoader = async ({
  params,
  request,
}: {
  params: { id?: string; provider?: string };
  request: Request;
}) => {
  const id = params.id;
  // provider is only ever present when this loader is reused by
  // integrations.$provider.$id.tsx (the cross-account public-browsing
  // route) -- this file's own single-segment route never has it, so every
  // branch below behaves exactly as before whenever provider is undefined.
  const provider = params.provider;
  if (!id)
    return { resolvedId: null, initialServiceData: null, error: "No ID provided" };

	const session = await api.auth.session().catch(() => ({ authenticated: false }));
	const isAuthenticated = session.authenticated;
  const url = new URL(request.url);
  const version = url.searchParams.get("version");

  const queryStr = provider
    ? `
    query($id: String!, $version: String, $provider: String) {
      service(id: $id, version: $version, provider: $provider) {
        id name slug description base_url current_service_version servers { url description environment is_default }
        is_public watch_for_drift created_at updated_at
        source_url import_method
        import_warnings { id endpoint_id method path operation_id reasons recommendation source created_at }
        auth_configs { type flow scheme location key_name token_url authorization_url open_id_connect_url scopes }
        event_extraction_path
        incoming_webhook_config { auth_type auth_location auth_key_name signature_header }
        rate_limit { strategy requests_per_second requests_per_minute }
        retry_config { strategy max_retries backoff_ms }
        default_headers
        provider { name handle }
        canonical_ref
        is_owner
        webhook_count
        endpoint_count
        webhooks { id name description method request_body }
        resources { id name endpointCount }
      }
      serviceVersions(serviceId: $id, provider: $provider) { id name is_public status created_at updated_at }
    }
  `
    : `
    query($id: String!, $version: String) {
      service(id: $id, version: $version) {
        id name slug description base_url current_service_version servers { url description environment is_default }
        is_public watch_for_drift created_at updated_at
        source_url import_method
        import_warnings { id endpoint_id method path operation_id reasons recommendation source created_at }
        auth_configs { type flow scheme location key_name token_url authorization_url open_id_connect_url scopes }
        event_extraction_path
        incoming_webhook_config { auth_type auth_location auth_key_name signature_header }
        rate_limit { strategy requests_per_second requests_per_minute }
        retry_config { strategy max_retries backoff_ms }
        default_headers
        provider { name handle }
        canonical_ref
        is_owner
        webhook_count
        endpoint_count
        webhooks { id name description method request_body }
        resources { id name endpointCount }
      }
      serviceVersions(serviceId: $id) { id name is_public status created_at updated_at }
    }
  `;

  let initialServiceData = null;
  let resolvedId = id;
  let error = null;
  try {
    const variables: Record<string, string> = {
      id: id as string,
      version: version || "",
    };
    if (provider) variables.provider = provider;
		const res = await api.graphql<{
      service: Service | null;
      serviceVersions?: ServiceVersion[];
		}>(queryStr, variables);
    initialServiceData = res;

		if (!initialServiceData?.service && !isAuthenticated) {
      return redirect(
        `/login?next=${encodeURIComponent(url.pathname + url.search)}`
      );
    }

    // The resolver returns the canonical UUID; use it so client always has the UUID
    if (initialServiceData?.service?.id) {
      resolvedId = initialServiceData.service.id;
    }

    // Redirect UUID-based URLs to the canonical slug URL (preserving the
    // provider segment, if this is the cross-account route).
    if (
      isUUID(id) &&
      initialServiceData?.service?.slug &&
      initialServiceData.service.slug !== id
    ) {
      const canonicalPath = provider
        ? `/integrations/${provider}/${initialServiceData.service.slug}`
        : `/integrations/${initialServiceData.service.slug}`;
      return redirect(`${canonicalPath}${url.search}`);
    }
  } catch (err) {
    const msg = err instanceof Error ? err.message : "";
    if (isAuthenticated && err instanceof APIRequestError && err.status === 401) {
      // Session status clears invalid cookies on the login page. A permission
      // denial is intentionally not a logout because the session remains valid.
      throw redirect(`/login?next=${encodeURIComponent(url.pathname + url.search)}`);
    }
    error = msg;
  }

  return {
    resolvedId,
    initialServiceData,
    error,
  };
};

function Badge({ label, color }: { label: string; color: string }) {
  return (
    <span
      className={`inline-flex items-center px-2 py-0.5 rounded text-xs font-medium ${color}`}
    >
      {label}
    </span>
  );
}

// AddToWorkspaceButton — S2.
// Handles the "Add to Workspace" CTA for authenticated non-owners.
// Optimistic UI: transitions to "Added ✓" immediately on success so the user
// gets instant feedback without a loading spinner for what is a fast DB write.
// State is component-local because: (a) the workspace service list is not
// rendered on this page, so no global state needs updating; (b) a page refresh
// naturally re-derives the true state from the Engine if needed.
function AddToWorkspaceButton({
  serviceId,
  serviceName,
  versionTag,
  onAdded,
}: {
  serviceId: string;
  serviceName: string;
  versionTag?: string | null;
  onAdded?: () => void;
}) {
  const [added, setAdded] = useState(false);
  const [adding, setAdding] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const canPinVersion = Boolean(versionTag);

  const handleAdd = async () => {
    if (added || adding || !versionTag) return;
    setAdding(true);
    setError(null);
    try {
      await api.workspace.addService(serviceId, serviceName, versionTag);
      setAdded(true);
      onAdded?.();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to add service");
    } finally {
      setAdding(false);
    }
  };

  if (added) {
    return (
      <span
        className="inline-flex items-center gap-1.5 px-3 py-1.5 text-sm font-medium bg-green-600 text-white rounded-md shadow-sm"
        data-track="add_to_workspace_success"
      >
        <Check className="w-3.5 h-3.5" />
        Added ✓
      </span>
    );
  }

  return (
    <button
      id="add-to-workspace-btn"
      onClick={handleAdd}
      disabled={adding || !canPinVersion}
      className="inline-flex items-center gap-1.5 px-3 py-1.5 text-sm font-medium border border-slate-300 text-slate-700 hover:bg-slate-50 hover:border-slate-400 rounded-md transition-colors shadow-sm disabled:opacity-50 disabled:cursor-not-allowed"
      data-track="add_to_workspace"
      title={`Add ${serviceName} to your workspace`}
    >
      {adding ? "Adding…" : "Add to workspace"}
      {error && <span className="ml-2 text-red-500 text-xs">{error}</span>}
    </button>
  );
}

export default function IntegrationDetail() {
  const toast = useToast();
  const loaderData = useLoaderData<typeof clientLoader>();
  const { id: paramId, provider } = useParams<{
    id: string;
    provider?: string;
  }>();
  const location = useLocation();
  const id = loaderData?.resolvedId || paramId;
  const rootData = useRouteLoaderData<{ isAuth: boolean }>("root");
  const isAuth = rootData?.isAuth ?? false;

  const [res, setRes] = useState<ServiceGenerationResult | null>(() => {
    if (loaderData?.initialServiceData?.service) {
      return {
        service: loaderData.initialServiceData.service,
        integrations: [],
      };
    }
    return null;
  });

  useEffect(() => {
    const initialServiceData = loaderData?.initialServiceData;
    const service = initialServiceData?.service;
    if (!service) return;
    setRes((prev) => {
      if (!prev)
        return {
          service,
          integrations: [],
        };

      const newService = { ...prev.service, ...service };

      return {
        ...prev,
        service: newService,
      };
    });
  }, [loaderData?.initialServiceData]);

  // Only the loaded service row gives us the Engine/Registry UUID. The route
  // param may be a slug, so UUID-only admin panels must wait for `res`.
  const serviceId = res?.service?.id;

  const [workspaceServiceActive, setWorkspaceServiceActive] = useState<
    boolean | null
  >(null);

  useEffect(() => {
    if (!isAuth || !serviceId) {
      setWorkspaceServiceActive(false);
      return;
    }
    let cancelled = false;
    setWorkspaceServiceActive(null);
    api.workspace
      .getServices()
      .then((services) => {
        if (!cancelled) {
          setWorkspaceServiceActive(
            services.some((service) => service.service_id === serviceId)
          );
        }
      })
      .catch(() => {
        if (!cancelled) setWorkspaceServiceActive(false);
      });
    return () => {
      cancelled = true;
    };
  }, [isAuth, serviceId]);

  const [drift, setDrift] = useState<DriftSnapshot[]>([]);
  const [loading, setLoading] = useState(
    !loaderData?.initialServiceData?.service
  );
  const [error, setError] = useState(loaderData?.error || "");
  const [driftAction, setDriftAction] = useState<string | null>(null);
  const [showShareMenu, setShowShareMenu] = useState(false);
  const shareMenuRef = useRef<HTMLDivElement>(null);
  const [showVisibilityMenu, setShowVisibilityMenu] = useState(false);
  const visibilityMenuRef = useRef<HTMLDivElement>(null);

  // Close header menus when clicking outside.
  useEffect(() => {
    function handleClickOutside(event: MouseEvent) {
      if (
        shareMenuRef.current &&
        !shareMenuRef.current.contains(event.target as Node)
      ) {
        setShowShareMenu(false);
      }
      if (
        visibilityMenuRef.current &&
        !visibilityMenuRef.current.contains(event.target as Node)
      ) {
        setShowVisibilityMenu(false);
      }
    }
    document.addEventListener("mousedown", handleClickOutside);
    return () => document.removeEventListener("mousedown", handleClickOutside);
  }, []);

  const [savingDrift, setSavingDrift] = useState(false);

  const [selectedEndpoint, setSelectedEndpoint] =
    useState<IntegrationObject | null>(null);

  const [searchParams, setSearchParams] = useSearchParams();
  // The selected provider version is URL-addressable so client-side refreshes
  // preserve the same immutable contract view as the initial loader.
  const version = searchParams.get("version") || "";

  const {
    resourceVersions,
    setResourceVersions,
    expandedResources,
    integrationsByResource,
    setIntegrationsByResource,
    loadingResources,
    hasMoreResources,
    toggleResource,
    loadMoreEndpoints,
  } = useResourceLoader(serviceId, version);

  const urlTab = searchParams.get("tab") as
    | "endpoints"
    | "webhooks"
    | "analytics";
  const activeTab = ["endpoints", "webhooks", "analytics"].includes(urlTab)
    ? urlTab
    : "endpoints";
  const urlWPage = parseInt(searchParams.get("wPage") || "1", 10);
  const urlWStartDate = searchParams.get("wStartDate") || "";
  const urlWEndDate = searchParams.get("wEndDate") || "";
  const importSessionId = searchParams.get("importSession");

  const [isClientFetchingTab, setIsClientFetchingTab] = useState(false);

  const handleTabChange = (tab: "endpoints" | "webhooks" | "analytics") => {
    setSearchParams(
      (prev) => {
        prev.set("tab", tab);
        return prev;
      },
      { replace: true, preventScrollReset: true }
    );
  };

  useEffect(() => {
    if (workspaceServiceActive === false && activeTab === "analytics") {
      handleTabChange("endpoints");
    }
  }, [workspaceServiceActive, activeTab]);

  const syncWebhookParams = (params: {
    page?: number;
    startDate?: string;
    endDate?: string;
  }) => {
    setSearchParams(
      (prev) => {
        if (params.page !== undefined) {
          if (params.page <= 1) prev.delete("wPage");
          else prev.set("wPage", String(params.page));
        }
        if (params.startDate !== undefined) {
          if (!params.startDate) prev.delete("wStartDate");
          else prev.set("wStartDate", params.startDate);
        }
        if (params.endDate !== undefined) {
          if (!params.endDate) prev.delete("wEndDate");
          else prev.set("wEndDate", params.endDate);
        }
        return prev;
      },
      { replace: true, preventScrollReset: true }
    );
  };

  const [webhookEvents, setWebhookEvents] = useState<WebhookEventEntry[]>([]);
  const [webhookPage, setWebhookPage] = useState(
    isNaN(urlWPage) || urlWPage < 1 ? 1 : urlWPage
  );
  const [webhookTotal, setWebhookTotal] = useState(0);
  const webhookLimit = 10;
  const [webhookFilterEvent, setWebhookFilterEvent] = useState<string>("");
  const [webhookStartDate, setWebhookStartDate] =
    useState<string>(urlWStartDate);
  const [webhookEndDate, setWebhookEndDate] = useState<string>(urlWEndDate);
  const [webhookAnalytics, setWebhookAnalytics] = useState<WebhookAnalyticsSummary | null>(null);
  const [executionEvents, setExecutionEvents] = useState<EngineExecutionEventEntry[]>([]);
  const [executionTotal, setExecutionTotal] = useState(0);
  const [executionPage, setExecutionPage] = useState(1);
  const executionLimit = 10;
  const [executionTransport, setExecutionTransport] = useState("");
  const [executionStatus, setExecutionStatus] = useState("");
  const [executionAnalytics, setExecutionAnalytics] = useState<EngineExecutionAnalyticsSummary | null>(null);
  const [dependentSDKs, setDependentSDKs] = useState<ServiceConsumerEntry[]>([]);
  const [dependentMCPs, setDependentMCPs] = useState<ServiceConsumerEntry[]>([]);
  const {
    searchQuery,
    setSearchQuery,
    isSearching,
    searchResults,
    hasMoreSearch,
    handleSearch,
    loadMoreSearchResults,
    handleClearSearch,
  } = useEndpointSearch(serviceId, res, integrationsByResource);

  // Contextual notification banner: filtered to just this service, via the
  // same matchesService rule the SDK/MCP-page banner mirrors with
  // matchesConfig. Complements (doesn't replace) the general bell in the
  // shared layout -- see plans/plan-service-changelog.md's Phase 4.
  const {
    unresolved: allNotifications,
    serviceRefs: notificationServiceRefs,
    markRead: markNotificationRead,
    dismiss: dismissNotification,
  } = useWorkspaceNotifications(isAuth);
  // Detail banners are action-oriented. Acknowledged items remain available
  // in the bell and full notifications page, but should not interrupt a
  // service view after the user has already marked them read.
  const serviceNotifications = serviceId
    ? allNotifications.filter((item) => isPending(item) && matchesService(item, serviceId))
    : [];

  useEffect(() => {
    if (selectedEndpoint?.status === "drifted") {
      const queryStr = `
        query($serviceId: String!) {
          driftSnapshots(serviceId: $serviceId) {
            id
            integration_object_id
            created_at
            status
            diff {
              field
              old_value
              new_value
            }
          }
        }
      `;
      // We pass the service ID since the backend driftSnapshots takes serviceId
      // and returns all pending drift snapshots for that service.
      if (id) {
        api
          .graphql<{ driftSnapshots: DriftSnapshot[] }>(queryStr, { serviceId })
          .then((res) => setDrift(res.driftSnapshots))
          .catch(console.error);
      }
    } else {
      setDrift([]);
    }
  }, [selectedEndpoint, id]);

  useEffect(() => {
    // Fetch full endpoint details when sidebar opens, only if not already loaded.
    if (
      selectedEndpoint &&
      !selectedEndpoint.isWebhook &&
      !selectedEndpoint._detailsLoaded
    ) {
      api
        .graphql<{ integration: IntegrationObject }>(
          `
        query($id: String!, $serviceId: String!) {
          integration(id: $id, serviceId: $serviceId) {
            id
            description
            parameters {
              name
              in
              required
              type
              description
            }
            request_body
            responses
            graphql_query
          }
        }
      `,
          { id: selectedEndpoint.id, serviceId: selectedEndpoint.service_id }
        )
        .then((data) => {
          // Merge fetched detail fields onto the existing list item object.
          if (data.integration) {
            setSelectedEndpoint((prev) =>
              prev && prev.id === data.integration.id
                ? { ...prev, ...data.integration, _detailsLoaded: true }
                : prev
            );
          } else {
            // Integration not found — mark as loaded to prevent infinite retries.
            setSelectedEndpoint((prev) =>
              prev && prev.id === selectedEndpoint.id
                ? { ...prev, _detailsLoaded: true }
                : prev
            );
          }
        })
        .catch((err) => {
          // On error, mark loaded anyway so the sidebar still renders whatever it has.
          console.error(err);
          setSelectedEndpoint((prev) =>
            prev && prev.id === selectedEndpoint.id
              ? { ...prev, _detailsLoaded: true }
              : prev
          );
        });
    }
  }, [selectedEndpoint]);

  async function loadWebhookData() {
    if (
      !id ||
      !/^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(
        id
      )
    )
      return;
    if (!isAuth || workspaceServiceActive !== true) return;
    if (!serviceId) return;
    setIsClientFetchingTab(true);
    try {
      // Webhook analytics/events are Engine-owned data and are fetched
      // directly from the Engine (BACKEND_URL) via api.workspace.* -- this
      // deliberately does NOT go through api.graphql (Registry). See
      // internal/engine/api/webhook_analytics_handlers.go.
      const offset = (webhookPage - 1) * webhookLimit;
      const params: Parameters<typeof api.workspace.listWebhookEvents>[0] = {
        serviceId,
        limit: webhookLimit,
        offset,
      };
      if (webhookFilterEvent) {
        params.eventName = webhookFilterEvent;
      }
      if (webhookStartDate) {
        params.startDate = new Date(webhookStartDate).toISOString();
      }
      if (webhookEndDate) {
        const end = new Date(webhookEndDate);
        end.setHours(23, 59, 59, 999);
        params.endDate = end.toISOString();
      }
      const [events, analytics] = await Promise.all([
        api.workspace.listWebhookEvents(params),
        api.workspace.getWebhookAnalytics(params),
      ]);
      setWebhookEvents(events.items || []);
      setWebhookTotal(events.total || 0);
      setWebhookAnalytics(analytics);
    } catch (e) {
      console.error(e);
    } finally {
      setIsClientFetchingTab(false);
    }
  }

  async function loadExecutionData() {
    if (!isAuth || workspaceServiceActive !== true || !serviceId) return;
    setIsClientFetchingTab(true);
    try {
      const params: Parameters<typeof api.workspace.listEngineExecutionEvents>[0] = {
        serviceId,
        limit: executionLimit,
        offset: (executionPage - 1) * executionLimit,
      };
      if (executionTransport) params.transport = executionTransport;
      if (executionStatus) params.status = executionStatus;
      const [events, analytics] = await Promise.all([
        api.workspace.listEngineExecutionEvents(params),
        api.workspace.getEngineExecutionAnalytics(params),
      ]);
      setExecutionEvents(events.items || []);
      setExecutionTotal(events.total || 0);
      setExecutionAnalytics(analytics);
    } catch (e) {
      console.error(e);
    } finally {
      setIsClientFetchingTab(false);
    }
  }

  useEffect(() => {
    if (activeTab === "webhooks" || activeTab === "analytics") {
      loadWebhookData();
    }
  }, [
    id,
    webhookFilterEvent,
    webhookStartDate,
    webhookEndDate,
    webhookPage,
    activeTab,
    workspaceServiceActive,
  ]);

  useEffect(() => {
    if (activeTab === "analytics") loadExecutionData();
  }, [
    activeTab,
    serviceId,
    executionPage,
    executionTransport,
    executionStatus,
    workspaceServiceActive,
  ]);

  async function loadAnalyticsData() {
    if (!id) return;
    if (!isAuth || workspaceServiceActive !== true || !serviceId) return;
    setIsClientFetchingTab(true);
    try {
      const consumers = await api.workspace.listServiceConsumers(serviceId);
      setDependentSDKs(consumers.filter((consumer) => consumer.kind === "sdk"));
      setDependentMCPs(consumers.filter((consumer) => consumer.kind === "mcp"));
    } catch (e) {
      console.error(e);
    } finally {
      setIsClientFetchingTab(false);
    }
  }

  useEffect(() => {
    if (activeTab === "analytics") {
      loadAnalyticsData();
    }
  }, [id, activeTab, workspaceServiceActive]);

  async function loadData() {
    if (!id) return;

    setLoading(true);
    autoExpandedServiceId.current = null;
    // provider is only set when this component is rendered under
    // integrations.$provider.$id.tsx (cross-account public-browsing route)
    const queryStr = provider
      ? `
      query($id: String!, $version: String, $provider: String) {
        service(id: $id, version: $version, provider: $provider) {
          id
          name
          slug
          description
          base_url
          current_service_version
          servers {
            url
            description
            environment
            is_default
          }
          is_public
          watch_for_drift
          created_at
          updated_at
          source_url
          import_method
          import_warnings {
            id
            endpoint_id
            method
            path
            operation_id
            reasons
            recommendation
            source
            created_at
          }
          auth_configs {
            type
            flow
            scheme
            location
            key_name
            token_url
            authorization_url
            open_id_connect_url
            scopes
          }
          event_extraction_path
          incoming_webhook_config {
            auth_type
            auth_location
            auth_key_name
            signature_header
          }
          rate_limit {
            strategy
            requests_per_second
            requests_per_minute
          }
          retry_config {
            strategy
            max_retries
            backoff_ms
          }
          default_headers
          provider { name handle }
          canonical_ref
          is_owner
          webhook_count
          endpoint_count
          webhooks { id name description method request_body }
          resources { id name endpointCount }
        }
        serviceVersions(serviceId: $id, provider: $provider) { id name is_public status created_at updated_at }
      }
    `
      : `
      query($id: String!, $version: String) {
        service(id: $id, version: $version) {
          id
          name
          slug
          description
          base_url
          current_service_version
          servers {
            url
            description
            environment
            is_default
          }
          is_public
          watch_for_drift
          created_at
          updated_at
          source_url
          import_method
          import_warnings {
            id
            endpoint_id
            method
            path
            operation_id
            reasons
            recommendation
            source
            created_at
          }
          auth_configs {
            type
            flow
            scheme
            location
            key_name
            token_url
            authorization_url
            open_id_connect_url
            scopes
          }
          event_extraction_path
          incoming_webhook_config {
            auth_type
            auth_location
            auth_key_name
            signature_header
          }
          rate_limit {
            strategy
            requests_per_second
            requests_per_minute
          }
          retry_config {
            strategy
            max_retries
            backoff_ms
          }
          default_headers
          provider { name handle }
          canonical_ref
          is_owner
          webhook_count
          endpoint_count
          webhooks { id name description method request_body }
          resources { id name endpointCount }
        }
        serviceVersions(serviceId: $id) { id name is_public status created_at updated_at }
      }
    `;
    const variables: Record<string, string | undefined> = {
      id: id as string,
      version,
    };
    if (provider) variables.provider = provider;
    api
      .graphql<{
        service: Service;
        serviceVersions: ServiceGenerationResult["serviceVersions"];
      }>(queryStr, variables)
      .then(async (data) => {
        const service = data.service;
        if (!service) {
          if (!isAuth) {
            window.location.href = `/login?next=${encodeURIComponent(
              location.pathname + location.search
            )}`;
            return;
          }
          throw new Error("Service not found");
        }

        setRes(() => {
          return {
            service,
            serviceVersions: data.serviceVersions || [],
            integrations: [],
          };
        });
      })
      .catch((e) => setError(e.message))
      .finally(() => setLoading(false));
  }
  useEffect(() => {
    loadData();
  }, [id, version]);

  const { importProgress } = useImportSession(
    importSessionId,
    (payload) => {
      setRes((prev) => {
        if (!prev) return prev;
        if (payload.path) {
          const ep: IntegrationObject = { ...payload, status: "active" };
          const resourceID = ep.resource_id;
          if (resourceID) {
            setIntegrationsByResource(
              (prevRes: Record<string, IntegrationObject[]>) => {
                const existing = prevRes[resourceID] || [];
                if (existing.some((e) => e.id === ep.id)) return prevRes;
                return { ...prevRes, [resourceID]: [...existing, ep] };
              }
            );
          }
          if (prev.integrations.some((e) => e.id === ep.id)) return prev;
          return { ...prev, integrations: [...prev.integrations, ep] };
        } else {
          if (prev.service.webhooks?.some((w) => w.id === payload.id))
            return prev;
          return {
            ...prev,
            service: {
              ...prev.service,
              webhooks: [...(prev.service.webhooks || []), payload],
            },
          };
        }
      });
    },
    () => loadData(),
    () =>
      setSearchParams(
        (prev) => {
          prev.delete("importSession");
          return prev;
        },
        { replace: true, preventScrollReset: true }
      )
  );

  const autoExpandedServiceId = useRef<string | null>(null);

  useEffect(() => {
    if (res?.service?.id && autoExpandedServiceId.current !== res.service.id && res?.service?.resources && res.service.resources.length > 0) {
      autoExpandedServiceId.current = res.service.id;
      const firstThree = res.service.resources.slice(0, 3);
      firstThree.forEach((resource) => {
        // Expand instead of toggle, but only if we haven't loaded it yet
        if (!integrationsByResource[resource.id] && !loadingResources[resource.id] && !expandedResources[resource.name]) {
          toggleResource(resource.id, resource.name);
        }
      });
    }
  }, [res?.service?.id, res?.service?.resources]); // toggleResource intentionally omitted so it only triggers on service change

  useEffect(() => {
    if (res?.service?.name) {
      document.title = `${
        res.service.name.charAt(0).toUpperCase() + res.service.name.slice(1)
      } - Fused`;
    }
  }, [res]);

  async function handleApply(driftId: string) {
    if (!id || !serviceId || !res) return;
    setDriftAction(driftId);
    try {
      await refreshContractFromDrift(driftId);
    } catch (e: unknown) {
      toast.error(
        e instanceof Error ? e.message : "Failed to plan the source refresh"
      );
    } finally {
      setDriftAction(null);
    }
  }

  async function refreshContractFromDrift(driftId: string) {
    if (!res || !serviceId) return;
    const sourceURL = requireRemoteSource(res.service.source_url);
    const targetVersion = resolveProviderVersion(
      version,
      res.serviceVersions || []
    );
    const plan = await api.integrations.planImport({
      name: res.service.name,
      slug: res.service.slug,
      version: targetVersion,
      source_url: sourceURL,
    });
    const confirmed = await toast.confirm(importPlanConfirmation(plan));
    if (!confirmed) return;
    await api.integrations.applyImport(plan.plan_id, plan.source_hash);
    await api.integrations.dismissDrift(serviceId, driftId);
    setDrift((items) => items.filter((item) => item.id !== driftId));
    await loadData();
  }

  async function handleDismiss(driftId: string) {
    if (!id || !serviceId) return;
    setDriftAction(driftId);
    try {
      await api.integrations.dismissDrift(serviceId, driftId);
      setDrift((d) => d.filter((s) => s.id !== driftId));
      setRes((r) =>
        r
          ? {
              ...r,
              integrations: r.integrations.map((i) => ({
                ...i,
                status: "active",
              })),
            }
          : r
      );
    } catch (e: unknown) {
      toast.error(e instanceof Error ? e.message : "Failed");
    } finally {
      setDriftAction(null);
    }
  }

  async function handleToggleDriftWatch(
    e: React.ChangeEvent<HTMLInputElement>
  ) {
    if (!id || !res || !serviceId) return;
    const watch = e.target.checked;
    setSavingDrift(true);
    try {
      await api.integrations.updateDriftWatch(serviceId, watch);
      setRes({ ...res, service: { ...res.service, watch_for_drift: watch } });
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed to update drift watch setting");
    } finally {
      setSavingDrift(false);
    }
  }

  async function handleTogglePublic() {
    if (!id || !res) return;
    try {
      const newStatus = !res.service.is_public;
      await api.integrations.updatePublic(res.service.id, newStatus);
      setRes({
        ...res,
        service: { ...res.service, is_public: newStatus },
      });
    } catch (e) {
      toast.error("Failed to update public status: " + (e instanceof Error ? e.message : String(e)));
    }
  }

  // handleToggleVersionPublic is handleTogglePublic's per-version sibling: it
  // flips is_public for just the currently viewed version (the same one
  // VersionSelector shows/selects), independent of the service-level Public
  // toggle above. Server-side, this is owner-only -- enforced by the Registry
  // handler, mirrored here by only rendering the control when canManage.
  async function handleToggleVersionPublic() {
    if (!id || !res || !currentVersionEntry) return;
    try {
      const newStatus = !currentVersionEntry.is_public;
      await api.integrations.updateVersionPublic(
        res.service.id,
        currentVersionEntry.name,
        newStatus
      );
      setRes({
        ...res,
        serviceVersions: (res.serviceVersions || []).map((v) =>
          v.name === currentVersionEntry.name ? { ...v, is_public: newStatus } : v
        ),
      });
    } catch (e) {
      toast.error("Failed to update version public status: " + (e instanceof Error ? e.message : String(e)));
    }
  }

  async function handleClearImportWarnings() {
    if (!id || !res) return;
    try {
      await api.integrations.clearImportWarnings(res.service.id);
      setRes({
        ...res,
        service: { ...res.service, import_warnings: [] },
      });
      toast.success("Import warnings cleared.");
    } catch (e) {
      toast.error("Failed to clear import warnings: " + (e instanceof Error ? e.message : String(e)));
    }
  }

  if (loading && !res)
    return (
      <div className="flex-1 flex flex-col items-center justify-center min-h-[400px] text-slate-400">
        <Loader2 className="w-8 h-8 text-blue-500 animate-spin mb-4" />
        <p className="animate-pulse font-medium text-slate-500">
          Loading service details...
        </p>
      </div>
    );
  if (error && !res) return <p className="text-sm text-red-600">{error}</p>;
  if (!res) return null;

  const srv = res.service;
  const canManage = canManageService(isAuth, srv.is_owner, paramId, provider);
  const serviceVersions =
    res.serviceVersions ||
    loaderData?.initialServiceData?.serviceVersions ||
    [];
  // currentVersionEntry backs the selected version state: the same
  // "currently viewed version" resolution WorkspaceConnectionProfileSection
  // already uses below (URL ?version= param, falling back to the service's
  // default version).
  const currentVersionEntry = serviceVersions.find(
    (v) => v.name === (version || srv.current_service_version)
  );
  const integrations = res.integrations || [];
  const totalEndpoints =
    searchResults !== null
      ? searchResults.length
      : srv.resources?.reduce(
          (sum: number, r) => sum + (r.endpointCount || 0),
          0
        ) || 0;
  const hasDrift = integrations.some((i) => i.status === "drifted");
  const importWarnings = srv.import_warnings || [];
  const overallStatus = hasDrift ? "drifted" : "active";
  const statusColor =
    {
      active: "bg-green-50 text-green-700",
      drifted: "bg-yellow-50 text-yellow-700",
      updating: "bg-blue-50 text-blue-700",
    }[overallStatus] || "bg-slate-50 text-slate-700";

  return (
    <div className="min-w-0 space-y-5 sm:space-y-6">
      {/* Header */}
      <div className="flex min-w-0 flex-col gap-4 xl:flex-row xl:items-start xl:justify-between">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-3 mb-1">
            <Link
              to="/integrations"
              className="text-sm text-slate-400 hover:text-slate-600"
            >
              Back to services
            </Link>
          </div>
          <div className="flex min-w-0 flex-wrap items-center gap-2">
            <h1 className="text-xl font-semibold text-slate-900">
              {formatServiceName(srv.name)}
            </h1>
            <VersionSelector
              currentVersionTag={srv.current_service_version}
              versions={serviceVersions}
            />
            {currentVersionEntry?.status === "deprecated" ? (
              <Badge label="DEPRECATED VERSION" color="bg-amber-50 text-amber-700" />
            ) : currentVersionEntry?.status === "draft" ? (
              <Badge label="DRAFT VERSION" color="bg-slate-100 text-slate-600" />
            ) : currentVersionEntry?.status === "public" ||
              currentVersionEntry?.is_public ? (
              <Badge label="PUBLIC VERSION" color="bg-slate-100 text-slate-600" />
            ) : null}
            {overallStatus !== "active" && (
              <Badge label={overallStatus} color={statusColor} />
            )}
            <ServiceHeaderWarningIcon warningCount={importWarnings.length} />
            <div className="relative" ref={shareMenuRef}>
              <button
                data-track="open_share_menu"
                onClick={() => setShowShareMenu(!showShareMenu)}
                className="p-1.5 text-slate-400 hover:text-slate-600 hover:bg-slate-100 rounded-md transition-colors"
                title="Share Integration"
              >
                <Share2 className="w-4 h-4" />
              </button>
              {showShareMenu && (
                <div className="absolute right-0 top-full mt-1 w-48 bg-white border border-slate-200 rounded-lg shadow-lg z-50 py-1 overflow-hidden animate-in fade-in slide-in-from-top-2 duration-200 sm:left-0 sm:right-auto">
                  <button
                    data-track="share_on_twitter"
                    onClick={() => {
                      const url = window.location.href;
                      window.open(
                        `https://twitter.com/intent/tweet?url=${encodeURIComponent(
                          url
                        )}&text=${encodeURIComponent(
                          `Check out the ${srv.name} integration on Fused!`
                        )}`,
                        "_blank"
                      );
                      setShowShareMenu(false);
                    }}
                    className="w-full px-3 py-2 text-left text-sm text-slate-700 hover:bg-slate-50 flex items-center gap-2"
                  >
                    <MessageSquare className="w-4 h-4 text-sky-500" />
                    Share on Twitter
                  </button>
                  <button
                    data-track="share_on_linkedin"
                    onClick={() => {
                      const url = window.location.href;
                      window.open(
                        `https://www.linkedin.com/sharing/share-offsite/?url=${encodeURIComponent(
                          url
                        )}`,
                        "_blank"
                      );
                      setShowShareMenu(false);
                    }}
                    className="w-full px-3 py-2 text-left text-sm text-slate-700 hover:bg-slate-50 flex items-center gap-2"
                  >
                    <Briefcase className="w-4 h-4 text-blue-600" />
                    Share on LinkedIn
                  </button>
                  <button
                    data-track="copy_integration_link"
                    onClick={() => {
                      navigator.clipboard.writeText(window.location.href);
                      toast.success("Link copied to clipboard");
                      setShowShareMenu(false);
                    }}
                    className="w-full px-3 py-2 text-left text-sm text-slate-700 hover:bg-slate-50 flex items-center gap-2 border-t border-slate-100 mt-1"
                  >
                    <Copy className="w-4 h-4 text-slate-400" />
                    Copy Link
                  </button>
                </div>
              )}
            </div>
          </div>
          {(srv.provider || srv.canonical_ref) && (
            <div className="mt-1 flex min-w-0 flex-wrap items-center gap-1.5 text-sm text-slate-500">
              {srv.provider && <span className="break-words">Provider: {srv.provider.name}</span>}
              {srv.provider && srv.canonical_ref && <span>·</span>}
              {srv.canonical_ref && <CopyableCanonicalRef text={srv.canonical_ref} />}
            </div>
          )}
          <ServerDisplay srv={srv} isAuth={false} />
        </div>
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2 xl:justify-end">
          {canManage ? (
            <div className="relative" ref={visibilityMenuRef}>
              <button
                type="button"
                aria-expanded={showVisibilityMenu}
                aria-haspopup="dialog"
                onClick={() => setShowVisibilityMenu((open) => !open)}
                className="inline-flex h-9 items-center gap-2 rounded-md border border-slate-200 bg-white px-3 text-sm font-medium text-slate-700 shadow-sm transition-colors hover:border-slate-300 hover:bg-slate-50 focus:outline-none focus:ring-2 focus:ring-blue-500 focus:ring-offset-1"
              >
                {srv.is_public ? (
                  <Globe2 className="h-4 w-4 text-blue-600" />
                ) : (
                  <Lock className="h-4 w-4 text-slate-500" />
                )}
                {srv.is_public ? "Public service" : "Private service"}
                <ChevronDown className="h-3.5 w-3.5 text-slate-400" />
              </button>
              {showVisibilityMenu && (
                <div
                  role="dialog"
                  aria-label="Service visibility"
                  className="absolute left-0 top-full z-50 mt-2 w-[min(22rem,calc(100vw-2rem))] rounded-md border border-slate-200 bg-white p-4 shadow-xl xl:left-auto xl:right-0"
                >
                  <h2 className="text-sm font-semibold text-slate-900">Visibility</h2>
                  <div className="mt-3 divide-y divide-slate-100">
                    <div className="flex items-start justify-between gap-4 pb-3">
                      <div>
                        <p className="text-sm font-medium text-slate-800">Service</p>
                        <p className="mt-0.5 text-xs leading-5 text-slate-500">
                          {srv.is_public
                            ? "Discoverable outside your workspace."
                            : "Only your workspace can discover it."}
                        </p>
                      </div>
                      <button
                        type="button"
                        role="switch"
                        aria-label="Make service public"
                        aria-checked={!!srv.is_public}
                        onClick={handleTogglePublic}
                        className={`relative mt-0.5 inline-flex h-5 w-9 shrink-0 rounded-full transition-colors focus:outline-none focus:ring-2 focus:ring-blue-500 focus:ring-offset-2 ${
                          srv.is_public ? "bg-slate-900" : "bg-slate-300"
                        }`}
                      >
                        <span
                          className={`absolute top-0.5 h-4 w-4 rounded-full bg-white shadow-sm transition-transform ${
                            srv.is_public ? "translate-x-4" : "translate-x-0.5"
                          }`}
                        />
                      </button>
                    </div>
                    {currentVersionEntry && (
                      <div className="flex items-start justify-between gap-4 pt-3">
                        <div>
                          <p className="text-sm font-medium text-slate-800">Consumer access</p>
                          <p className="mt-0.5 text-xs leading-5 text-slate-500">
                            {currentVersionEntry.is_public
                              ? "Available when the service is public."
                              : "This version is held back from consumers."}
                          </p>
                        </div>
                        <button
                          type="button"
                          role="switch"
                          aria-label="Allow consumer access to this version"
                          aria-checked={!!currentVersionEntry.is_public}
                          onClick={handleToggleVersionPublic}
                          className={`relative mt-0.5 inline-flex h-5 w-9 shrink-0 rounded-full transition-colors focus:outline-none focus:ring-2 focus:ring-blue-500 focus:ring-offset-2 ${
                            currentVersionEntry.is_public
                              ? "bg-slate-900"
                              : "bg-slate-300"
                          }`}
                        >
                          <span
                            className={`absolute top-0.5 h-4 w-4 rounded-full bg-white shadow-sm transition-transform ${
                              currentVersionEntry.is_public
                                ? "translate-x-4"
                                : "translate-x-0.5"
                            }`}
                          />
                        </button>
                      </div>
                    )}
                  </div>
                </div>
              )}
            </div>
          ) : srv.is_public ? (
            <Badge label="PUBLIC SERVICE" color="bg-blue-50 text-blue-700" />
          ) : (
            <Badge label="PRIVATE SERVICE" color="bg-slate-100 text-slate-600" />
          )}
          {canManage && (
            <div className="flex flex-col items-start group relative">
              <label
                className={`flex items-center gap-2 text-sm transition-colors ${
                  srv.source_url === "uploaded://spec"
                    ? "text-slate-400 cursor-not-allowed"
                    : "text-slate-700 cursor-pointer hover:text-slate-900"
                }`}
              >
                <input
                  type="checkbox"
                  checked={!!srv.watch_for_drift}
                  onChange={handleToggleDriftWatch}
                  disabled={savingDrift || srv.source_url === "uploaded://spec"}
                  className="w-4 h-4 text-blue-600 rounded border-slate-300 focus:ring-blue-500 disabled:opacity-50"
                />
                Watch for Drift
              </label>
              {srv.source_url === "uploaded://spec" && (
                <div className="absolute top-full mt-1 hidden group-hover:block z-10 w-48 bg-slate-800 text-white text-xs rounded p-2 shadow-lg">
                  We can't monitor this for changes. To enable drift detection,
                  provide a URL.
                </div>
              )}
            </div>
          )}
          {isAuth && workspaceServiceActive === false && (
            <AddToWorkspaceButton
              serviceId={srv.id}
              serviceName={srv.name}
              versionTag={srv.current_service_version}
              onAdded={() => setWorkspaceServiceActive(true)}
            />
          )}
        </div>
      </div>

      <div className="flex gap-4 mb-4 text-xs">
        {srv.import_method && canManage && (
          <span className="bg-slate-100 text-slate-600 px-2 py-1 rounded-md font-medium border border-slate-200 uppercase tracking-wider">
            {srv.import_method === "openapi"
              ? "Imported via OpenAPI"
              : "Imported via Docs"}
          </span>
        )}
      </div>

      {srv.description && (
        <p className="text-sm text-slate-600">{srv.description}</p>
      )}

      <ImportWarningPanel
        warnings={importWarnings}
        onClear={handleClearImportWarnings}
      />

      {isAuth && serviceNotifications.length > 0 && (
        <NotificationBanner
          items={serviceNotifications}
          serviceRefs={notificationServiceRefs}
          onMarkRead={markNotificationRead}
          onDismiss={dismissNotification}
        />
      )}

      {importProgress && (
        <div
          className={`border rounded-lg px-4 py-3 flex items-start gap-3 ${
            importProgress.error
              ? "bg-red-50 border-red-200 text-red-700"
              : importProgress.active
              ? "bg-blue-50 border-blue-200 text-blue-800"
              : "bg-green-50 border-green-200 text-green-700"
          }`}
        >
          {importProgress.error ? (
            <AlertTriangle className="w-4 h-4 mt-0.5 shrink-0" />
          ) : importProgress.active ? (
            <Loader2 className="w-4 h-4 mt-0.5 shrink-0 animate-spin" />
          ) : (
            <Check className="w-4 h-4 mt-0.5 shrink-0" />
          )}
          <div>
            <p className="text-sm font-medium">
              {importProgress.active
                ? "Still Extracting"
                : importProgress.error
                ? "Extraction Failed"
                : "Extraction Complete"}
            </p>
            <p className="text-sm opacity-90">
              {importProgress.error || importProgress.status}
            </p>
          </div>
        </div>
      )}
      <section className="bg-white border border-slate-200 rounded-lg p-4 sm:p-5">
        <h2 className="text-sm font-semibold text-slate-900 mb-4">
          Service configuration
        </h2>
        <dl className="grid sm:grid-cols-2 lg:grid-cols-4 gap-4 text-sm">
          {/* Base URL */}
          <details className={`group bg-slate-50 border border-slate-100 rounded-lg p-3 ${srv.servers && srv.servers.length > 1 ? "cursor-pointer hover:bg-slate-100 transition-colors" : ""}`}>
            <summary className="list-none flex items-start justify-between outline-none">
              <div>
                <dt className="text-xs text-slate-500">Base URL</dt>
                <dd className="mt-1 font-mono text-xs break-all text-slate-800">
                  {srv.base_url || (srv.servers?.[0]?.url) || "Not declared"}
                </dd>
              </div>
              {srv.servers && srv.servers.length > 1 && (
                <ChevronDown className="w-4 h-4 text-slate-400 mt-1 transition-transform group-open:rotate-180 shrink-0" />
              )}
            </summary>
            {srv.servers && srv.servers.length > 1 && (
              <div className="mt-3 pt-3 border-t border-slate-200 text-xs text-slate-700 space-y-2">
                {srv.servers.map((server, i: number) => (
                  <div key={i}>
                    <div className="font-mono text-slate-800 break-all">{server.url}</div>
                    <div className="flex gap-2 text-slate-500 mt-0.5">
                      {server.environment && <span>{server.environment}</span>}
                      {server.is_default && <span className="bg-blue-100 text-blue-700 px-1 rounded text-[10px] uppercase font-bold">Default</span>}
                    </div>
                  </div>
                ))}
              </div>
            )}
          </details>

          {/* Authentication */}
          <details className={`group bg-slate-50 border border-slate-100 rounded-lg p-3 ${srv.auth_configs && srv.auth_configs.length > 0 ? "cursor-pointer hover:bg-slate-100 transition-colors" : ""}`}>
            <summary className="list-none flex items-start justify-between outline-none">
              <div>
                <dt className="text-xs text-slate-500">Authentication</dt>
                <dd className="mt-1 text-slate-800">
                  {srv.auth_configs?.map((auth) => auth.type).join(", ") || "None"}
                </dd>
              </div>
              {srv.auth_configs && srv.auth_configs.length > 0 && (
                <ChevronDown className="w-4 h-4 text-slate-400 mt-1 transition-transform group-open:rotate-180 shrink-0" />
              )}
            </summary>
            {srv.auth_configs && srv.auth_configs.length > 0 && (
              <div className="mt-3 pt-3 border-t border-slate-200 text-xs text-slate-700 space-y-3">
                {srv.auth_configs.map((auth, i: number) => (
                  <div key={i} className={i > 0 ? "pt-3 border-t border-slate-200" : ""}>
                    {auth.type && <div className="flex justify-between"><span className="text-slate-500">Type</span><span className="font-medium text-slate-900">{auth.type}</span></div>}
                    {auth.flow && <div className="flex justify-between mt-1"><span className="text-slate-500">Flow</span><span className="font-medium text-slate-900">{auth.flow}</span></div>}
                    {auth.scheme && <div className="flex justify-between mt-1"><span className="text-slate-500">Scheme</span><span className="font-medium text-slate-900">{auth.scheme}</span></div>}
                    {auth.location && <div className="flex justify-between mt-1"><span className="text-slate-500">Location</span><span className="font-medium text-slate-900">{auth.location}</span></div>}
                    {auth.key_name && <div className="flex justify-between mt-1"><span className="text-slate-500">Key</span><span className="font-mono text-slate-900">{auth.key_name}</span></div>}
                    {auth.scopes && auth.scopes.length > 0 && (
                      <div className="mt-2 min-w-0">
                        <span className="mb-1 block text-slate-500">
                          Scopes ({auth.scopes.length})
                        </span>
                        <div className="max-h-40 overflow-y-auto overscroll-contain rounded-md border border-slate-200 bg-white [scrollbar-gutter:stable]">
                          <ul className="divide-y divide-slate-100">
                            {auth.scopes.map((scope: string, scopeIndex: number) => (
                              <li
                                key={`${scope}-${scopeIndex}`}
                                className="break-words px-2 py-1.5 font-mono text-[11px] leading-4 text-slate-700"
                              >
                                {scope}
                              </li>
                            ))}
                          </ul>
                        </div>
                      </div>
                    )}
                  </div>
                ))}
              </div>
            )}
          </details>

          {/* Rate limit */}
          <details className={`group bg-slate-50 border border-slate-100 rounded-lg p-3 ${srv.rate_limit ? "cursor-pointer hover:bg-slate-100 transition-colors" : ""}`}>
            <summary className="list-none flex items-start justify-between outline-none">
              <div>
                <dt className="text-xs text-slate-500">Rate limit</dt>
                <dd className="mt-1 text-slate-800">
                  {srv.rate_limit?.strategy || "Not declared"}
                </dd>
              </div>
              {srv.rate_limit && (
                <ChevronDown className="w-4 h-4 text-slate-400 mt-1 transition-transform group-open:rotate-180 shrink-0" />
              )}
            </summary>
            {srv.rate_limit && (
              <div className="mt-3 pt-3 border-t border-slate-200 text-xs text-slate-700 space-y-1">
                <div className="flex justify-between"><span className="text-slate-500">Strategy</span><span className="font-medium text-slate-900">{srv.rate_limit.strategy}</span></div>
                {srv.rate_limit.requests_per_second != null && (
                  <div className="flex justify-between"><span className="text-slate-500">Per second</span><span className="font-medium text-slate-900">{srv.rate_limit.requests_per_second}</span></div>
                )}
                {srv.rate_limit.requests_per_minute != null && (
                  <div className="flex justify-between"><span className="text-slate-500">Per minute</span><span className="font-medium text-slate-900">{srv.rate_limit.requests_per_minute}</span></div>
                )}
              </div>
            )}
          </details>

          {/* Retry */}
          <details className={`group bg-slate-50 border border-slate-100 rounded-lg p-3 ${srv.retry_config ? "cursor-pointer hover:bg-slate-100 transition-colors" : ""}`}>
            <summary className="list-none flex items-start justify-between outline-none">
              <div>
                <dt className="text-xs text-slate-500">Retry</dt>
                <dd className="mt-1 text-slate-800">
                  {srv.retry_config?.strategy || "Not declared"}
                </dd>
              </div>
              {srv.retry_config && (
                <ChevronDown className="w-4 h-4 text-slate-400 mt-1 transition-transform group-open:rotate-180 shrink-0" />
              )}
            </summary>
            {srv.retry_config && (
              <div className="mt-3 pt-3 border-t border-slate-200 text-xs text-slate-700 space-y-1">
                <div className="flex justify-between"><span className="text-slate-500">Strategy</span><span className="font-medium text-slate-900">{srv.retry_config.strategy}</span></div>
                {srv.retry_config.max_retries != null && (
                  <div className="flex justify-between"><span className="text-slate-500">Max retries</span><span className="font-medium text-slate-900">{srv.retry_config.max_retries}</span></div>
                )}
                {srv.retry_config.backoff_ms != null && (
                  <div className="flex justify-between"><span className="text-slate-500">Backoff</span><span className="font-medium text-slate-900">{srv.retry_config.backoff_ms}ms</span></div>
                )}
              </div>
            )}
          </details>
        </dl>
      </section>

      {srv.incoming_webhook_config && (
        <section className="bg-white border border-slate-200 rounded-lg p-4 mt-6 mb-6 sm:p-5">
          <h2 className="text-sm font-semibold text-slate-900 mb-4">
            Webhook configuration
          </h2>
          <dl className="grid sm:grid-cols-2 lg:grid-cols-4 gap-4 text-sm">
            {/* Webhook verification */}
            <details className={`group bg-slate-50 border border-slate-100 rounded-lg p-3 ${srv.incoming_webhook_config ? "cursor-pointer hover:bg-slate-100 transition-colors" : ""}`}>
              <summary className="list-none flex items-start justify-between outline-none">
                <div>
                  <dt className="text-xs text-slate-500">Webhook verification</dt>
                  <dd className="mt-1 text-slate-800">
                    {srv.incoming_webhook_config.auth_type || "None"}
                  </dd>
                </div>
                {srv.incoming_webhook_config && (
                  <ChevronDown className="w-4 h-4 text-slate-400 mt-1 transition-transform group-open:rotate-180 shrink-0" />
                )}
              </summary>
              {srv.incoming_webhook_config && (
                <div className="mt-3 pt-3 border-t border-slate-200 text-xs text-slate-700 space-y-1">
                  {srv.incoming_webhook_config.auth_type && <div className="flex justify-between"><span className="text-slate-500">Auth type</span><span className="font-medium text-slate-900">{srv.incoming_webhook_config.auth_type}</span></div>}
                  {srv.incoming_webhook_config.auth_location && <div className="flex justify-between"><span className="text-slate-500">Location</span><span className="font-medium text-slate-900">{srv.incoming_webhook_config.auth_location}</span></div>}
                  {srv.incoming_webhook_config.auth_key_name && <div className="flex justify-between"><span className="text-slate-500">Key name</span><span className="font-mono text-slate-900">{srv.incoming_webhook_config.auth_key_name}</span></div>}
                  {srv.incoming_webhook_config.signature_header && <div className="flex justify-between"><span className="text-slate-500">Sig. header</span><span className="font-mono text-slate-900">{srv.incoming_webhook_config.signature_header}</span></div>}
                </div>
              )}
            </details>
          </dl>
        </section>
      )}


      {serviceId && (
        <WorkspaceConnectionProfileSection
          serviceId={serviceId}
          serviceVersionId={
            serviceVersions.find(
              (item) => item.name === (version || srv.current_service_version)
            )?.id
          }
          serviceVersion={version || srv.current_service_version || ""}
          authConfigs={srv.auth_configs || []}
          isOwner={canManage}
        />
      )}

      {/* Tabs */}
      <div className="flex max-w-full overflow-x-auto whitespace-nowrap rounded-lg bg-slate-100/80 p-1 mb-6 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
        <button
          data-track="view_endpoints_tab"
          onClick={() => handleTabChange("endpoints")}
          className={`shrink-0 rounded-md px-3 py-1.5 text-xs font-medium transition-all cursor-pointer sm:px-4 sm:text-sm ${
            activeTab === "endpoints"
              ? "bg-white text-slate-900 shadow-sm"
              : "text-slate-500 hover:text-slate-700"
          }`}
        >
          Operations ({totalEndpoints})
        </button>
        <button
          data-track="view_webhooks_tab"
          onClick={() => handleTabChange("webhooks")}
          className={`shrink-0 rounded-md px-3 py-1.5 text-xs font-medium transition-all cursor-pointer sm:px-4 sm:text-sm ${
            activeTab === "webhooks"
              ? "bg-white text-slate-900 shadow-sm"
              : "text-slate-500 hover:text-slate-700"
          }`}
        >
          Webhooks ({srv.webhook_count ?? 0})
        </button>
        {isAuth && workspaceServiceActive === true && (
          <button
            data-track="view_activity_tab"
            onClick={() => handleTabChange("analytics")}
            className={`shrink-0 rounded-md px-3 py-1.5 text-xs font-medium transition-all cursor-pointer sm:px-4 sm:text-sm ${
              activeTab === "analytics"
                ? "bg-white text-slate-900 shadow-sm"
                : "text-slate-500 hover:text-slate-700"
            }`}
          >
            Activity
          </button>
        )}
      </div>

      {isClientFetchingTab && (
        <div className="absolute top-1/2 left-1/2 -translate-x-1/2 -translate-y-1/2 z-10 flex flex-col items-center justify-center bg-white/80 backdrop-blur-sm p-4 rounded-lg shadow-sm border border-slate-100">
          <Loader2 className="w-6 h-6 text-blue-500 animate-spin mb-2" />
          <p className="text-xs font-medium text-slate-500">Updating...</p>
        </div>
      )}

      <div className="relative">
        <>
          {/* Endpoints Tab */}
          {activeTab === "endpoints" && (
            <EndpointsTab
              res={res}
              searchQuery={searchQuery}
              setSearchQuery={setSearchQuery}
              searchResults={searchResults}
              isSearching={isSearching}
              handleSearch={handleSearch}
              handleClearSearch={handleClearSearch}
              resourceVersions={resourceVersions}
              setResourceVersions={setResourceVersions}
              expandedResources={expandedResources}
              integrationsByResource={integrationsByResource}
              loadingResources={loadingResources}
              toggleResource={toggleResource}
              hasMoreResources={hasMoreResources}
              loadMoreEndpoints={loadMoreEndpoints}
              hasMoreSearch={hasMoreSearch}
              loadMoreSearchResults={loadMoreSearchResults}
              selectedEndpoint={selectedEndpoint}
              setSelectedEndpoint={setSelectedEndpoint}
            />
          )}

          {/* Webhooks Tab */}
          {activeTab === "webhooks" && (
            <WebhooksTab srv={srv} setSelectedEndpoint={setSelectedEndpoint} />
          )}

          {/* Activity Tab */}
          {activeTab === "analytics" && (
            <ActivityTab
              res={res}
              executionEvents={executionEvents}
              executionTotal={executionTotal}
              executionPage={executionPage}
              setExecutionPage={setExecutionPage}
              executionLimit={executionLimit}
              executionTransport={executionTransport}
              setExecutionTransport={(transport) => {
                setExecutionPage(1);
                setExecutionTransport(transport);
              }}
              executionStatus={executionStatus}
              setExecutionStatus={(status) => {
                setExecutionPage(1);
                setExecutionStatus(status);
              }}
              executionAnalytics={executionAnalytics}
              webhookEvents={webhookEvents}
              webhookTotal={webhookTotal}
              webhookPage={webhookPage}
              setWebhookPage={(page) => {
                const nextPage =
                  typeof page === "function" ? page(webhookPage) : page;
                setWebhookPage(nextPage);
                syncWebhookParams({ page: nextPage });
              }}
              webhookLimit={webhookLimit}
              webhookFilterEvent={webhookFilterEvent}
              setWebhookFilterEvent={setWebhookFilterEvent}
              webhookStartDate={webhookStartDate}
              setWebhookStartDate={(date) => {
                setWebhookStartDate(date);
                syncWebhookParams({ startDate: date });
              }}
              webhookEndDate={webhookEndDate}
              setWebhookEndDate={(date) => {
                setWebhookEndDate(date);
                syncWebhookParams({ endDate: date });
              }}
              webhookAnalytics={webhookAnalytics}
              loadWebhookData={loadWebhookData}
              dependentSDKs={dependentSDKs}
              dependentMCPs={dependentMCPs}
            />
          )}
        </>
      </div>

      {/* Metadata */}
      <div className="text-xs text-slate-400 flex flex-col sm:flex-row sm:items-center sm:flex-wrap gap-2 sm:gap-6 border-t border-slate-100 pt-6 mt-8">
        <span suppressHydrationWarning>
          Created {new Date(srv.created_at).toLocaleString()}
        </span>
        <span suppressHydrationWarning>
          Updated {new Date(srv.updated_at).toLocaleString()}
        </span>
        <span className="font-mono break-all">
          Source: {srv.source_url || "N/A"}
        </span>
      </div>

      {/* Side Panel for Endpoint Details */}
      {selectedEndpoint && (
        <EndpointDetailsSidebar
          selectedEndpoint={selectedEndpoint}
          setSelectedEndpoint={setSelectedEndpoint}
          srv={srv}
          drift={drift}
          driftAction={driftAction}
          handleDismiss={handleDismiss}
          handleApply={handleApply}
        />
      )}
    </div>
  );
}
// To be moved to top

function CopyableCanonicalRef({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <span
      onClick={() => {
        navigator.clipboard.writeText(text);
        setCopied(true);
        setTimeout(() => setCopied(false), 2000);
      }}
      className="relative break-all cursor-pointer group hover:text-slate-700 transition-colors"
      title="Click to copy"
    >
      {text}
      {copied && (
        <span className="absolute bottom-full mb-1 left-1/2 -translate-x-1/2 bg-slate-800 text-white text-[10px] px-1.5 py-0.5 rounded shadow-sm whitespace-nowrap z-10 pointer-events-none">
          Copied
          <span className="absolute top-full left-1/2 -translate-x-1/2 border-4 border-transparent border-t-slate-800" />
        </span>
      )}
    </span>
  );
}
