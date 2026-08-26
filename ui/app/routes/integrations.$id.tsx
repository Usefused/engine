import { createContext, useContext, useState, useEffect, useRef } from "react";
import { AuthNameField } from "~/components/AuthNameField";
import {
  useParams,
  Link,
  useSearchParams,
  useLoaderData,
  useRouteLoaderData,
  useLocation,
  type MetaFunction,
} from "@remix-run/react";

function serviceMetaTitle(service?: Service | null) {
  if (!service?.name) return "Service details - Fused";
  return `${service.name.charAt(0).toUpperCase() + service.name.slice(1)} - Fused`;
}

function serviceMetaDescription(service?: Service | null) {
  if (service?.description) return service.description;
  const name = service?.name
    ? service.name.charAt(0).toUpperCase() + service.name.slice(1)
    : "";
  return `Explore the ${name} API integration on Fused — generate typed SDKs, webhook receivers, and MCP servers.`;
}

function serviceMetaURL(service: Service | null | undefined, provider?: string) {
  const providerSegment = provider ? `${provider}/` : "";
  const serviceSegment = service?.slug ?? service?.id ?? "";
  return `https://usefused.com/integrations/${providerSegment}${serviceSegment}`;
}

export const meta: MetaFunction<typeof clientLoader> = ({
  data,
  matches,
  params,
}) => {
  const service = data?.initialServiceData?.service;
  const title = serviceMetaTitle(service);
  const description = serviceMetaDescription(service);
  // params.provider is only present when this meta function is reused by
  // integrations.$provider.$id.tsx (the cross-account public-browsing route).
  const url = serviceMetaURL(service, params.provider);

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
  type AuthConfig,
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
import { useResourceLoader } from "~/hooks/useResourceLoader";

import { redirect } from "@remix-run/react";
import { APIRequestError } from "~/lib/authorization-error";
import {
  RATE_LIMIT_GRAPHQL_FIELDS,
  rateLimitAlgorithmLabel,
  rateLimitPolicyName,
  rateLimitPolicyQuotaLabel,
  rateLimitSummary,
} from "~/lib/rate-limit";
import { RETRY_GRAPHQL_FIELDS, retrySummary } from "~/lib/retry-policy";
import { oauth2FlowEntries, oauth2ScopeNames } from "~/lib/oauth2-flows";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import {
  hasResourcePermission,
  hasWorkspacePermission,
} from "~/lib/current-actor-access";

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

type DetailTab = "endpoints" | "webhooks" | "analytics";

function resolvedRouteID(loaderID: string | null | undefined, routeID?: string) {
  return loaderID ?? routeID;
}

function resolvedDetailTab(value: string | null): DetailTab {
  const tabs: DetailTab[] = ["endpoints", "webhooks", "analytics"];
  return tabs.includes(value as DetailTab) ? (value as DetailTab) : "endpoints";
}

function positivePage(value: string | null): number {
  const page = Number.parseInt(value ?? "1", 10);
  return Number.isNaN(page) || page < 1 ? 1 : page;
}

function serviceNotificationsFor(
  serviceId: string | undefined,
  notifications: ReturnType<typeof useWorkspaceNotifications>["unresolved"]
) {
  if (!serviceId) return [];
  return notifications.filter(
    (item) => isPending(item) && matchesService(item, serviceId)
  );
}

function selectedServiceVersions(
  current: ServiceVersion[] | undefined,
  initial: ServiceVersion[] | undefined
) {
  return current ?? initial ?? [];
}

function selectedVersionName(version: string, fallback?: string) {
  return version || fallback || "";
}

// routeAuthenticated normalizes optional root-loader session state.
function routeAuthenticated(rootData?: { isAuth: boolean }): boolean {
  return Boolean(rootData?.isAuth);
}

// initialDetailService extracts the nullable service returned by the loader.
function initialDetailService(data: InitialServicePayload): Service | null {
  return data?.service || null;
}

// detailServiceVersions chooses refreshed rows before loader bootstrap rows.
function detailServiceVersions(
  result: ServiceGenerationResult | null,
  initialData: InitialServicePayload
): ServiceVersion[] {
  return selectedServiceVersions(
    generatedServiceVersions(result),
    initialData?.serviceVersions
  );
}

// detailSelectedVersion resolves the URL pin against the effective version.
function detailSelectedVersion(
  requested: string,
  result: ServiceGenerationResult | null,
  initialService: Service | null
): string {
  return selectedVersionName(
    requested,
    generatedService(result)?.current_service_version ||
      initialService?.current_service_version
  );
}

// queryValue returns a stable empty string for absent optional URL state.
function queryValue(params: URLSearchParams, key: string): string {
  return params.get(key) || "";
}

// serviceReadAllowed checks the exact service grant only after identity exists.
function serviceReadAllowed(
  access: Parameters<typeof hasResourcePermission>[0],
  serviceId?: string
): boolean {
  return hasResourcePermission(access, "service.read", "SERVICE", serviceId || "");
}

// activityReadAllowed combines service visibility with workspace audit access.
function activityReadAllowed(
  access: Parameters<typeof hasWorkspacePermission>[0],
  canReadService: boolean
): boolean {
  return canReadService && hasWorkspacePermission(access, "audit.read");
}

function endpointTotal(
  searchResults: IntegrationObject[] | null,
  service: Service
): number {
  if (searchResults !== null) return searchResults.length;
  return (
    service.resources?.reduce(
      (sum: number, resource) => sum + (resource.endpointCount || 0),
      0
    ) ?? 0
  );
}

function serviceStatus(integrations: IntegrationObject[]) {
  return integrations.some((integration) => integration.status === "drifted")
    ? "drifted"
    : "active";
}

function serviceStatusColor(status: string) {
  const colors: Record<string, string> = {
    active: "bg-green-50 text-green-700",
    drifted: "bg-yellow-50 text-yellow-700",
    updating: "bg-blue-50 text-blue-700",
  };
  return colors[status] ?? "bg-slate-50 text-slate-700";
}

type InitialServicePayload = {
  service?: Service | null;
  serviceVersions?: ServiceVersion[];
} | null;

function loaderInitialData(loaderData: {
  initialServiceData?: InitialServicePayload;
} | undefined): InitialServicePayload {
  return loaderData ? loaderData.initialServiceData ?? null : null;
}

function loaderResolvedID(
  loaderData: { resolvedId?: string | null } | undefined,
  routeID?: string
) {
  return resolvedRouteID(loaderData ? loaderData.resolvedId : null, routeID);
}

function loaderErrorMessage(loaderData: { error?: string | null } | undefined) {
  return loaderData ? loaderData.error ?? "" : "";
}

function generatedService(result: ServiceGenerationResult | null) {
  return result ? result.service : undefined;
}

function generatedServiceVersions(result: ServiceGenerationResult | null) {
  return result ? result.serviceVersions : undefined;
}

function serviceResources(result: ServiceGenerationResult | null) {
  return result ? result.service.resources : undefined;
}

function webhookRequestParams(input: {
  serviceId: string;
  page: number;
  limit: number;
  eventName: string;
  startDate: string;
  endDate: string;
}): Parameters<typeof api.workspace.listWebhookEvents>[0] {
  const params: Parameters<typeof api.workspace.listWebhookEvents>[0] = {
    serviceId: input.serviceId,
    limit: input.limit,
    offset: (input.page - 1) * input.limit,
  };
  if (input.eventName) params.eventName = input.eventName;
  if (input.startDate) params.startDate = new Date(input.startDate).toISOString();
  if (input.endDate) {
    const end = new Date(input.endDate);
    end.setHours(23, 59, 59, 999);
    params.endDate = end.toISOString();
  }
  return params;
}

function OAuth2FlowDetails({ auth }: { auth: AuthConfig }) {
  const entries = oauth2FlowEntries(auth);
  if (entries.length === 0) return null;

  return (
    <div className="mt-2 space-y-2">
      {entries.map(([name, flow]) => {
        const scopes = oauth2ScopeNames(flow);
        return (
          <div key={name} className="min-w-0 rounded-md border border-slate-200 bg-white p-2">
            <div className="flex justify-between">
              <span className="text-slate-500">Flow</span>
              <span className="font-medium text-slate-900">{name}</span>
            </div>
            {scopes.length > 0 && (
              <div className="mt-2 min-w-0">
                <span className="mb-1 block text-slate-500">Scopes ({scopes.length})</span>
                <ul className="max-h-40 divide-y divide-slate-100 overflow-y-auto overscroll-contain rounded-md border border-slate-200 [scrollbar-gutter:stable]">
                  {scopes.map((scope) => (
                    <li key={scope} className="break-words px-2 py-1.5 font-mono text-[11px] leading-4 text-slate-700">
                      {scope}
                    </li>
                  ))}
                </ul>
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}

// serviceDetailQuery keeps initial and refreshed service reads on one schema projection.
function serviceDetailQuery(provider?: string): string {
  if (provider) {
    return `
      query($id: String!, $version: String, $provider: String) {
        service(id: $id, version: $version, provider: $provider) {
          id name slug description base_url current_service_version servers { url description environment is_default }
          is_public watch_for_drift created_at updated_at source_url import_method
          import_warnings { id endpoint_id method path operation_id reasons recommendation source created_at }
          auth_configs {
            name type scheme basic_password_mode location key_name open_id_connect_url oauth2_metadata_url
            deprecated token_endpoint_auth_method token_request_media_type pkce_required scopes_delimiter
            extra_auth_params extra_token_params refresh_token_rotates oauth2_flows strategy policy_provenance
          }
          event_extraction_path
          incoming_webhook_config { auth_type auth_location auth_key_name signature_header }
          rate_limit { ${RATE_LIMIT_GRAPHQL_FIELDS} }
          retry_config { ${RETRY_GRAPHQL_FIELDS} }
          default_headers provider { name handle } canonical_ref is_owner webhook_count endpoint_count
          webhooks { id name description method request_body }
          resources { id name endpointCount }
        }
        serviceVersions(serviceId: $id, provider: $provider) { id name is_public status created_at updated_at }
      }
    `;
  }
  return `
    query($id: String!, $version: String) {
      service(id: $id, version: $version) {
        id name slug description base_url current_service_version servers { url description environment is_default }
        is_public watch_for_drift created_at updated_at source_url import_method
        import_warnings { id endpoint_id method path operation_id reasons recommendation source created_at }
        auth_configs {
          name type scheme basic_password_mode location key_name open_id_connect_url oauth2_metadata_url
          deprecated token_endpoint_auth_method token_request_media_type pkce_required scopes_delimiter
          extra_auth_params extra_token_params refresh_token_rotates oauth2_flows strategy policy_provenance
        }
        event_extraction_path
        incoming_webhook_config { auth_type auth_location auth_key_name signature_header }
        rate_limit { ${RATE_LIMIT_GRAPHQL_FIELDS} }
        retry_config { ${RETRY_GRAPHQL_FIELDS} }
        default_headers provider { name handle } canonical_ref is_owner webhook_count endpoint_count
        webhooks { id name description method request_body }
        resources { id name endpointCount }
      }
      serviceVersions(serviceId: $id) { id name is_public status created_at updated_at }
    }
  `;
}

function canonicalServiceRedirect(
  id: string,
  provider: string | undefined,
  service: Service | null | undefined,
  search: string
) {
  if (!isUUID(id) || !service?.slug || service.slug === id) return null;
  const prefix = provider ? `/integrations/${provider}` : "/integrations";
  return redirect(`${prefix}/${service.slug}${search}`);
}

function loaderErrorRedirect(
  error: unknown,
  isAuthenticated: boolean,
  next: string
) {
  if (
    isAuthenticated &&
    error instanceof APIRequestError &&
    error.status === 401
  ) {
    // Session status clears invalid cookies on the login page. A permission
    // denial is intentionally not a logout because the session remains valid.
    throw redirect(`/login?next=${encodeURIComponent(next)}`);
  }
  return error instanceof Error ? error.message : "";
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

  let initialServiceData = null;
  let resolvedId = id;
  let error = null;
  try {
    const variables: Record<string, string> = {
      id,
      version: version ?? "",
    };
    if (provider) variables.provider = provider;
    const res = await api.graphql<{
      service: Service | null;
      serviceVersions?: ServiceVersion[];
    }>(serviceDetailQuery(provider), variables);
    initialServiceData = res;

    if (!initialServiceData.service && !isAuthenticated) {
      return redirect(
        `/login?next=${encodeURIComponent(url.pathname + url.search)}`
      );
    }

    // The resolver returns the canonical UUID; use it so client always has the UUID
    if (initialServiceData.service?.id) {
      resolvedId = initialServiceData.service.id;
    }

    // Redirect UUID-based URLs to the canonical slug URL (preserving the
    // provider segment, if this is the cross-account route).
    const canonicalRedirect = canonicalServiceRedirect(
      id,
      provider,
      initialServiceData.service,
      url.search
    );
    if (canonicalRedirect) return canonicalRedirect;
  } catch (err) {
    error = loaderErrorRedirect(
      err,
      isAuthenticated,
      url.pathname + url.search
    );
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

// useIntegrationDetailModel owns version-pinned service detail state and actions.
/** Builds the service-detail state while keeping protected activity reads gated. */
function useIntegrationDetailModel() {
  const toast = useToast();
  const { access } = useCurrentActorAccess();
  const loaderData = useLoaderData<typeof clientLoader>();
  const { id: paramId, provider } = useParams<{
    id: string;
    provider?: string;
  }>();
  const location = useLocation();
  const id = loaderResolvedID(loaderData, paramId);
  const rootData = useRouteLoaderData<{ isAuth: boolean }>("root");
  const isAuth = routeAuthenticated(rootData);
  const initialServiceData = loaderInitialData(loaderData);
  const initialService = initialDetailService(initialServiceData);

  const [res, setRes] = useState<ServiceGenerationResult | null>(() => {
    if (initialService) {
      return {
        service: initialService,
        integrations: [],
      };
    }
    return null;
  });

  useEffect(() => {
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
  }, [initialServiceData]);

  // Only the loaded service row gives us the Engine/Registry UUID. The route
  // param may be a slug, so UUID-only admin panels must wait for `res`.
  const serviceId = generatedService(res)?.id;
  const canReadService = serviceReadAllowed(access, serviceId);
  const canReadActivity = activityReadAllowed(access, canReadService);

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
    !initialService
  );
  const [error, setError] = useState(loaderErrorMessage(loaderData));
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
  const version = queryValue(searchParams, "version");
  const serviceVersions = detailServiceVersions(res, initialServiceData);
  const selectedVersion = detailSelectedVersion(version, res, initialService);
  const currentVersionEntry = serviceVersions.find(
    (item) => item.name === selectedVersion
  );

  const {
    resourceVersions,
    setResourceVersions,
    expandedResources,
    integrationsByResource,
    loadingResources,
    hasMoreResources,
    toggleResource,
    loadMoreEndpoints,
  } = useResourceLoader(serviceId, currentVersionEntry?.id);

  const activeTab = resolvedDetailTab(searchParams.get("tab"));
  const urlWPage = positivePage(searchParams.get("wPage"));
  const urlWStartDate = queryValue(searchParams, "wStartDate");
  const urlWEndDate = queryValue(searchParams, "wEndDate");
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
  const [webhookPage, setWebhookPage] = useState(urlWPage);
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
  } = useEndpointSearch(
    serviceId,
    selectedVersion,
    res,
    integrationsByResource
  );

  // Contextual notification banner: filtered to just this service, via the
  // same matchesService rule the SDK/MCP-page banner mirrors with
  // matchesConfig. Complements (doesn't replace) the general bell in the
  // shared layout -- see plans/plan-service-changelog.md's Phase 4.
  const {
    unresolved: allNotifications,
    serviceRefs: notificationServiceRefs,
    markRead: markNotificationRead,
    dismiss: dismissNotification,
    canUpdate: canUpdateNotifications,
  } = useWorkspaceNotifications(isAuth);
  // Detail banners are action-oriented. Acknowledged items remain available
  // in the bell and full notifications page, but should not interrupt a
  // service view after the user has already marked them read.
  const serviceNotifications = serviceNotificationsFor(
    serviceId,
    allNotifications
  );

  useEffect(() => {
    if (selectedEndpoint?.status === "drifted") {
      const queryStr = `
        query($serviceId: String!) {
          driftSnapshots(serviceId: $serviceId) {
            id
            integration_object_id
            detected_at
            status
            diff {
              field
              old_value
              new_value
              severity
              description
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
      !selectedEndpoint._detailsLoaded &&
      currentVersionEntry?.id
    ) {
      api
        .graphql<{ integration: IntegrationObject }>(
          `
        query($id: String!, $serviceId: String!, $serviceVersionId: String!) {
          integration(id: $id, serviceId: $serviceId, service_version_id: $serviceVersionId) {
            id
            description
            stable_key
            normalized_path
            provider_protocol
            operation_kind
            parameters {
              name
              in
              required
              type
              description
              path_encoding
            }
            request_content
            responses
            security_requirements { schemes { scheme scopes } server_selection }
            pagination { version limits { max_pages max_items max_bytes max_duration_ms } }
            graphql_query
          }
        }
      `,
          {
            id: selectedEndpoint.id,
            serviceId: selectedEndpoint.service_id,
            serviceVersionId: currentVersionEntry?.id,
          }
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
  }, [selectedEndpoint, currentVersionEntry?.id]);

  /** Loads webhook receipts and aggregates for authorized service auditors. */
  async function loadWebhookData() {
    if (!canReadActivity) return;
    if (!isUUID(id)) return;
    if (!isAuth || workspaceServiceActive !== true) return;
    if (!serviceId) return;
    setIsClientFetchingTab(true);
    try {
      // Webhook analytics/events are Engine-owned data and are fetched
      // directly from the Engine (BACKEND_URL) via api.workspace.* -- this
      // deliberately does NOT go through api.graphql (Registry). See
      // internal/engine/api/webhook_analytics_handlers.go.
      const params = webhookRequestParams({
        serviceId,
        page: webhookPage,
        limit: webhookLimit,
        eventName: webhookFilterEvent,
        startDate: webhookStartDate,
        endDate: webhookEndDate,
      });
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

  /** Loads outbound execution receipts for authorized service auditors. */
  async function loadExecutionData() {
    if (!canReadActivity) return;
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
    canReadActivity,
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
    canReadActivity,
  ]);

  /** Loads service consumers used to label authorized activity rows. */
  async function loadAnalyticsData() {
    if (!canReadActivity) return;
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
  }, [id, activeTab, workspaceServiceActive, canReadActivity]);

  // loadData refreshes the same service projection used by the route loader.
  async function loadData() {
    if (!id) return;

    setLoading(true);
    autoExpandedServiceId.current = null;
    // One document prevents client refreshes from drifting from loader fields.
    const queryStr = serviceDetailQuery(provider);
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
  }, [serviceId, serviceResources(res)]); // toggleResource intentionally omitted so it only triggers on service change

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
    await api.integrations.applyImport(plan.plan_id, plan.review_hash);
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

  return {
    toast, loaderData, paramId, provider, id, isAuth, res, serviceId,
    access, canReadActivity,
    serviceVersions, currentVersionEntry,
    workspaceServiceActive, setWorkspaceServiceActive, drift, loading, error,
    driftAction, showShareMenu, setShowShareMenu, shareMenuRef,
    showVisibilityMenu, setShowVisibilityMenu, visibilityMenuRef, savingDrift,
    selectedEndpoint, setSelectedEndpoint, version, resourceVersions,
    setResourceVersions, expandedResources, integrationsByResource,
    loadingResources, hasMoreResources, toggleResource, loadMoreEndpoints,
    activeTab, isClientFetchingTab, handleTabChange, syncWebhookParams,
    webhookEvents, webhookPage, setWebhookPage, webhookTotal, webhookLimit,
    webhookFilterEvent, setWebhookFilterEvent, webhookStartDate,
    setWebhookStartDate, webhookEndDate, setWebhookEndDate, webhookAnalytics,
    executionEvents, executionTotal, executionPage, setExecutionPage,
    executionLimit, executionTransport, setExecutionTransport, executionStatus,
    setExecutionStatus, executionAnalytics, dependentSDKs, dependentMCPs,
    searchQuery, setSearchQuery, isSearching, searchResults, hasMoreSearch,
    handleSearch, loadMoreSearchResults, handleClearSearch,
    notificationServiceRefs, markNotificationRead, dismissNotification,
    canUpdateNotifications,
    serviceNotifications, loadWebhookData, loadData, handleDismiss,
    handleApply, handleToggleDriftWatch, handleTogglePublic,
    handleToggleVersionPublic, handleClearImportWarnings,
  };
}

type DetailModel = ReturnType<typeof useIntegrationDetailModel>;

const DetailContext = createContext<DetailModel | null>(null);

function useDetail(): DetailModel {
  const detail = useContext(DetailContext);
  if (!detail) throw new Error("Integration detail context is unavailable");
  return detail;
}

export default function IntegrationDetail() {
  const detail = useIntegrationDetailModel();
  return (
    <DetailContext.Provider value={detail}>
      <DetailState />
    </DetailContext.Provider>
  );
}

function DetailState() {
  const { loading, error, res } = useDetail();
  if (loading && !res) {
    return (
      <div className="flex min-h-[400px] flex-1 flex-col items-center justify-center text-slate-400">
        <Loader2 className="mb-4 h-8 w-8 animate-spin text-blue-500" />
        <p className="animate-pulse font-medium text-slate-500">Loading service details...</p>
      </div>
    );
  }
  if (error && !res) return <p className="text-sm text-red-600">{error}</p>;
  if (!res) return null;
  return <LoadedDetail />;
}

function LoadedDetail() {
  const detail = useDetail();
  const srv = detail.res!.service;
  const integrations = detail.res!.integrations ?? [];
  const overallStatus = serviceStatus(integrations);
  const importWarnings = srv.import_warnings ?? [];
  const totalEndpoints = endpointTotal(detail.searchResults, srv);

  return (
    <div className="min-w-0 space-y-5 sm:space-y-6">
      <DetailHeader
        srv={srv}
        overallStatus={overallStatus}
        importWarnings={importWarnings}
      />
      <DetailNotices srv={srv} importWarnings={importWarnings} />
      <ServiceConfiguration srv={srv} />
      <WebhookConfiguration srv={srv} />
      <ConnectionProfile srv={srv} />
      <DetailTabs srv={srv} totalEndpoints={totalEndpoints} />
      <ServiceMetadata srv={srv} />
      <SelectedEndpointSidebar srv={srv} />
    </div>
  );
}

function DetailHeader({
  srv,
  overallStatus,
  importWarnings,
}: {
  srv: Service;
  overallStatus: string;
  importWarnings: NonNullable<Service["import_warnings"]>;
}) {
  const { serviceVersions, currentVersionEntry } = useDetail();
  return (
    <div className="flex min-w-0 flex-col gap-4 xl:flex-row xl:items-start xl:justify-between">
      <div className="min-w-0 flex-1">
        <div className="mb-1 flex items-center gap-3">
          <Link to="/integrations" className="text-sm text-slate-400 hover:text-slate-600">
            Back to services
          </Link>
        </div>
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <h1 className="text-xl font-semibold text-slate-900">{formatServiceName(srv.name)}</h1>
          <VersionSelector currentVersionTag={srv.current_service_version} versions={serviceVersions} />
          <VersionStatusBadge version={currentVersionEntry} />
          <DriftStatusBadge status={overallStatus} />
          <ServiceHeaderWarningIcon warningCount={importWarnings.length} />
          <ShareControl serviceName={srv.name} />
        </div>
        <ProviderIdentity srv={srv} />
        <ServerDisplay srv={srv} isAuth={false} />
      </div>
      <HeaderActions srv={srv} />
    </div>
  );
}

function VersionStatusBadge({ version }: { version?: ServiceVersion }) {
  if (version?.status === "deprecated") {
    return <Badge label="DEPRECATED VERSION" color="bg-amber-50 text-amber-700" />;
  }
  if (version?.status === "draft") {
    return <Badge label="DRAFT VERSION" color="bg-slate-100 text-slate-600" />;
  }
  if (version?.status === "public" || version?.is_public) {
    return <Badge label="PUBLIC VERSION" color="bg-slate-100 text-slate-600" />;
  }
  return null;
}

function DriftStatusBadge({ status }: { status: string }) {
  if (status === "active") return null;
  return <Badge label={status} color={serviceStatusColor(status)} />;
}

function ShareControl({ serviceName }: { serviceName: string }) {
  const { toast, showShareMenu, setShowShareMenu, shareMenuRef } = useDetail();
  return (
    <div className="relative" ref={shareMenuRef}>
      <button
        data-track="open_share_menu"
        onClick={() => setShowShareMenu(!showShareMenu)}
        className="rounded-md p-1.5 text-slate-400 transition-colors hover:bg-slate-100 hover:text-slate-600"
        title="Share Integration"
      >
        <Share2 className="h-4 w-4" />
      </button>
      {showShareMenu && (
        <div className="absolute right-0 top-full z-50 mt-1 w-48 overflow-hidden rounded-lg border border-slate-200 bg-white py-1 shadow-lg sm:left-0 sm:right-auto">
          <button
            data-track="share_on_twitter"
            onClick={() => {
              window.open(`https://twitter.com/intent/tweet?url=${encodeURIComponent(window.location.href)}&text=${encodeURIComponent(`Check out the ${serviceName} integration on Fused!`)}`, "_blank");
              setShowShareMenu(false);
            }}
            className="flex w-full items-center gap-2 px-3 py-2 text-left text-sm text-slate-700 hover:bg-slate-50"
          >
            <MessageSquare className="h-4 w-4 text-sky-500" /> Share on Twitter
          </button>
          <button
            data-track="share_on_linkedin"
            onClick={() => {
              window.open(`https://www.linkedin.com/sharing/share-offsite/?url=${encodeURIComponent(window.location.href)}`, "_blank");
              setShowShareMenu(false);
            }}
            className="flex w-full items-center gap-2 px-3 py-2 text-left text-sm text-slate-700 hover:bg-slate-50"
          >
            <Briefcase className="h-4 w-4 text-blue-600" /> Share on LinkedIn
          </button>
          <button
            data-track="copy_integration_link"
            onClick={() => {
              navigator.clipboard.writeText(window.location.href);
              toast.success("Link copied to clipboard");
              setShowShareMenu(false);
            }}
            className="mt-1 flex w-full items-center gap-2 border-t border-slate-100 px-3 py-2 text-left text-sm text-slate-700 hover:bg-slate-50"
          >
            <Copy className="h-4 w-4 text-slate-400" /> Copy Link
          </button>
        </div>
      )}
    </div>
  );
}

function ProviderIdentity({ srv }: { srv: Service }) {
  if (!srv.provider && !srv.canonical_ref) return null;
  return (
    <div className="mt-1 flex min-w-0 flex-wrap items-center gap-1.5 text-sm text-slate-500">
      {srv.provider && <span className="break-words">Provider: {srv.provider.name}</span>}
      {srv.provider && srv.canonical_ref && <span>·</span>}
      {srv.canonical_ref && <CopyableCanonicalRef text={srv.canonical_ref} />}
    </div>
  );
}

function HeaderActions({ srv }: { srv: Service }) {
  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-2 xl:justify-end">
      <VisibilityControl srv={srv} />
      <DriftWatchControl srv={srv} />
      <WorkspaceAddControl srv={srv} />
    </div>
  );
}

function VisibilityControl({ srv }: { srv: Service }) {
  const detail = useDetail();
  const canManage = canManageService(detail.isAuth, srv.is_owner, detail.paramId, detail.provider);
  if (!canManage) {
    return srv.is_public
      ? <Badge label="PUBLIC SERVICE" color="bg-blue-50 text-blue-700" />
      : <Badge label="PRIVATE SERVICE" color="bg-slate-100 text-slate-600" />;
  }
  return (
    <div className="relative" ref={detail.visibilityMenuRef}>
      <button
        type="button"
        aria-expanded={detail.showVisibilityMenu}
        aria-haspopup="dialog"
        onClick={() => detail.setShowVisibilityMenu((open) => !open)}
        className="inline-flex h-9 items-center gap-2 rounded-md border border-slate-200 bg-white px-3 text-sm font-medium text-slate-700 shadow-sm hover:bg-slate-50"
      >
        {srv.is_public ? <Globe2 className="h-4 w-4 text-blue-600" /> : <Lock className="h-4 w-4 text-slate-500" />}
        {srv.is_public ? "Public service" : "Private service"}
        <ChevronDown className="h-3.5 w-3.5 text-slate-400" />
      </button>
      {detail.showVisibilityMenu && <VisibilityMenu srv={srv} />}
    </div>
  );
}

function VisibilitySwitch({
  label,
  checked,
  onClick,
}: {
  label: string;
  checked: boolean;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-label={label}
      aria-checked={checked}
      onClick={onClick}
      className={`relative mt-0.5 inline-flex h-5 w-9 shrink-0 rounded-full ${checked ? "bg-slate-900" : "bg-slate-300"}`}
    >
      <span className={`absolute top-0.5 h-4 w-4 rounded-full bg-white shadow-sm transition-transform ${checked ? "translate-x-4" : "translate-x-0.5"}`} />
    </button>
  );
}

function VisibilityMenu({ srv }: { srv: Service }) {
  const { currentVersionEntry, handleTogglePublic, handleToggleVersionPublic } = useDetail();
  return (
    <div role="dialog" aria-label="Service visibility" className="absolute left-0 top-full z-50 mt-2 w-[min(22rem,calc(100vw-2rem))] rounded-md border border-slate-200 bg-white p-4 shadow-xl xl:left-auto xl:right-0">
      <h2 className="text-sm font-semibold text-slate-900">Visibility</h2>
      <div className="mt-3 divide-y divide-slate-100">
        <div className="flex items-start justify-between gap-4 pb-3">
          <div>
            <p className="text-sm font-medium text-slate-800">Service</p>
            <p className="mt-0.5 text-xs leading-5 text-slate-500">{srv.is_public ? "Discoverable outside your workspace." : "Only your workspace can discover it."}</p>
          </div>
          <VisibilitySwitch label="Make service public" checked={Boolean(srv.is_public)} onClick={handleTogglePublic} />
        </div>
        {currentVersionEntry && (
          <div className="flex items-start justify-between gap-4 pt-3">
            <div>
              <p className="text-sm font-medium text-slate-800">Consumer access</p>
              <p className="mt-0.5 text-xs leading-5 text-slate-500">{currentVersionEntry.is_public ? "Available when the service is public." : "This version is held back from consumers."}</p>
            </div>
            <VisibilitySwitch label="Allow consumer access to this version" checked={Boolean(currentVersionEntry.is_public)} onClick={handleToggleVersionPublic} />
          </div>
        )}
      </div>
    </div>
  );
}

function DriftWatchControl({ srv }: { srv: Service }) {
  const detail = useDetail();
  const canManage = canManageService(detail.isAuth, srv.is_owner, detail.paramId, detail.provider);
  if (!canManage) return null;
  const uploaded = srv.source_url === "uploaded://spec";
  return (
    <div className="group relative flex flex-col items-start">
      <label className={`flex items-center gap-2 text-sm ${uploaded ? "cursor-not-allowed text-slate-400" : "cursor-pointer text-slate-700"}`}>
        <input type="checkbox" checked={Boolean(srv.watch_for_drift)} onChange={detail.handleToggleDriftWatch} disabled={detail.savingDrift || uploaded} className="h-4 w-4 rounded border-slate-300 text-blue-600" />
        Watch for Drift
      </label>
      {uploaded && <div className="absolute top-full z-10 mt-1 hidden w-48 rounded bg-slate-800 p-2 text-xs text-white group-hover:block">We can't monitor this for changes. To enable drift detection, provide a URL.</div>}
    </div>
  );
}

function WorkspaceAddControl({ srv }: { srv: Service }) {
  const { isAuth, workspaceServiceActive, setWorkspaceServiceActive } = useDetail();
  if (!isAuth || workspaceServiceActive !== false) return null;
  return <AddToWorkspaceButton serviceId={srv.id} serviceName={srv.name} versionTag={srv.current_service_version} onAdded={() => setWorkspaceServiceActive(true)} />;
}

function DetailNotices({ srv, importWarnings }: { srv: Service; importWarnings: NonNullable<Service["import_warnings"]> }) {
  const detail = useDetail();
  const canManage = canManageService(detail.isAuth, srv.is_owner, detail.paramId, detail.provider);
  return (
    <>
      <ImportMethodBadge method={srv.import_method} canManage={canManage} />
      {srv.description && <p className="text-sm text-slate-600">{srv.description}</p>}
      <ImportWarningPanel warnings={importWarnings} onClear={detail.handleClearImportWarnings} />
      <ServiceNotificationBanner />
    </>
  );
}

function ImportMethodBadge({ method, canManage }: { method?: string; canManage: boolean }) {
  if (!method || !canManage) return <div className="mb-4" />;
  return (
    <div className="mb-4 flex gap-4 text-xs">
      <span className="rounded-md border border-slate-200 bg-slate-100 px-2 py-1 font-medium uppercase tracking-wider text-slate-600">
        {method === "openapi" ? "Imported via OpenAPI" : "Imported via Docs"}
      </span>
    </div>
  );
}

// ServiceNotificationBanner resets contextual disclosure state when navigation selects another service.
function ServiceNotificationBanner() {
  const detail = useDetail();
  // Unauthenticated and empty contexts do not reserve space for notifications.
  if (!detail.isAuth || !detail.res || detail.serviceNotifications.length === 0) return null;
  return <NotificationBanner key={detail.res.service.id} items={detail.serviceNotifications} serviceRefs={detail.notificationServiceRefs} onMarkRead={detail.markNotificationRead} onDismiss={detail.dismissNotification} canUpdate={detail.canUpdateNotifications} />;
}

const configCardClass = "group rounded-lg border border-slate-100 bg-slate-50 p-3";

function ServiceConfiguration({ srv }: { srv: Service }) {
  return (
    <section className="rounded-lg border border-slate-200 bg-white p-4 sm:p-5">
      <h2 className="mb-4 text-sm font-semibold text-slate-900">Service configuration</h2>
      <dl className="grid gap-4 text-sm sm:grid-cols-2 lg:grid-cols-4">
        <ServerConfigCard srv={srv} />
        <AuthConfigCard srv={srv} />
        <RateLimitConfigCard srv={srv} />
        <RetryConfigCard srv={srv} />
      </dl>
    </section>
  );
}

function ServerConfigCard({ srv }: { srv: Service }) {
  const multiple = Boolean(srv.servers && srv.servers.length > 1);
  return (
    <details className={`${configCardClass} ${multiple ? "cursor-pointer hover:bg-slate-100" : ""}`}>
      <summary className="flex list-none items-start justify-between outline-none">
        <div><dt className="text-xs text-slate-500">Base URL</dt><dd className="mt-1 break-all font-mono text-xs text-slate-800">{srv.base_url || srv.servers?.[0]?.url || "Not declared"}</dd></div>
        {multiple && <ChevronDown className="mt-1 h-4 w-4 shrink-0 text-slate-400 transition-transform group-open:rotate-180" />}
      </summary>
      {multiple && (
        <div className="mt-3 space-y-2 border-t border-slate-200 pt-3 text-xs text-slate-700">
          {srv.servers?.map((server, index) => (
            <div key={index}>
              <div className="break-all font-mono text-slate-800">{server.url}</div>
              <div className="mt-0.5 flex gap-2 text-slate-500">
                {server.environment && <span>{server.environment}</span>}
                {server.is_default && <span className="rounded bg-blue-100 px-1 text-[10px] font-bold uppercase text-blue-700">Default</span>}
              </div>
            </div>
          ))}
        </div>
      )}
    </details>
  );
}

// AuthConfigCard makes service-defined scheme names the primary discovery surface before expanding method details.
function AuthConfigCard({ srv }: { srv: Service }) {
  const authConfigs = srv.auth_configs ?? [];
  const hasAuth = authConfigs.length > 0;
  // Empty contracts have no auth selector to reveal or copy.
  return (
    <details className={`${configCardClass} ${hasAuth ? "cursor-pointer hover:bg-slate-100" : ""}`}>
      <summary className="flex list-none items-start justify-between outline-none">
        <div className="min-w-0">
          <dt className="text-xs text-slate-500">Authentication schemes · auth.name</dt>
          <dd className="mt-1 space-y-1 text-slate-800">
            {/* Names are exact and case-sensitive; missing metadata must never be inferred from type. */}
            {hasAuth ? authConfigs.map((auth, index) => (
              <div key={index} className="break-all"><code className="font-mono text-xs">{auth.name || "Name not provided"}</code> <span className="text-xs text-slate-500">({auth.type})</span></div>
            )) : "None"}
          </dd>
        </div>
        {hasAuth && <ChevronDown className="mt-1 h-4 w-4 shrink-0 text-slate-400 transition-transform group-open:rotate-180" />}
      </summary>
      {hasAuth && <AuthConfigDetails authConfigs={authConfigs} />}
    </details>
  );
}

// AuthConfigDetails explains which names the service defines without conflating them with stored credentials or wire keys.
function AuthConfigDetails({ authConfigs }: { authConfigs: AuthConfig[] }) {
  return (
    <div className="mt-3 space-y-3 border-t border-slate-200 pt-3 text-xs text-slate-700">
      <p className="leading-relaxed text-slate-500">This service defines the scheme names below. Use the exact, case-sensitive name as <code>auth.name</code> in SDK/MCP config. Credentials stay in the bucket; <code>end_user_ref</code> identifies a connected user. Operations within this service may require different auth methods.</p>
      {/* Separate schemes visually while keeping optional provider fields truthful to the contract. */}
      {authConfigs.map((auth, index) => (
        <div key={index} className={index > 0 ? "border-t border-slate-200 pt-3" : ""}>
          <AuthNameField name={auth.name} />
          {auth.type && <ConfigRow label="Provider type" value={auth.type} />}
          {auth.scheme && <ConfigRow label="HTTP scheme" value={auth.scheme} />}
          {auth.location && <ConfigRow label="Location" value={auth.location} />}
          {auth.key_name && <ConfigRow label="Request key" value={auth.key_name} mono />}
          <OAuth2FlowDetails auth={auth} />
        </div>
      ))}
    </div>
  );
}

function ConfigRow({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
  return <div className="mt-1 flex justify-between"><span className="text-slate-500">{label}</span><span className={mono ? "font-mono text-slate-900" : "font-medium text-slate-900"}>{value}</span></div>;
}

// RateLimitConfigCard summarizes the v3 policy set and reveals exact policy details on demand.
function RateLimitConfigCard({ srv }: { srv: Service }) {
  const rateLimit = srv.rate_limit;
  return (
    <details className={`${configCardClass} ${rateLimit ? "cursor-pointer hover:bg-slate-100" : ""}`}>
      <summary className="flex list-none items-start justify-between outline-none">
        <div><dt className="text-xs text-slate-500">Rate limit</dt><dd className="mt-1 text-slate-800">{rateLimitSummary(rateLimit)}</dd></div>
        {rateLimit && <ChevronDown className="mt-1 h-4 w-4 shrink-0 text-slate-400 transition-transform group-open:rotate-180" />}
      </summary>
      {rateLimit && (
        <div className="mt-3 space-y-2 border-t border-slate-200 pt-3 text-xs text-slate-700">
          {rateLimit.policies.map((policy) => <RateLimitPolicyCard key={policy.name} policy={policy} />)}
        </div>
      )}
    </details>
  );
}

// rateLimitIdentityLabel renders the ordered dimensions that form a provider quota bucket.
function rateLimitIdentityLabel(policy: NonNullable<Service["rate_limit"]>["policies"][number]): string {
  return policy.identity.inputs
    .map((input) => input.name || input.binding || input.kind)
    .map((value) => value.replace(/[_-]+/g, " "))
    .join(" + ");
}

// RateLimitPolicyCard renders every supported v3 rate-limit algorithm without legacy aliases.
function RateLimitPolicyCard({ policy }: { policy: NonNullable<Service["rate_limit"]>["policies"][number] }) {
  // Mode uses a restrained semantic tint so the quota remains the visual focus.
  const modeClass = policy.mode === "observe"
    ? "bg-amber-50 text-amber-700 ring-amber-200"
    : "bg-emerald-50 text-emerald-700 ring-emerald-200";
  return (
    <div className="overflow-hidden rounded-md border border-slate-200 bg-white shadow-sm">
      <div className="p-2.5">
        <div className="flex min-w-0 items-center justify-between gap-2">
          <span className="truncate font-semibold text-slate-900" title={policy.name}>
            {rateLimitPolicyName(policy.name)}
          </span>
          <span className={`shrink-0 rounded-full px-1.5 py-0.5 text-[10px] font-semibold capitalize ring-1 ring-inset ${modeClass}`}>
            {policy.mode}
          </span>
        </div>
        <div className="mt-2 text-sm font-semibold tracking-tight text-slate-900">
          {rateLimitPolicyQuotaLabel(policy)}
        </div>
      </div>
      <div className="flex flex-wrap gap-1.5 border-t border-slate-100 bg-slate-50/80 px-2.5 py-2 text-[10px] font-medium text-slate-600">
        <span className="rounded-full bg-white px-1.5 py-0.5 ring-1 ring-inset ring-slate-200">
          {rateLimitAlgorithmLabel(policy.algorithm)}
        </span>
        <span className="rounded-full bg-white px-1.5 py-0.5 ring-1 ring-inset ring-slate-200">
          {rateLimitIdentityLabel(policy) || "Unscoped"}
        </span>
        <span className="rounded-full bg-white px-1.5 py-0.5 ring-1 ring-inset ring-slate-200">
          Cost {policy.cost.default}
        </span>
      </div>
    </div>
  );
}

// retryPredicateSummary condenses a rule's independently optional predicates for the card header.
function retryPredicateSummary(rule: NonNullable<Service["retry_config"]>["rules"][number]): string {
  const predicates = rule.predicates;
  const labels = [
    ...predicates.methods,
    ...predicates.operation_kinds,
    ...predicates.statuses.map((status) => `${status.min}-${status.max}`),
    ...predicates.errors,
  ];
  return labels.join(", ") || "Any admitted request";
}

// RetryRuleCard displays the bounded action paired with one exact v3 predicate set.
function RetryRuleCard({ rule }: { rule: NonNullable<Service["retry_config"]>["rules"][number] }) {
  return (
    <div className="rounded border border-slate-200 bg-white p-2">
      <div className="font-medium text-slate-900">{retryPredicateSummary(rule)}</div>
      <ConfigRow label="Max attempts" value={String(rule.action.max_attempts)} />
      <ConfigRow label="Max elapsed" value={`${rule.action.max_elapsed_ms}ms`} />
      <ConfigRow label="Backoff" value={rule.action.backoff.strategy} />
    </div>
  );
}

// RetryConfigCard summarizes v3 rules instead of reading the removed legacy strategy fields.
function RetryConfigCard({ srv }: { srv: Service }) {
  const retry = srv.retry_config;
  return (
    <details className={`${configCardClass} ${retry ? "cursor-pointer hover:bg-slate-100" : ""}`}>
      <summary className="flex list-none items-start justify-between outline-none">
        <div><dt className="text-xs text-slate-500">Retry</dt><dd className="mt-1 text-slate-800">{retrySummary(retry)}</dd></div>
        {retry && <ChevronDown className="mt-1 h-4 w-4 shrink-0 text-slate-400 transition-transform group-open:rotate-180" />}
      </summary>
      {retry && (
        <div className="mt-3 space-y-2 border-t border-slate-200 pt-3 text-xs text-slate-700">
          {retry.rules.map((rule, index) => <RetryRuleCard key={index} rule={rule} />)}
        </div>
      )}
    </details>
  );
}

function WebhookConfiguration({ srv }: { srv: Service }) {
  const webhook = srv.incoming_webhook_config;
  if (!webhook) return null;
  return (
    <section className="mb-6 mt-6 rounded-lg border border-slate-200 bg-white p-4 sm:p-5">
      <h2 className="mb-4 text-sm font-semibold text-slate-900">Webhook configuration</h2>
      <dl className="grid gap-4 text-sm sm:grid-cols-2 lg:grid-cols-4">
        <details className={`${configCardClass} cursor-pointer hover:bg-slate-100`}>
          <summary className="flex list-none items-start justify-between outline-none">
            <div><dt className="text-xs text-slate-500">Webhook verification</dt><dd className="mt-1 text-slate-800">{webhook.auth_type || "None"}</dd></div>
            <ChevronDown className="mt-1 h-4 w-4 shrink-0 text-slate-400 transition-transform group-open:rotate-180" />
          </summary>
          <div className="mt-3 space-y-1 border-t border-slate-200 pt-3 text-xs text-slate-700">
            {webhook.auth_type && <ConfigRow label="Auth type" value={webhook.auth_type} />}
            {webhook.auth_location && <ConfigRow label="Location" value={webhook.auth_location} />}
            {webhook.auth_key_name && <ConfigRow label="Key name" value={webhook.auth_key_name} mono />}
            {webhook.signature_header && <ConfigRow label="Sig. header" value={webhook.signature_header} mono />}
          </div>
        </details>
      </dl>
    </section>
  );
}

/** Resolves read and manage grants for the selected service profile. */
function ConnectionProfile({ srv }: { srv: Service }) {
  const { serviceId, serviceVersions, version } = useDetail();
  if (!serviceId) return null;
  const serviceVersion = selectedVersionName(version, srv.current_service_version);
  const versionID = serviceVersions.find((item) => item.name === serviceVersion)?.id;
  const detail = useDetail();
  const canRead =
    hasResourcePermission(detail.access, "service.read", "SERVICE", serviceId) &&
    hasWorkspacePermission(detail.access, "credentials.metadata.read");
  const canManage =
    canRead &&
    hasResourcePermission(detail.access, "service.manage", "SERVICE", serviceId);
  return <WorkspaceConnectionProfileSection serviceId={serviceId} serviceVersionId={versionID} serviceVersion={serviceVersion} authConfigs={srv.auth_configs ?? []} canRead={canRead} canManage={canManage} />;
}

function tabClass(active: boolean) {
  return `shrink-0 rounded-md px-3 py-1.5 text-xs font-medium transition-all sm:px-4 sm:text-sm ${active ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700"}`;
}

function DetailTabs({ srv, totalEndpoints }: { srv: Service; totalEndpoints: number }) {
  return (
    <>
      <TabNavigation srv={srv} totalEndpoints={totalEndpoints} />
      <TabLoadingIndicator />
      <div className="relative"><ActiveTabContent srv={srv} /></div>
    </>
  );
}

function TabNavigation({ srv, totalEndpoints }: { srv: Service; totalEndpoints: number }) {
  const { activeTab, handleTabChange, isAuth, workspaceServiceActive, canReadActivity } = useDetail();
  return (
    <div className="mb-6 flex max-w-full overflow-x-auto whitespace-nowrap rounded-lg bg-slate-100/80 p-1 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
      <button data-track="view_endpoints_tab" onClick={() => handleTabChange("endpoints")} className={tabClass(activeTab === "endpoints")}>Operations ({totalEndpoints})</button>
      <button data-track="view_webhooks_tab" onClick={() => handleTabChange("webhooks")} className={tabClass(activeTab === "webhooks")}>Webhooks ({srv.webhook_count ?? 0})</button>
      {isAuth && workspaceServiceActive === true && canReadActivity && (
        <button data-track="view_activity_tab" onClick={() => handleTabChange("analytics")} className={tabClass(activeTab === "analytics")}>Activity</button>
      )}
    </div>
  );
}

function TabLoadingIndicator() {
  const { isClientFetchingTab } = useDetail();
  if (!isClientFetchingTab) return null;
  return (
    <div className="absolute left-1/2 top-1/2 z-10 flex -translate-x-1/2 -translate-y-1/2 flex-col items-center justify-center rounded-lg border border-slate-100 bg-white/80 p-4 shadow-sm backdrop-blur-sm">
      <Loader2 className="mb-2 h-6 w-6 animate-spin text-blue-500" />
      <p className="text-xs font-medium text-slate-500">Updating...</p>
    </div>
  );
}

// Webhook authoring reuses the selected-version refresh without changing ordinary tab navigation.
function ActiveTabContent({ srv }: { srv: Service }) {
  const { activeTab, setSelectedEndpoint, canReadActivity, version, loadData } = useDetail();
  // Operation browsing remains the default read-only view.
  if (activeTab === "endpoints") return <EndpointTabContent />;
  // Only the webhook tab exposes its click-gated owner editor; it never mounts from settings expansion.
  if (activeTab === "webhooks") return <WebhooksTab srv={srv} version={selectedVersionName(version, srv.current_service_version)} onSaved={loadData} setSelectedEndpoint={setSelectedEndpoint} />;
  // A direct analytics URL must fall back to ordinary service content when
  // the Activity capability is absent; its data effects are also gated.
  return canReadActivity ? <ActivityTabContent /> : <EndpointTabContent />;
}

function EndpointTabContent() {
  const detail = useDetail();
  return (
    <EndpointsTab
      res={detail.res!}
      searchQuery={detail.searchQuery}
      setSearchQuery={detail.setSearchQuery}
      searchResults={detail.searchResults}
      isSearching={detail.isSearching}
      handleSearch={detail.handleSearch}
      handleClearSearch={detail.handleClearSearch}
      resourceVersions={detail.resourceVersions}
      setResourceVersions={detail.setResourceVersions}
      expandedResources={detail.expandedResources}
      integrationsByResource={detail.integrationsByResource}
      loadingResources={detail.loadingResources}
      toggleResource={detail.toggleResource}
      hasMoreResources={detail.hasMoreResources}
      loadMoreEndpoints={detail.loadMoreEndpoints}
      hasMoreSearch={detail.hasMoreSearch}
      loadMoreSearchResults={detail.loadMoreSearchResults}
      selectedEndpoint={detail.selectedEndpoint}
      setSelectedEndpoint={detail.setSelectedEndpoint}
    />
  );
}

function nextPageValue(page: number | ((value: number) => number), current: number) {
  return typeof page === "function" ? page(current) : page;
}

/** Renders service activity only after its audit permission is established. */
function ActivityTabContent() {
  const detail = useDetail();
  return (
    <ActivityTab
      res={detail.res!}
      executionEvents={detail.executionEvents}
      executionTotal={detail.executionTotal}
      executionPage={detail.executionPage}
      setExecutionPage={detail.setExecutionPage}
      executionLimit={detail.executionLimit}
      executionTransport={detail.executionTransport}
      setExecutionTransport={(transport) => {
        detail.setExecutionPage(1);
        detail.setExecutionTransport(transport);
      }}
      executionStatus={detail.executionStatus}
      setExecutionStatus={(status) => {
        detail.setExecutionPage(1);
        detail.setExecutionStatus(status);
      }}
      executionAnalytics={detail.executionAnalytics}
      webhookEvents={detail.webhookEvents}
      webhookTotal={detail.webhookTotal}
      webhookPage={detail.webhookPage}
      setWebhookPage={(page) => {
        const nextPage = nextPageValue(page, detail.webhookPage);
        detail.setWebhookPage(nextPage);
        detail.syncWebhookParams({ page: nextPage });
      }}
      webhookLimit={detail.webhookLimit}
      webhookFilterEvent={detail.webhookFilterEvent}
      setWebhookFilterEvent={detail.setWebhookFilterEvent}
      webhookStartDate={detail.webhookStartDate}
      setWebhookStartDate={(date) => {
        detail.setWebhookStartDate(date);
        detail.syncWebhookParams({ startDate: date });
      }}
      webhookEndDate={detail.webhookEndDate}
      setWebhookEndDate={(date) => {
        detail.setWebhookEndDate(date);
        detail.syncWebhookParams({ endDate: date });
      }}
      webhookAnalytics={detail.webhookAnalytics}
      loadWebhookData={detail.loadWebhookData}
      dependentSDKs={detail.dependentSDKs}
      dependentMCPs={detail.dependentMCPs}
    />
  );
}

function ServiceMetadata({ srv }: { srv: Service }) {
  return (
    <div className="mt-8 flex flex-col gap-2 border-t border-slate-100 pt-6 text-xs text-slate-400 sm:flex-row sm:flex-wrap sm:items-center sm:gap-6">
      <span suppressHydrationWarning>Created {new Date(srv.created_at).toLocaleString()}</span>
      <span suppressHydrationWarning>Updated {new Date(srv.updated_at).toLocaleString()}</span>
      <span className="break-all font-mono">Source: {srv.source_url || "N/A"}</span>
    </div>
  );
}

// SelectedEndpointSidebar pins schema expansion to the same visible version as the selected operation.
function SelectedEndpointSidebar({ srv }: { srv: Service }) {
  const detail = useDetail();
  // No operation means no schema identity to resolve.
  if (!detail.selectedEndpoint) return null;
  // An unresolved version never silently borrows the current version's dictionary.
  const componentScope = detail.currentVersionEntry?.id || "unresolved";
  const allowRemoteRefs = Boolean(detail.currentVersionEntry?.id);
  return (
    <EndpointDetailsSidebar
      selectedEndpoint={detail.selectedEndpoint}
      setSelectedEndpoint={detail.setSelectedEndpoint}
      srv={srv}
      drift={detail.drift}
      driftAction={detail.driftAction}
      handleDismiss={detail.handleDismiss}
      handleApply={detail.handleApply}
      componentScope={componentScope}
      allowRemoteRefs={allowRemoteRefs}
    />
  );
}

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
