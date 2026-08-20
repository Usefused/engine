import { useState, useEffect, type FormEvent } from "react";
import { useSearchParams, useLoaderData, type MetaFunction } from "@remix-run/react";
import { redirect } from "@remix-run/react";

// meta preserves shared metadata while naming the app builder route.
export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m) => m.id === "root").flatMap((m) => m.meta ?? []);
  return [
    ...parentMeta.filter((m) => !('title' in m)),
    { title: "Create app - Fused" },
  ];
};
import { api, handleCredentialedResponse, type Service, type IntegrationObject, type WebhookObject, type ServiceVersion, BASE } from "~/lib/api";
import {
  listAppBuildSelectors,
  listAppOwningTeams,
  planAndApplyApp,
} from "~/lib/app-builder";
import type { AppBuildSelector, AppOwningTeam } from "~/lib/app-builder-contract";
import { openAuthenticatedTab } from "~/lib/session";
import { CREATE_CREDENTIAL_PATH } from "~/lib/credential-navigation";
import { useToast } from "~/components/Toast";
import { fetchEventSource } from "@microsoft/fetch-event-source";
import { CheckSquare, Square, ChevronDown, ChevronRight, Search, Loader2, X, ChevronLeft } from "lucide-react";
import EndpointSelectionList from "~/components/EndpointSelectionList";
import WebhookSelectionList from "~/components/WebhookSelectionList";
import {
  ConsumerGenerationPanel,
  type ConsumerGenerationPanelProps,
} from "~/components/consumer/ConsumerGenerationPanel";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasAnyPermission } from "~/lib/current-actor-access";

// clientLoader requires an authenticated Engine session before building an app.
export const clientLoader = async ({ request }: { request: Request }) => {
	const session = await api.auth.session().catch(() => ({ authenticated: false }));
	const url = new URL(request.url);
	if (!session.authenticated) {
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

type AppSelection = {
  service_id: string;
  service_name?: string;
  service_slug?: string;
  select_all: boolean;
  endpoint_ids: string[];
  webhook_ids: string[];
  service_version_id?: string;
};

type GenerationMode = "sdk" | "mcp";

type SelectionMaps = {
  selections: Record<string, Set<string>>;
  selectAllServices: Set<string>;
  webhookSelections: Record<string, Set<string>>;
  versionSelections: Record<string, string>;
};

type GenerationInput = {
  selections: AppSelection[];
  data: ServiceData[];
  sdkName: string;
  generationMode: GenerationMode;
  availableBuckets: AppBuildSelector[];
  bucketId: string;
  ownerTeamId: string;
  webhookAttachment: string;
};

type GenerationValidation =
  | { ok: false; severity: "error" | "warning"; message: string }
  | { ok: true; bucket: AppBuildSelector; hasWebhookSelections: boolean };

type GenerationStreamEvent =
  | { type: "thinking"; message?: string }
  | { type: "complete"; integration_id: string }
  | { type: "error"; message?: string };

type SDKStreamContext = {
  controller: AbortController;
  appId: string;
  jobId: string;
  sdkName: string;
  appVersion: string;
  executionToken: string;
  setStatus: (status: string) => void;
  setDeployment: (deployment: { id: string; name: string; version: string; token: string }) => void;
};

// hasServiceSelection keeps every selection gate aligned with the generated payload.
function hasServiceSelection(serviceId: string, maps: SelectionMaps): boolean {
  return maps.selectAllServices.has(serviceId) ||
    (maps.selections[serviceId]?.size || 0) > 0 ||
    (maps.webhookSelections[serviceId]?.size || 0) > 0;
}

// buildAppSelection creates one exact-version service selection for plan/apply.
function buildAppSelection(
  serviceId: string,
  data: ServiceData[],
  maps: SelectionMaps
): AppSelection {
  const serviceData = data.find((candidate) => candidate.service.id === serviceId);
  const serviceVersionId = maps.versionSelections[serviceId] || serviceData?.serviceVersions[0]?.id;
  const endpointIds = maps.selectAllServices.has(serviceId)
    ? []
    : Array.from(maps.selections[serviceId] || new Set<string>());
  return {
    service_id: serviceId,
    service_name: serviceData?.service.name,
    service_slug: serviceData?.service.slug,
    select_all: maps.selectAllServices.has(serviceId),
    endpoint_ids: endpointIds,
    webhook_ids: Array.from(maps.webhookSelections[serviceId] || new Set<string>()),
    service_version_id: serviceVersionId,
  };
}

// buildAppSelections includes every service with at least one selected capability.
function buildAppSelections(data: ServiceData[], maps: SelectionMaps): AppSelection[] {
  const serviceIds = new Set([
    ...Object.keys(maps.selections),
    ...maps.selectAllServices,
    ...Object.keys(maps.webhookSelections),
  ]);
  return Array.from(serviceIds)
    .filter((serviceId) => hasServiceSelection(serviceId, maps))
    .map((serviceId) => buildAppSelection(serviceId, data, maps));
}

// webhookConfigurationError explains the first selected webhook contract that cannot execute.
function webhookConfigurationError(selections: AppSelection[], data: ServiceData[]): string {
  for (const selection of selections) {
    if (selection.webhook_ids.length === 0) continue;
    const serviceData = data.find((candidate) => candidate.service.id === selection.service_id);
    if (serviceData && (!serviceData.service.event_extraction_path || !serviceData.service.incoming_webhook_config)) {
      return `Service '${serviceData.service.name}' has webhooks selected but is missing proper webhook configuration. Please configure Webhook Setup in the service settings and ensure an event extraction path is set before generating an SDK.`;
    }
  }
  return "";
}

// baseURLConfigurationError explains the first selected operation contract that cannot execute.
function baseURLConfigurationError(selections: AppSelection[], data: ServiceData[]): string {
  for (const selection of selections) {
    if (selection.endpoint_ids.length === 0 && !selection.select_all) continue;
    const serviceData = data.find((candidate) => candidate.service.id === selection.service_id);
    if (serviceData && !serviceData.service.base_url) {
      return `Service '${serviceData.service.name}' is missing an API URL. Please configure the API Base URL in the service settings before generating an SDK.`;
    }
  }
  return "";
}

// generationArtifactName returns the user-facing artifact name for validation copy.
function generationArtifactName(mode: GenerationMode): string {
  return mode === "mcp" ? "MCP server" : "App";
}

// validateGenerationInput resolves all local prerequisites before starting plan/apply.
function validateGenerationInput(input: GenerationInput): GenerationValidation {
  if (input.selections.length === 0) {
    return { ok: false, severity: "warning", message: "Please select at least one endpoint or webhook to generate an SDK." };
  }
  if (input.selections.some((selection) => !selection.service_version_id)) {
    return { ok: false, severity: "error", message: "Each selected service needs a service version before generating an SDK." };
  }
  const serviceError = webhookConfigurationError(input.selections, input.data) ||
    baseURLConfigurationError(input.selections, input.data);
  if (serviceError) return { ok: false, severity: "error", message: serviceError };
  if (!input.sdkName.trim()) {
    return { ok: false, severity: "warning", message: `${generationArtifactName(input.generationMode)} name is required.` };
  }
  const bucket = input.availableBuckets.find((candidate) => candidate.resource_id === input.bucketId);
  if (!bucket) {
    const message = input.ownerTeamId
      ? "Choose a credential set available to both you and the owning team."
      : "Choose a credential set you can use.";
    return { ok: false, severity: "warning", message };
  }
  const hasWebhookSelections = input.selections.some((selection) => selection.webhook_ids.length > 0);
  if (hasWebhookSelections && !input.webhookAttachment.trim()) {
    return { ok: false, severity: "warning", message: "Enter the webhook bundle that supplies the selected events." };
  }
  return { ok: true, bucket, hasWebhookSelections };
}

// reportGenerationValidation shows a validation result at its intended severity.
function reportGenerationValidation(
  toast: ReturnType<typeof useToast>,
  validation: Extract<GenerationValidation, { ok: false }>
) {
  if (validation.severity === "error") toast.error(validation.message);
  else toast.warning(validation.message);
}

// confirmDuplicateGeneration protects an existing immutable SDK version from accidental replacement.
async function confirmDuplicateGeneration(input: {
  toast: ReturnType<typeof useToast>;
  mode: GenerationMode;
  duplicate: boolean;
  name: string;
  version: string;
}): Promise<boolean> {
  if (input.mode !== "sdk" || !input.duplicate) return true;
  return input.toast.confirm(
    `An SDK with name "${input.name.trim()}" and version "${input.version.trim()}" already exists. Generating it again will overwrite the existing package file. Are you sure you want to continue?`
  );
}

// generationFailureMessage keeps mode-specific failure copy consistent.
function generationFailureMessage(mode: GenerationMode, cause: unknown): string {
  const prefix = mode === "mcp" ? "Failed to deploy MCP server" : "Failed to generate SDK";
  const detail = cause instanceof Error ? cause.message : "Unknown error";
  return `${prefix}: ${detail}`;
}

// buildGenerationConfig serializes the validated builder form into the shared app contract.
function buildGenerationConfig(input: {
  mode: GenerationMode;
  name: string;
  version: string;
  bucket: string;
  selections: AppSelection[];
  data: ServiceData[];
  language: "typescript" | "python";
  webhookAttachment: string;
  hasWebhookSelections: boolean;
}): Record<string, unknown> {
  const config: Record<string, unknown> = {
    apiVersion: "fused/v1",
    kind: input.mode,
    name: input.name.trim(),
    version: input.version.trim(),
    bucket: input.bucket,
    services: appServicesConfig(input.selections, input.data),
  };
  if (input.mode === "sdk") config.language = input.language;
  if (input.hasWebhookSelections) config.webhook_attachment = input.webhookAttachment.trim();
  return config;
}

// processGenerationStreamEvent applies progress or completion from one SDK stream event.
function processGenerationStreamEvent(
  context: SDKStreamContext,
  event: GenerationStreamEvent,
  resolve: () => void,
  reject: (reason?: unknown) => void
) {
  if (event.type === "thinking") {
    context.setStatus(event.message || "Generating...");
    return;
  }
  if (event.type === "complete") {
    context.controller.abort();
    context.setStatus("Downloading...");
    api.sdks.download(event.integration_id, context.sdkName, context.appVersion).then(() => {
      context.setDeployment({
        id: context.appId,
        name: context.sdkName.trim(),
        version: context.appVersion.trim(),
        token: context.executionToken,
      });
      context.setStatus("App ready");
      resolve();
    }).catch(reject);
    return;
  }
  if (event.type === "error") {
    context.controller.abort();
    reject(new Error(event.message || "Unknown generation error"));
  }
}

// waitForSDKGeneration follows the Engine job stream through download completion.
async function waitForSDKGeneration(context: SDKStreamContext): Promise<void> {
  await new Promise<void>((resolve, reject) => {
    fetchEventSource(`${BASE}/sdks/job/${context.jobId}/stream`, {
      credentials: "include",
      signal: context.controller.signal,
      async onopen(response) {
        handleCredentialedResponse(response);
        if (response.status === 401) context.controller.abort();
        if (!response.ok) throw new Error(`Failed to connect to generation stream: ${response.status}`);
      },
      onmessage(message) {
        try {
          processGenerationStreamEvent(context, JSON.parse(message.data), resolve, reject);
        } catch {
          console.error("Failed to parse SSE event", message.data);
        }
      },
      onerror(cause) {
        if (context.controller.signal.aborted) throw cause;
        context.controller.abort();
        reject(new Error("Connection to server lost during generation"));
        // Throwing prevents the SSE client from retrying a failed generation.
        throw cause;
      },
      onclose() {
        // A close before completion is an incomplete generation, not success.
        throw new Error("Server closed connection gracefully");
      },
    });
  });
}

const BUILDER_RESOURCE_GQL = `
  query($resourceId: String!, $serviceId: String!, $serviceVersionId: String!, $limit: Int, $offset: Int) {
    resourceIntegrations(resourceId: $resourceId, serviceId: $serviceId, service_version_id: $serviceVersionId, limit: $limit, offset: $offset) {
      id service_id name description version status method path deprecated deprecation_date
    }
  }
`;

type BuilderVersionService = Pick<
  Service,
  "current_service_version" | "resources"
> & { webhooks?: WebhookObject[] };

type BuilderVersionContract = { service: BuilderVersionService | null };

type BuilderServiceBootstrap = {
  service: Pick<
    Service,
    | "current_service_version"
    | "resources"
    | "webhooks"
    | "endpoint_count"
    | "webhook_count"
    | "event_extraction_path"
    | "incoming_webhook_config"
  > | null;
  serviceVersions: SdkServiceVersion[];
};

// canLoadBuilderService prevents duplicate service-contract requests.
function canLoadBuilderService(
  service: ServiceData | undefined,
  loaded: boolean,
  loading: boolean
): boolean {
  return Boolean(service) && !loaded && !loading;
}

// builderBootstrapRows normalizes nullable GraphQL lists for builder state.
function builderBootstrapRows(response: BuilderServiceBootstrap) {
  return {
    service: response.service,
    webhooks: response.service?.webhooks || [],
    versions: response.serviceVersions || [],
  };
}

// effectiveBuilderVersion selects the version that produced the bootstrap service.
function effectiveBuilderVersion(
  versions: SdkServiceVersion[],
  currentVersion?: string
): SdkServiceVersion | undefined {
  return versions.find((candidate) => candidate.name === currentVersion) || versions[0];
}

// loadBuilderVersionContract returns resources and webhooks from one immutable
// service version after the user changes the builder pin.
async function loadBuilderVersionContract(
  serviceId: string,
  version: string
): Promise<BuilderVersionService> {
  const response = await api.graphql<BuilderVersionContract>(`
    query($id: String!, $version: String!) {
      service(id: $id, version: $version) {
        current_service_version
        resources { id name }
        webhooks { id name description method }
      }
    }
  `, { id: serviceId, version });
  if (!response.service) throw new Error("Selected service version was not found");
  return response.service;
}

// appServicesConfig keys selected service contracts by their stable SDK identity.
function appServicesConfig(selections: AppSelection[], services: ServiceData[]): Record<string, unknown> {
  return Object.fromEntries(selections.map((selection) => appServiceEntry(selection, services)));
}

// appServiceEntry serializes one selected service into declarative config.
function appServiceEntry(selection: AppSelection, services: ServiceData[]): [string, Record<string, unknown>] {
  const service = services.find((item) => item.service.id === selection.service_id);
  return [appSelectionKey(selection), {
    version: service?.serviceVersions.find((version) => version.id === selection.service_version_id)?.name,
    operations: appOperations(selection, service),
    webhooks: appWebhooks(selection, service),
    select_all: selection.select_all,
  }];
}

// appSelectionKey prefers portable service identities over Registry row IDs.
function appSelectionKey(selection: AppSelection): string {
  return selection.service_slug || selection.service_name || selection.service_id;
}

// appOperations maps selected endpoint IDs back to operation names.
function appOperations(selection: AppSelection, service?: ServiceData): string[] {
  if (selection.select_all) return [];
  const selected = new Set(selection.endpoint_ids);
  return (service?.integrations || []).filter((endpoint) => selected.has(endpoint.id)).map((endpoint) => endpoint.name);
}

// appWebhooks maps selected webhook IDs back to event names.
function appWebhooks(selection: AppSelection, service?: ServiceData): string[] {
  const selected = new Set(selection.webhook_ids);
  return (service?.webhooks || []).filter((webhook) => selected.has(webhook.id)).map((webhook) => webhook.name);
}

// capitalizeFirstLetter formats service names without changing their stored identity.
const capitalizeFirstLetter = (value?: string | null) => {
  if (!value) return "";
  return value.charAt(0).toUpperCase() + value.slice(1);
};

// loadRegistryServicesByIDs hydrates authorized selector results from the Registry.
async function loadRegistryServicesByIDs(serviceIds: string[]): Promise<Service[]> {
  if (serviceIds.length === 0) return [];
  const response = await api.graphql<{ servicesByIds: Service[] }>(`
    query AppBuilderServices($serviceIds: [String!]!) {
      servicesByIds(serviceIds: $serviceIds) {
        id name slug canonical_ref provider { name handle } description
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

  // handleAdd activates the exact selected version in the current workspace.
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

type BuilderServiceInteractions = {
  expanded: Record<string, boolean>;
  loadedServices: Record<string, boolean>;
  selections: Record<string, Set<string>>;
  webhookSelections: Record<string, Set<string>>;
  selectAllServices: Set<string>;
  loadingService: Record<string, boolean>;
  expandedSections: Record<string, { endpoints: boolean; webhooks: boolean }>;
  versionSelections: Record<string, string>;
  hasMoreResources: Record<string, boolean>;
  loadingResourceByName: Record<string, boolean>;
  toggleExpand: (serviceId: string) => void;
  toggleSection: (serviceId: string, section: "endpoints" | "webhooks") => void;
  toggleEndpoint: (serviceId: string, endpointId: string) => void;
  toggleWebhook: (serviceId: string, webhookId: string) => void;
  toggleSelectAllEndpoints: (serviceId: string) => void;
  toggleSelectAllWebhooks: (serviceId: string, webhooks: WebhookObject[]) => void;
  handleVersionSelection: (serviceId: string, serviceVersionId: string) => void;
  loadMoreResource: (serviceId: string, resourceName: string) => void;
  loadResourceEndpoints: (serviceId: string, resourceId: string, resourceName: string) => void;
};

type BuilderSelectionPaneProps = BuilderServiceInteractions & {
  data: ServiceData[];
  generationMode: GenerationMode;
  query: string;
  setQuery: (query: string) => void;
  searching: boolean;
  handleSearch: (event: FormEvent) => void;
  handleClear: () => void;
  workspaceServicesLoaded: boolean;
  workspaceServiceCount: number;
  ownerTeamId: string;
  loading: boolean;
  page: number;
  totalPages: number;
  totalItems: number;
  setPage: (page: number | ((previous: number) => number)) => void;
};

type BuilderPageProps = {
  generationMode: GenerationMode;
  error: string;
  loading: boolean;
  selection: BuilderSelectionPaneProps;
  generation: ConsumerGenerationPanelProps;
};

type ServiceCardView = {
  expanded: boolean;
  loaded: boolean;
  selectedEndpoints: Set<string>;
  selectedWebhooks: Set<string>;
  selectAll: boolean;
  endpointCount: number;
  webhookCount: number;
  totalSelected: number;
};

// reportedCapabilityCount prefers Registry totals while retaining loaded-row fallback.
function reportedCapabilityCount(reported: number | undefined, loaded: number): number {
  if (reported) return reported;
  return loaded;
}

// serviceCardView derives display-only counts and selection state for one service.
function serviceCardView(item: ServiceData, state: BuilderServiceInteractions): ServiceCardView {
  const serviceId = item.service.id;
  const selectedEndpoints = state.selections[serviceId] || new Set<string>();
  const selectedWebhooks = state.webhookSelections[serviceId] || new Set<string>();
  const selectAll = state.selectAllServices.has(serviceId);
  const endpointCount = reportedCapabilityCount(item.service.endpoint_count, item.integrations.length);
  const webhookCount = reportedCapabilityCount(item.service.webhook_count, item.webhooks.length);
  const selectedEndpointCount = selectAll ? endpointCount : selectedEndpoints.size;
  return {
    expanded: Boolean(state.expanded[serviceId]),
    loaded: Boolean(state.loadedServices[serviceId]),
    selectedEndpoints,
    selectedWebhooks,
    selectAll,
    endpointCount,
    webhookCount,
    totalSelected: selectedEndpointCount + selectedWebhooks.size,
  };
}

// serviceReference renders the stable canonical or provider-scoped identity.
function serviceReference(service: Service): string {
  if (service.canonical_ref) return service.canonical_ref;
  if (service.provider) return `@${service.provider.handle}/${service.slug}`;
  return "";
}

// serviceSelectionSummary explains the service-card expansion state.
function serviceSelectionSummary(loaded: boolean, selected: number): string {
  if (loaded && selected > 0) return `${selected} selected`;
  return "Expand to view endpoints and webhooks";
}

// BuilderServiceCardHeader renders the compact identity and selected count.
function BuilderServiceCardHeader({
  item,
  view,
  onExpand,
}: {
  item: ServiceData;
  view: ServiceCardView;
  onExpand: () => void;
}) {
  const reference = serviceReference(item.service);
  const roundedClass = view.expanded ? "rounded-t-xl" : "rounded-xl";
  return (
    <div
      className={`flex items-center justify-between p-4 cursor-pointer hover:bg-slate-50 transition-colors ${roundedClass}`}
      onClick={onExpand}
    >
      <div className="flex min-w-0 items-center gap-3">
        <button className="shrink-0 text-slate-400 hover:text-slate-600 transition-colors">
          {view.expanded ? <ChevronDown className="w-5 h-5" /> : <ChevronRight className="w-5 h-5" />}
        </button>
        <div className="min-w-0">
          <h3 className="font-semibold text-slate-900 flex items-center gap-2">
            <span className="truncate">{capitalizeFirstLetter(item.service.name)}</span>
          </h3>
          <p className="truncate text-xs text-slate-400">
            {reference}{reference ? " · " : ""}
            {serviceSelectionSummary(view.loaded, view.totalSelected)}
          </p>
        </div>
      </div>
    </div>
  );
}

// BuilderVersionPicker pins all subsequent resource loads to one immutable version.
function BuilderVersionPicker({
  serviceId,
  versions,
  selectedVersionId,
  generationMode,
  onSelect,
}: {
  serviceId: string;
  versions: SdkServiceVersion[];
  selectedVersionId: string;
  generationMode: GenerationMode;
  onSelect: (serviceId: string, versionId: string) => void;
}) {
  if (versions.length === 0) return null;
  const selectedVersion = versions.find((version) => version.id === selectedVersionId) || versions[0];
  const title = selectedVersion?.header_value || selectedVersion?.name || "";
  return (
    <div className="flex min-w-0 flex-col gap-3 border-b border-slate-100 bg-slate-50 px-4 py-4 sm:flex-row sm:items-center sm:justify-between sm:px-5">
      <div className="flex min-w-0 flex-col">
        <span className="text-sm font-semibold text-slate-800">Service Version</span>
        <span className="text-xs text-slate-500">
          Select the version this {generationArtifactName(generationMode).toLowerCase()} will use
        </span>
      </div>
      <select
        value={selectedVersionId}
        onChange={(event) => onSelect(serviceId, event.target.value)}
        title={title}
        className="block w-full min-w-0 truncate rounded-lg border border-slate-300 bg-white px-3 py-1.5 text-sm shadow-sm outline-none focus:border-blue-500 focus:ring-2 focus:ring-blue-500 sm:w-[min(55%,22rem)]"
      >
        {versions.map((version) => (
          <option key={version.id} value={version.id}>{version.header_value || version.name}</option>
        ))}
      </select>
    </div>
  );
}

// EndpointSelectAllButton toggles the compact all-operations representation.
function EndpointSelectAllButton({
  serviceId,
  view,
  onToggle,
}: {
  serviceId: string;
  view: ServiceCardView;
  onToggle: (serviceId: string) => void;
}) {
  if (view.endpointCount === 0) return null;
  return (
    <button
      type="button"
      onClick={(event) => {
        event.stopPropagation();
        onToggle(serviceId);
      }}
      className="text-[10px] font-medium px-2 py-1 rounded text-slate-500 hover:text-slate-700 hover:bg-slate-200 transition-colors flex items-center gap-1.5"
    >
      {view.selectAll ? (
        <><CheckSquare className="w-3.5 h-3.5 text-blue-600" />Deselect All</>
      ) : (
        <>
          {view.selectedEndpoints.size > 0
            ? <Square className="w-3.5 h-3.5 text-slate-400 fill-slate-200" />
            : <Square className="w-3.5 h-3.5 text-slate-400" />}
          Select All ({view.endpointCount})
        </>
      )}
    </button>
  );
}

// BuilderEndpointsSection renders exact-version operation resources and paging.
function BuilderEndpointsSection({
  item,
  view,
  sections,
  state,
}: {
  item: ServiceData;
  view: ServiceCardView;
  sections: { endpoints: boolean; webhooks: boolean };
  state: BuilderServiceInteractions;
}) {
  const serviceId = item.service.id;
  return (
    <>
      <div
        onClick={() => state.toggleSection(serviceId, "endpoints")}
        className="w-full flex items-center justify-between px-3 py-2 hover:bg-slate-100 transition-colors cursor-pointer group"
      >
        <div className="flex items-center gap-2">
          {sections.endpoints
            ? <ChevronDown className="w-3.5 h-3.5 text-slate-400" />
            : <ChevronRight className="w-3.5 h-3.5 text-slate-400" />}
          <span className="text-xs font-bold uppercase tracking-wider text-slate-500">
            Endpoints {view.endpointCount > 0 ? `(${view.endpointCount})` : ""}
          </span>
        </div>
        <EndpointSelectAllButton serviceId={serviceId} view={view} onToggle={state.toggleSelectAllEndpoints} />
      </div>
      {sections.endpoints && (
        <div className="px-2 pb-2">
          <EndpointSelectionList
            endpoints={item.integrations}
            selectedIds={view.selectedEndpoints}
            isSelectAll={view.selectAll}
            onToggle={(endpointId) => state.toggleEndpoint(serviceId, endpointId)}
            getId={(endpoint) => endpoint.id}
            maxHeightClass=""
            hasMoreResources={state.hasMoreResources}
            onLoadMoreResource={(resourceName) => state.loadMoreResource(serviceId, resourceName)}
            resourceMetadata={item.service.resources || []}
            loadingResources={state.loadingResourceByName}
            onResourceExpand={(resourceId, resourceName) => state.loadResourceEndpoints(serviceId, resourceId, resourceName)}
          />
        </div>
      )}
    </>
  );
}

// WebhookSelectAllButton toggles every currently loaded webhook for a service.
function WebhookSelectAllButton({
  item,
  view,
  onToggle,
}: {
  item: ServiceData;
  view: ServiceCardView;
  onToggle: (serviceId: string, webhooks: WebhookObject[]) => void;
}) {
  if (view.webhookCount === 0 || !view.loaded) return null;
  const allSelected = view.selectedWebhooks.size === item.webhooks.length;
  return (
    <button
      type="button"
      onClick={(event) => {
        event.stopPropagation();
        onToggle(item.service.id, item.webhooks);
      }}
      className="text-[10px] font-medium px-2 py-1 rounded text-slate-500 hover:text-slate-700 hover:bg-slate-200 transition-colors flex items-center gap-1.5"
    >
      {allSelected ? (
        <><CheckSquare className="w-3.5 h-3.5 text-blue-600" />Deselect All</>
      ) : (
        <>
          {view.selectedWebhooks.size > 0
            ? <Square className="w-3.5 h-3.5 text-slate-400 fill-slate-200" />
            : <Square className="w-3.5 h-3.5 text-slate-400" />}
          Select All ({view.webhookCount})
        </>
      )}
    </button>
  );
}

// BuilderWebhooksSection renders webhook choices shared by SDK and MCP plans.
function BuilderWebhooksSection({
  item,
  view,
  sections,
  state,
}: {
  item: ServiceData;
  view: ServiceCardView;
  sections: { endpoints: boolean; webhooks: boolean };
  state: BuilderServiceInteractions;
}) {
  const hasWebhooks = view.webhookCount > 0;
  const headerClass = hasWebhooks
    ? "hover:bg-slate-100 cursor-pointer"
    : "cursor-default opacity-40";
  return (
    <>
      <div
        onClick={() => {
          if (hasWebhooks) state.toggleSection(item.service.id, "webhooks");
        }}
        className={`w-full flex items-center justify-between px-3 py-2 transition-colors border-t border-slate-100 group ${headerClass}`}
      >
        <div className="flex items-center gap-2">
          {sections.webhooks
            ? <ChevronDown className="w-3.5 h-3.5 text-slate-400" />
            : <ChevronRight className="w-3.5 h-3.5 text-slate-400" />}
          <span className="text-xs font-bold uppercase tracking-wider text-slate-500">
            Webhooks {view.webhookCount > 0 ? `(${view.webhookCount})` : ""}
          </span>
          {!hasWebhooks && (
            <span className="ml-2 text-[10px] text-slate-400 font-normal normal-case tracking-normal">Not configured</span>
          )}
        </div>
        <WebhookSelectAllButton item={item} view={view} onToggle={state.toggleSelectAllWebhooks} />
      </div>
      {sections.webhooks && hasWebhooks && (
        <div className="px-2 pb-2">
          <WebhookSelectionList
            webhooks={item.webhooks}
            selectedIds={view.selectedWebhooks}
            onToggle={(webhookId) => state.toggleWebhook(item.service.id, webhookId)}
            getId={(webhook) => webhook.id}
            maxHeightClass=""
          />
        </div>
      )}
    </>
  );
}

// BuilderExpandedService composes version, operation, and webhook selectors.
function BuilderExpandedService({
  item,
  view,
  generationMode,
  state,
}: {
  item: ServiceData;
  view: ServiceCardView;
  generationMode: GenerationMode;
  state: BuilderServiceInteractions;
}) {
  const serviceId = item.service.id;
  const sections = state.expandedSections[serviceId] || { endpoints: true, webhooks: true };
  const selectedVersionId = state.versionSelections[serviceId] || item.serviceVersions[0]?.id || "";
  return (
    <div>
      <BuilderVersionPicker
        serviceId={serviceId}
        versions={item.serviceVersions}
        selectedVersionId={selectedVersionId}
        generationMode={generationMode}
        onSelect={state.handleVersionSelection}
      />
      <BuilderEndpointsSection item={item} view={view} sections={sections} state={state} />
      {/* SDK and MCP share the same webhook bundle selection contract. */}
      <BuilderWebhooksSection item={item} view={view} sections={sections} state={state} />
    </div>
  );
}

// BuilderServiceCard renders one collapsible service selection surface.
function BuilderServiceCard({
  item,
  generationMode,
  state,
}: {
  item: ServiceData;
  generationMode: GenerationMode;
  state: BuilderServiceInteractions;
}) {
  const view = serviceCardView(item, state);
  return (
    <div className="min-w-0 overflow-hidden bg-white rounded-xl border border-slate-200 shadow-sm hover:shadow-md transition-shadow">
      <BuilderServiceCardHeader
        item={item}
        view={view}
        onExpand={() => state.toggleExpand(item.service.id)}
      />
      {view.expanded && (
        <div className="min-w-0 overflow-x-hidden border-t border-slate-100 bg-slate-50/50 max-h-[500px] overflow-y-auto">
          {state.loadingService[item.service.id] ? (
            <div className="flex items-center justify-center gap-2 py-6 text-slate-400">
              <Loader2 className="w-4 h-4 animate-spin" />
              <span className="text-sm">Loading...</span>
            </div>
          ) : (
            <BuilderExpandedService item={item} view={view} generationMode={generationMode} state={state} />
          )}
        </div>
      )}
    </div>
  );
}

// emptyServiceCopy selects the most useful recovery guidance for an empty catalog.
function emptyServiceCopy(input: {
  query: string;
  workspaceServicesLoaded: boolean;
  workspaceServiceCount: number;
  ownerTeamId: string;
}): { title: string; detail: string } {
  if (input.query) {
    return {
      title: "No services in your workspace match your search.",
      detail: "Try another service name or activate one from the service catalog.",
    };
  }
  if (input.workspaceServicesLoaded && input.workspaceServiceCount === 0) {
    return {
      title: "No services added yet.",
      detail: "Define and activate a service before creating an app or MCP server.",
    };
  }
  if (input.ownerTeamId) {
    return {
      title: "No shared services are available.",
      detail: "Ask an access administrator to give both you and the team access.",
    };
  }
  return {
    title: "No services are available with your access.",
    detail: "Ask a workspace administrator for access to the services and credential sets you need.",
  };
}

// BuilderEmptyServices renders access-aware guidance when no services are available.
function BuilderEmptyServices(props: Pick<
  BuilderSelectionPaneProps,
  "query" | "workspaceServicesLoaded" | "workspaceServiceCount" | "ownerTeamId"
>) {
  const copy = emptyServiceCopy(props);
  return (
    <div className="text-center py-12 text-slate-400 bg-white rounded-xl border border-dashed border-slate-200">
      <p className="font-medium text-slate-600">{copy.title}</p>
      <p className="mt-1 text-sm">{copy.detail}</p>
    </div>
  );
}

// BuilderSearchForm controls server-backed service search without local filtering.
function BuilderSearchForm({
  query,
  setQuery,
  searching,
  handleSearch,
  handleClear,
}: Pick<BuilderSelectionPaneProps, "query" | "setQuery" | "searching" | "handleSearch" | "handleClear">) {
  return (
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
        {searching ? <Loader2 className="w-5 h-5 animate-spin" /> : <Search className="w-5 h-5" />}
      </button>
      <input
        type="text"
        placeholder="Search for a service (e.g. Stripe, Shopify...)"
        value={query}
        onChange={(event) => setQuery(event.target.value)}
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
  );
}

// BuilderServiceList delegates each service to a bounded-complexity card.
function BuilderServiceList(props: BuilderSelectionPaneProps) {
  if (props.data.length === 0) return <BuilderEmptyServices {...props} />;
  return (
    <>
      {props.data.map((item) => (
        <BuilderServiceCard key={item.service.id} item={item} generationMode={props.generationMode} state={props} />
      ))}
    </>
  );
}

// BuilderPagination renders server-backed pages only for unfiltered catalogs.
function BuilderPagination(props: Pick<
  BuilderSelectionPaneProps,
  "loading" | "query" | "page" | "totalPages" | "totalItems" | "setPage"
>) {
  if (props.loading || props.query || props.totalPages <= 1) return null;
  const start = props.totalItems === 0 ? 0 : (props.page - 1) * 20 + 1;
  return (
    <div className="flex items-center justify-between gap-3 border border-slate-200 px-4 py-3 bg-white rounded-xl shadow-sm mt-4">
      <p className="text-xs text-slate-500">
        {start}-{Math.min(props.totalItems, props.page * 20)} of {props.totalItems}
      </p>
      <div className="flex items-center gap-1">
        <button
          type="button"
          data-track="paginate_previous"
          onClick={() => props.setPage((page) => Math.max(1, page - 1))}
          disabled={props.page === 1}
          className="rounded-md p-1.5 text-slate-500 hover:bg-slate-100 disabled:opacity-40 disabled:hover:bg-transparent"
          aria-label="Previous page"
          title="Previous"
        >
          <ChevronLeft className="w-4 h-4" />
        </button>
        <span className="text-xs text-slate-500 pl-2">Page</span>
        <select
          className="bg-white border border-slate-200 rounded px-2 py-1 text-xs focus:outline-none focus:ring-2 focus:ring-blue-500 mx-1 cursor-pointer"
          value={props.page}
          onChange={(event) => props.setPage(parseInt(event.target.value, 10))}
        >
          {Array.from({ length: props.totalPages }, (_, index) => index + 1).map((page) => (
            <option key={page} value={page}>{page}</option>
          ))}
        </select>
        <span className="text-xs font-medium text-slate-500 pr-2">of {props.totalPages}</span>
        <button
          type="button"
          data-track="paginate_next"
          onClick={() => props.setPage((page) => page + 1)}
          disabled={props.page >= props.totalPages}
          className="rounded-md p-1.5 text-slate-500 hover:bg-slate-100 disabled:opacity-40 disabled:hover:bg-transparent"
          aria-label="Next page"
          title="Next"
        >
          <ChevronRight className="w-4 h-4" />
        </button>
      </div>
    </div>
  );
}

// BuilderSelectionPane composes search, service cards, and pagination.
function BuilderSelectionPane(props: BuilderSelectionPaneProps) {
  return (
    <div className="flex-1 flex flex-col min-h-0 min-w-0">
      <BuilderSearchForm {...props} />
      <div className="flex-1 overflow-y-auto pr-2 pb-8 space-y-4">
        <BuilderServiceList {...props} />
        <BuilderPagination {...props} />
      </div>
    </div>
  );
}

// BuilderPageHeader names the artifact being configured.
function BuilderPageHeader({ generationMode }: { generationMode: GenerationMode }) {
  const isMCP = generationMode === "mcp";
  return (
    <div className="flex items-center justify-between mb-8">
      <div>
        <h1 className="text-3xl font-bold text-slate-900 tracking-tight mb-1">
          {isMCP ? "Create MCP server" : "Create app"}
        </h1>
        <p className="text-slate-500">
          {isMCP
            ? "Choose the services and operations to make available through MCP."
            : "Choose the services and operations this app can use."}
        </p>
      </div>
    </div>
  );
}

// BuilderPage renders the builder shell without owning execution state.
function BuilderPage({ generationMode, error, loading, selection, generation }: BuilderPageProps) {
  return (
    <div className="max-w-6xl mx-auto py-8 px-4 h-full flex flex-col">
      <BuilderPageHeader generationMode={generationMode} />
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
          <BuilderSelectionPane {...selection} />
          <ConsumerGenerationPanel {...generation} />
        </div>
      )}
    </div>
  );
}

// initialBuilderServiceId resolves a route-selected service only when the loader is unambiguous.
function initialBuilderServiceId(searchParams: URLSearchParams, services: Service[]): string | undefined {
  const selected = searchParams.get("serviceId") || searchParams.get("service") || searchParams.get("slug");
  if (!selected || services.length !== 1) return undefined;
  return services[0]?.id;
}

// SdkBuilder assembles exact-version service selections into an app contract.
export default function SdkBuilder() {
  const { access } = useCurrentActorAccess();
  const canReadApps = hasAnyPermission(access, "app.read");
  const canReadServices = hasAnyPermission(access, "service.read");
  const toast = useToast();
  const loaderData = useLoaderData<typeof clientLoader>();
  const [searchParams, setSearchParams] = useSearchParams();
  const isAuth = loaderData.isAuth;
  const initialSelectedServiceId = initialBuilderServiceId(searchParams, loaderData.services);

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
  // toggleSection expands one capability group without affecting sibling services.
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
  const [appVersion, setAppVersion] = useState("1.0.0");
  const [ownerTeams, setOwnerTeams] = useState<AppOwningTeam[]>([]);
  const [ownerTeamId, setOwnerTeamId] = useState("");
  const [availableBuckets, setAvailableBuckets] = useState<AppBuildSelector[]>([]);
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
  const [generationMode] = useState<"sdk" | "mcp">(() => {
    const tab = searchParams.get("tab");
    return tab === "mcp" ? "mcp" : "sdk";
  });
  const [language, setLanguage] = useState<"typescript" | "python">("typescript");

  useEffect(() => {
    document.title = generationMode === "mcp" ? "Create MCP server - Fused" : "Create app - Fused";
  }, [generationMode]);

  const [searching, setSearching] = useState(false);
  const pageParam = searchParams.get("page");
  const page = pageParam ? parseInt(pageParam, 10) : 1;
  // setPage keeps server-backed pagination reflected in the route URL.
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

  type RawServiceWithExtras = Omit<Service, "resources"> & {
    resources?: (NonNullable<Service["resources"]>[number] & {
      integrations?: Omit<IntegrationObject, "resource" | "resource_id">[];
    })[];
    serviceVersions?: SdkServiceVersion[];
  };

  // processResponse normalizes authorized service rows into builder state.
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

  // loadData fetches one authorized selector page and hydrates its services.
  async function loadData(pageNum: number, search = "") {
    setLoading(true);
    setError("");
    try {
      const limit = 20;
      const selectors = await listAppBuildSelectors(ownerTeamId, "SERVICE", search, limit, (pageNum - 1) * limit);
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

  // runSearch resets pagination and performs a server-backed service search.
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

  // handleSearch submits the current service query without navigation.
  async function handleSearch(e: FormEvent) {
    e.preventDefault();
    runSearch(query);
  }

  // handleClear restores the unfiltered first selector page.
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
    listAppOwningTeams()
      .then((page) => {
        setOwnerTeams(page.items);
      })
      .catch((cause: unknown) => setError(cause instanceof Error ? cause.message : "Could not load owning teams."));
  }, []);

  // loadAvailableBuckets refreshes credential sets usable by both actor and owner.
  const loadAvailableBuckets = () =>
    listAppBuildSelectors(ownerTeamId, "BUCKET", "", 100, 0).then((bucketPage) => {
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

  // createCredential preserves builder state while opening credential creation.
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
    if (!isAuth || !canReadServices) {
      // Builder-only actors rely on plan/apply for authoritative workspace
      // validation instead of issuing a service.read query they cannot use.
      setWorkspaceServicesLoaded(false);
      return;
    }
    api.workspace.getServices()
      .then(services => setWorkspaceServiceIds(new Set(services.map(s => s.service_id))))
      .catch(() => {})
      .finally(() => setWorkspaceServicesLoaded(true));
  }, [canReadServices, isAuth]);

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

  // loadMoreResource appends operations without crossing the selected version.
  const loadMoreResource = async (serviceId: string, resourceName: string) => {
    // Find the resourceId based on the first endpoint's resource_id in this service
    const serviceData = data.find(s => s.service.id === serviceId);
    if (!serviceData) return;
    const sample = serviceData.integrations.find(ep => ep.resource === resourceName);
    if (!sample || !sample.resource_id) return;
    
    const resourceId = sample.resource_id;
    const serviceVersionId = versionSelections[serviceId];
    // Exact SDK scopes must never load an unversioned union of operations.
    if (!serviceVersionId) return;
    if (loadingResource[resourceId] || !hasMoreResources[resourceId]) return;
    
    setLoadingResource(prev => ({ ...prev, [resourceId]: true }));
    try {
      const currentOffset = resourceOffsets[resourceId] || 0;
      const response = await api.graphql<{ resourceIntegrations: IntegrationObject[] }>(BUILDER_RESOURCE_GQL, {
        resourceId,
        serviceId,
        serviceVersionId,
        limit: 50,
        offset: currentOffset,
      });
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

  // loadServiceIntegrations loads versions plus the effective version contract.
  const loadServiceIntegrations = async (serviceId: string) => {
    const serviceData = data.find(s => s.service.id === serviceId);
    if (!canLoadBuilderService(serviceData, loadedServices[serviceId], loadingService[serviceId])) return;

    setLoadingService(prev => ({ ...prev, [serviceId]: true }));
    try {
      // Only fetch webhooks and service versions on expand — endpoints load lazily per resource
      const webhookRes = await api.graphql<BuilderServiceBootstrap>(`
        query($id: String!) {
          service(id: $id) {
            current_service_version
            endpoint_count
            webhook_count
            event_extraction_path
            incoming_webhook_config { auth_type }
            resources { id name }
            webhooks { id name description method }
          }
          serviceVersions(serviceId: $id) { id name header_value status }
        }
      `, { id: serviceId });

      const bootstrap = builderBootstrapRows(webhookRes);
      setData(prev => prev.map(s =>
        s.service.id === serviceId
          ? {
              ...s,
              service: {
                ...s.service,
                current_service_version: bootstrap.service?.current_service_version,
                resources: bootstrap.service?.resources || [],
                endpoint_count: bootstrap.service?.endpoint_count,
                webhook_count: bootstrap.service?.webhook_count,
                event_extraction_path: bootstrap.service?.event_extraction_path,
                incoming_webhook_config: bootstrap.service?.incoming_webhook_config,
              },
              webhooks: bootstrap.webhooks,
              serviceVersions: bootstrap.versions,
            }
          : s
      ));
      
      const serviceVersion = effectiveBuilderVersion(
        bootstrap.versions,
        bootstrap.service?.current_service_version
      );
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

  // handleVersionSelection replaces all version-bound builder rows together.
  async function handleVersionSelection(serviceId: string, serviceVersionId: string) {
    const serviceData = data.find((candidate) => candidate.service.id === serviceId);
    const selected = serviceData?.serviceVersions.find(
      (candidate) => candidate.id === serviceVersionId
    );
    if (!serviceData || !selected) return;

    const previousVersionId = versionSelections[serviceId];
    setVersionSelections((previous) => ({ ...previous, [serviceId]: serviceVersionId }));
    try {
      const contract = await loadBuilderVersionContract(serviceId, selected.name);
      setData((previous) => previous.map((candidate) =>
        candidate.service.id === serviceId
          ? {
              ...candidate,
              service: {
                ...candidate.service,
                current_service_version: contract.current_service_version,
                resources: contract.resources || [],
              },
              integrations: [],
              webhooks: contract.webhooks || [],
            }
          : candidate
      ));
      // Explicit IDs belong to a version, so carrying them across a pin change
      // could generate a selection the Registry correctly rejects.
      setSelections((previous) => ({ ...previous, [serviceId]: new Set() }));
      setWebhookSelections((previous) => ({ ...previous, [serviceId]: new Set() }));
      setSelectAllServices((previous) => {
        const next = new Set(previous);
        next.delete(serviceId);
        return next;
      });
    } catch (cause) {
      setVersionSelections((previous) => {
        const next = { ...previous };
        if (previousVersionId) next[serviceId] = previousVersionId;
        else delete next[serviceId];
        return next;
      });
      toast.error(cause instanceof Error ? cause.message : "Failed to load the selected service version");
    }
  }

  // loadResourceEndpoints loads the first operation page for the selected version.
  const loadResourceEndpoints = async (serviceId: string, resourceId: string, resourceName: string) => {
    const serviceVersionId = versionSelections[serviceId];
    if (!serviceVersionId) return;
    if (loadingResourceByName[resourceName]) return;
    setLoadingResourceByName(prev => ({ ...prev, [resourceName]: true }));
    try {
      const response = await api.graphql<{ resourceIntegrations: IntegrationObject[] }>(
        BUILDER_RESOURCE_GQL,
        { resourceId, serviceId, serviceVersionId, limit: 50, offset: 0 }
      );
      const enriched = response.resourceIntegrations.map(ep => ({ ...ep, resource: resourceName, resource_id: resourceId }));
      setData(prev => prev.map(s => {
        if (s.service.id !== serviceId) return s;
        // Append, deduplicating by id
        const existing = new Set(s.integrations.map(e => e.id));
        return { ...s, integrations: [...s.integrations, ...enriched.filter(e => !existing.has(e.id))] };
      }));
      const hasMore = response.resourceIntegrations.length === 50;
      setHasMoreResources(prev => ({ ...prev, [resourceId]: hasMore }));
      setResourceOffsets(prev => ({ ...prev, [resourceId]: hasMore ? 50 : 0 }));
    } catch (err) {
      toast.error("Failed to load endpoints for " + resourceName + ": " + (err instanceof Error ? err.message : String(err)));
    } finally {
      setLoadingResourceByName(prev => ({ ...prev, [resourceName]: false }));
    }
  };

  // toggleExpand lazily loads immutable service metadata on first expansion.
  const toggleExpand = (serviceId: string) => {
    const willExpand = !expanded[serviceId];
    setExpanded(prev => ({ ...prev, [serviceId]: willExpand }));
    if (willExpand) {
      loadServiceIntegrations(serviceId);
    }
  };

  // toggleEndpoint changes one explicit operation selection.
  const toggleEndpoint = (serviceId: string, endpointId: string) => {
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

  // toggleWebhook changes one explicit event selection.
  const toggleWebhook = (serviceId: string, webhookId: string) => {
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

  // toggleSelectAllEndpoints switches between compact all and explicit operation IDs.
  const toggleSelectAllEndpoints = (serviceId: string) => {
    const isSelectAll = selectAllServices.has(serviceId);
    if (isSelectAll) {
      setSelectAllServices(prev => { const s = new Set(prev); s.delete(serviceId); return s; });
      setSelections(prev => ({ ...prev, [serviceId]: new Set() }));
    } else {
      setSelectAllServices(prev => new Set([...prev, serviceId]));
      setSelections(prev => ({ ...prev, [serviceId]: new Set() }));
    }
  };

  // toggleSelectAllWebhooks selects or clears every loaded webhook for a service.
  const toggleSelectAllWebhooks = (serviceId: string, webhooks: WebhookObject[]) => {
    if (webhooks.length === 0) return;
    const allSelected = (webhookSelections[serviceId]?.size || 0) === webhooks.length;
    if (allSelected) {
      setWebhookSelections(prev => ({ ...prev, [serviceId]: new Set() }));
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
  const unactivatedSelectedServiceIds = isAuth && canReadServices && workspaceServicesLoaded
    ? selectedServiceIdsForGate.filter(id => !workspaceServiceIds.has(id))
    : [];

  // handleServiceAddedToWorkspace updates the local activation gate after success.
  const handleServiceAddedToWorkspace = (serviceId: string) => {
    setWorkspaceServiceIds(prev => new Set(prev).add(serviceId));
  };

  /** Previews duplicate versions when app-read access is available. */
  const checkDuplicateSDK = async () => {
    if (!sdkName.trim() || !appVersion.trim()) {
      setIsDuplicate(false);
      return;
    }
    if (!canReadApps) {
      // Immutable-version conflicts are still rejected by plan/apply; skipping
      // this optional preview avoids turning app.create into implicit app.read.
      setIsDuplicate(false);
      return;
    }
    setCheckingDuplicate(true);
    try {
      const queryStr = `query($search: String!, $version: String!) { apps(kind: "sdk", search: $search, version: $version, limit: 1, offset: 0) { items { name version } } }`;
      const res = await api.mcpGraphql<{ apps: { items: { name: string; version: string }[] } }>(queryStr, {
        search: sdkName.trim(),
        version: appVersion.trim(),
      });
      setIsDuplicate(res.apps.items.some(item => item.name === sdkName.trim() && item.version === appVersion.trim()));
    } catch {
      setIsDuplicate(false);
    } finally {
      setCheckingDuplicate(false);
    }
  };

  // handleGenerate validates, plans, applies, and reports one app build.
  const handleGenerate = async (e: FormEvent) => {
    e.preventDefault();
    const selectionPayload = buildAppSelections(data, {
      selections,
      selectAllServices,
      webhookSelections,
      versionSelections,
    });
    const validation = validateGenerationInput({
      selections: selectionPayload,
      data,
      sdkName,
      generationMode,
      availableBuckets,
      bucketId,
      ownerTeamId,
      webhookAttachment,
    });
    if (!validation.ok) {
      reportGenerationValidation(toast, validation);
      return;
    }

    const confirmed = await confirmDuplicateGeneration({
      toast,
      mode: generationMode,
      duplicate: isDuplicate,
      name: sdkName,
      version: appVersion,
    });
    if (!confirmed) return;

    setGenerating(true);
    setGenerateStatus("Starting generation...");
    setSdkDeployment(null);
    setMcpDeployment(null);
    setSdkTokenCopied(false);
    setMcpTokenCopied(false);
    try {
      const ownerTeamSlug = ownerTeams.find((team) => team.id === ownerTeamId)?.slug || "";
      const config = buildGenerationConfig({
        mode: generationMode,
        name: sdkName,
        version: appVersion,
        bucket: validation.bucket.display_name,
        selections: selectionPayload,
        data,
        language,
        webhookAttachment,
        hasWebhookSelections: validation.hasWebhookSelections,
      });

      if (generationMode === "mcp") {
        setGenerateStatus("Deploying MCP server...");
		const result = await planAndApplyApp<{ app_id: string; mcp_url: string; execution_token?: string }>("mcp", ownerTeamSlug, config);
        await syncWorkspacePinsAfterGenerate(selectionPayload);
		setMcpDeployment({ id: result.app_id, url: result.mcp_url, token: result.execution_token || "" });
        setGenerateStatus("MCP server deployed");
        return;
      }

      setGenerateStatus("Planning and generating SDK...");
      const result = await planAndApplyApp<{ app_id: string; job_id: string; execution_token?: string }>("sdk", ownerTeamSlug, config);
      await waitForSDKGeneration({
        controller: new AbortController(),
        appId: result.app_id,
        jobId: result.job_id,
        sdkName,
        appVersion,
        executionToken: result.execution_token || "",
        setStatus: setGenerateStatus,
        setDeployment: setSdkDeployment,
      });

      // Reaching here means every step above resolved without throwing --
      // generation genuinely succeeded, so this is the one place to sync
      // workspace pins for it, regardless of which success branch got hit.
      await syncWorkspacePinsAfterGenerate(selectionPayload);

    } catch (err) {
      toast.error(generationFailureMessage(generationMode, err), 0);
    } finally {
      setGenerating(false);
      setGenerateStatus("");
    }
  };

  const serviceInteractions: BuilderServiceInteractions = {
    expanded,
    loadedServices,
    selections,
    webhookSelections,
    selectAllServices,
    loadingService,
    expandedSections,
    versionSelections,
    hasMoreResources,
    loadingResourceByName,
    toggleExpand,
    toggleSection,
    toggleEndpoint,
    toggleWebhook,
    toggleSelectAllEndpoints,
    toggleSelectAllWebhooks,
    handleVersionSelection,
    loadMoreResource,
    loadResourceEndpoints,
  };
  const selection: BuilderSelectionPaneProps = {
    ...serviceInteractions,
    data,
    generationMode,
    query,
    setQuery,
    searching,
    handleSearch,
    handleClear,
    workspaceServicesLoaded,
    workspaceServiceCount: workspaceServiceIds.size,
    ownerTeamId,
    loading,
    page,
    totalPages,
    totalItems,
    setPage,
  };
  const generation: ConsumerGenerationPanelProps = {
    generationMode,
    ownerTeams,
    ownerTeamId,
    setOwnerTeamId,
    availableBuckets,
    bucketId,
    setBucketId,
    onCreateCredential: createCredential,
    sdkName,
    setSdkName,
    setIsDuplicate,
    checkDuplicateSDK,
    totalSelectedWebhooks,
    webhookAttachment,
    setWebhookAttachment,
    appVersion,
    setAppVersion,
    checkingDuplicate,
    isDuplicate,
    language,
    setLanguage,
    totalSelectedServices,
    totalSelected,
    unactivatedSelectedServiceIds,
    data,
    versionSelections,
    handleServiceAddedToWorkspace,
    handleGenerate,
    generating,
    generateStatus,
    sdkDeployment,
    sdkTokenCopied,
    setSdkTokenCopied,
    mcpDeployment,
    mcpTokenCopied,
    setMcpTokenCopied,
    AddSelectedServiceToWorkspaceButton,
  };

  return (
    <BuilderPage
      generationMode={generationMode}
      error={error}
      loading={loading}
      selection={selection}
      generation={generation}
    />
  );
}
