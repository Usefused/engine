import { FormEvent } from "react";
import { Link } from "@remix-run/react";
import { ArrowUpRight, Check, Loader2, Search, X, ChevronLeft, ChevronRight } from "lucide-react";
import { Service, ActivatedService, serviceHref } from "~/lib/api";
import { formatServiceName, formatVersion } from "~/lib/format";
import { openServiceLink } from "~/lib/service-navigation";
import { ServiceIcon } from "~/components/ServiceIcon";

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
  is_owner?: boolean | null;
  is_public?: boolean | null;
  description?: string;
  icon_url?: string | null;
  endpoint_count?: number;
  webhook_count?: number;
  base_url?: string;
  servers?: { url: string; description?: string; is_default?: boolean }[];
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
    description: s.description,
    icon_url: s.icon_url,
    base_url: s.base_url,
    endpoint_count: s.endpoint_count,
    webhook_count: s.webhook_count,
    servers: s.servers,
  };
}

/** Normalise a workspace ActivatedService into a ListableService. */
export function fromActivatedService(s: ActivatedService): ListableService {
  return {
    id: s.service_id, // Use the Registry service_id as the stable ID
    name: s.service_name,
    slug: s.registry_slug || s.service_slug,
    provider: s.provider,
    is_owner: s.is_owner,
    is_public: s.is_public,
    description: s.description,
    icon_url: s.icon_url,
    base_url: s.base_url,
    endpoint_count: s.endpoint_count,
    webhook_count: s.webhook_count,
    service_id: s.service_id,
    // UI links pair the bare Registry slug with provider identity; the CLI-oriented qualified slug is only a fallback.
    service_slug: s.registry_slug || s.service_slug,
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
  setShowNewPanel: (show: boolean) => void;
  page: number;
  totalPages: number;
  totalItems?: number;
  onPageChange: (page: number) => void;
  viewType?: "workspace" | "catalog";
  handleAddWorkspace?: (e: React.MouseEvent, id: string, name: string) => void;
  activeServiceIds?: string[];
  pendingServiceIds?: string[];
  isAuth?: boolean;
  showSearch?: boolean;
  hideEmptyState?: boolean;
  searchPlaceholder?: string;
}

// IntegrationsListTab renders one paged service projection with action state supplied by its route owner.
export default function IntegrationsListTab({
  integrations,
  loading,
  error,
  query,
  setQuery,
  handleSearch,
  handleClear,
  searching,
  setShowNewPanel,
  page,
  totalPages,
  totalItems,
  onPageChange,
  isAuth,
  viewType = "catalog",
  handleAddWorkspace,
  activeServiceIds = [],
  pendingServiceIds = [],
  showSearch = true,
  hideEmptyState = false,
  searchPlaceholder,
}: IntegrationsListTabProps) {
  return (
    <>
      {/* Combined workspace/catalogue pages own one shared search field above both collections. */}
      {showSearch && <IntegrationSearch
        query={query}
        setQuery={setQuery}
        handleSearch={handleSearch}
        handleClear={handleClear}
        searching={searching}
        isAuth={isAuth}
        viewType={viewType}
        placeholder={searchPlaceholder}
      />}
      <IntegrationError error={error} />
      <IntegrationResults
        integrations={integrations}
        loading={loading}
        searching={searching}
        query={query}
        setShowNewPanel={setShowNewPanel}
        viewType={viewType}
        isAuth={isAuth}
        handleAddWorkspace={handleAddWorkspace}
        activeServiceIds={activeServiceIds}
        pendingServiceIds={pendingServiceIds}
        hideEmptyState={hideEmptyState}
        page={page}
        totalPages={totalPages}
        totalItems={totalItems}
        onPageChange={onPageChange}
      />
    </>
  );
}

type IntegrationSearchProps = Pick<
  IntegrationsListTabProps,
  "query" | "setQuery" | "handleSearch" | "handleClear" | "searching" | "isAuth" | "viewType"
> & { placeholder?: string };

// IntegrationSearch isolates search-state decisions from list rendering so
// asynchronous feedback cannot make the row component harder to reason about.
function IntegrationSearch({ query, setQuery, handleSearch, handleClear, searching, isAuth, viewType, placeholder }: IntegrationSearchProps) {
  // A combined page can describe both collections; standalone lists retain their view-specific copy.
  const resolvedPlaceholder = placeholder || integrationSearchPlaceholder(isAuth, viewType);
  return (
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
        {searching ? <Loader2 className="w-3.5 h-3.5 animate-spin" /> : <Search className="w-3.5 h-3.5" />}
      </button>
      <input
        type="text"
        value={query}
        onChange={(event) => setQuery(event.target.value)}
        placeholder={resolvedPlaceholder}
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
  );
}

// integrationSearchPlaceholder keeps authentication and view-specific copy in
// one decision boundary shared by every search render.
function integrationSearchPlaceholder(isAuth: boolean | undefined, viewType: "workspace" | "catalog"): string {
  // Anonymous visitors search the public service surface rather than a workspace.
  if (!isAuth) {
    return "Search for a service (e.g. Stripe, Shopify...)";
  }
  // Authenticated workspace and catalog searches describe their distinct scopes.
  if (viewType === "workspace") {
    return "Search your services";
  }
  return "Search service catalog";
}

// IntegrationError renders errors independently so absent feedback costs no
// additional list-state branch.
function IntegrationError({ error }: Pick<IntegrationsListTabProps, "error">) {
  // Returning nothing preserves vertical spacing when no request failed.
  if (!error) {
    return null;
  }
  return <div className="mb-4 p-3 bg-red-50 border border-red-200 rounded-lg text-sm text-red-700">{error}</div>;
}

type IntegrationResultsProps = Pick<
  IntegrationsListTabProps,
  | "integrations"
  | "loading"
  | "searching"
  | "query"
  | "setShowNewPanel"
  | "viewType"
  | "isAuth"
  | "handleAddWorkspace"
  | "activeServiceIds"
  | "pendingServiceIds"
  | "hideEmptyState"
  | "page"
  | "totalPages"
  | "totalItems"
  | "onPageChange"
>;

// IntegrationResults selects one mutually exclusive list state before row
// rendering, preventing loading and empty-state rules from leaking into rows.
function IntegrationResults(props: IntegrationResultsProps) {
  // Loading and active searches intentionally share the same progress state.
  if (props.loading || props.searching) {
    return (
      <div className="flex flex-col items-center justify-center py-20 text-slate-400">
        <Loader2 className="w-8 h-8 text-blue-500 animate-spin mb-4" />
        <p className="animate-pulse font-medium text-slate-500">Loading services...</p>
      </div>
    );
  }
  // A combined workspace page suppresses its empty block because the catalogue and toggle provide the next action.
  if (props.integrations.length === 0 && props.hideEmptyState) {
    return null;
  }
  // Standalone empty results need guidance rather than an empty bordered collection.
  if (props.integrations.length === 0) {
    return <IntegrationEmptyState {...props} />;
  }
  return <IntegrationCollection {...props} />;
}

// IntegrationEmptyState gives searched, authenticated, and anonymous users
// different next actions without coupling those rules to loaded rows.
function IntegrationEmptyState({ query, isAuth, viewType, setShowNewPanel }: IntegrationResultsProps) {
  // A failed search offers definition because the requested service is known.
  if (query) {
    return (
      <div className="text-center py-16 text-slate-400">
        <p className="text-base font-medium text-slate-600 mb-1">Service not found</p>
        <p className="text-sm text-slate-400 mb-4">Define it from an OpenAPI or GraphQL spec, or point Fused to its docs.</p>
        <button data-track="submit_schema_or_docs_url" onClick={() => setShowNewPanel(true)} className="px-4 py-2 bg-blue-500 hover:bg-blue-600 text-white text-sm font-medium rounded-lg transition-colors cursor-pointer">
          Define this service
        </button>
      </div>
    );
  }
  // Authenticated users can define a service even when their current scope is empty.
  if (isAuth) {
    return (
      <div className="text-center py-16 text-slate-400">
        <p className="text-lg mb-2">{viewType === "workspace" ? "No services added yet" : "No services found"}</p>
        {viewType === "workspace" && <p className="text-sm text-slate-400 mb-3">Add one from the catalog, or define a service from a spec or docs.</p>}
        <button data-track="create_first_integration" onClick={() => setShowNewPanel(true)} className="text-blue-500 hover:text-blue-600 text-sm underline cursor-pointer">
          Define a service
        </button>
      </div>
    );
  }
  return <div className="text-center py-16 text-slate-400"><p className="text-sm text-slate-400">Search for any API service on Fused.</p></div>;
}

// IntegrationCollection renders responsive service cards and keeps pagination outside the grid.
function IntegrationCollection(props: IntegrationResultsProps) {
  return (
    <div>
      <div className="grid grid-cols-1 items-start gap-4 md:grid-cols-2 xl:grid-cols-3">
        {props.integrations.map((service) => (
          <IntegrationCard key={service.id} service={service} {...props} />
        ))}
      </div>
      <IntegrationPagination {...props} />
    </div>
  );
}

type IntegrationCardProps = IntegrationResultsProps & { service: ListableService };

// IntegrationCard presents identity, ownership, descriptive metadata, and actions without nesting buttons inside a link.
function IntegrationCard(props: IntegrationCardProps) {
  const { service, viewType } = props;
  const href = detailHref(service);
  // Provider attribution is meaningful only for a service owned by another Registry account.
  const providerName = service.is_owner === false ? service.provider?.name || service.provider?.handle : "";
  const apiURL = defaultAPIURL(service);
  return (
    <article className="group flex flex-col overflow-hidden rounded-2xl border border-slate-200/90 bg-white p-5 shadow-sm transition duration-200 hover:-translate-y-0.5 hover:border-black hover:shadow-lg hover:shadow-slate-200/70">
      <div className="flex items-start justify-between gap-3">
        <div className="flex min-w-0 items-center gap-3">
          <ServiceIcon name={service.name} iconURL={service.icon_url} />
          <div className="min-w-0">
            <Link
              to={href}
              target="_blank"
              rel="noopener noreferrer"
              onClick={(event) => openServiceLink(event, href)}
              className="block truncate text-sm font-semibold text-slate-900 hover:text-blue-700"
            >
              {formatServiceName(service.name)}
            </Link>
            {/* Owned services need no attribution; foreign services identify their actual provider. */}
            {providerName && <p className="mt-0.5 truncate text-xs text-slate-500">@{providerName}</p>}
            {/* The effective API target belongs to service identity rather than the analytics body. */}
            {apiURL && <p title={apiURL} className="mt-1 break-all font-mono text-[11px] leading-4 text-slate-400">{apiURL}</p>}
          </div>
        </div>
        <div className="flex shrink-0 items-center gap-1.5">
          {/* Visibility and exact local version remain independently readable. */}
          <ServiceVisibilityBadge isPublic={service.is_public} />
          {viewType === "workspace" && service.version && (
            <span className="px-2 py-0.5 bg-slate-100 text-slate-600 text-[10px] font-medium rounded-md">{formatVersion(service.version)}</span>
          )}
        </div>
      </div>
      <IntegrationDescription description={service.description} />
      <ServiceAnalytics endpointCount={service.endpoint_count} webhookCount={service.webhook_count} />
      <div className="pt-5">
        <div className="flex items-center justify-between gap-3 border-t border-slate-100 pt-4">
          <Link
            to={href}
            target="_blank"
            rel="noopener noreferrer"
            onClick={(event) => openServiceLink(event, href)}
            className="inline-flex items-center gap-1 text-xs font-semibold text-blue-700 transition-colors hover:text-blue-800"
          >
            View details <ArrowUpRight className="h-3.5 w-3.5" />
          </Link>
          <IntegrationActions {...props} />
        </div>
      </div>
    </article>
  );
}

/** Shows Registry visibility without presenting missing metadata as private. */
function ServiceVisibilityBadge({ isPublic }: { isPublic?: boolean | null }) {
  // An unavailable Registry projection is unknown, not proof of private visibility.
  if (typeof isPublic !== "boolean") return null;
  return isPublic
    ? <span className="rounded-full bg-blue-100 px-2 py-0.5 text-[10px] font-semibold tracking-wide text-blue-700">PUBLIC</span>
    : <span className="rounded-full bg-slate-100 px-2 py-0.5 text-[10px] font-semibold tracking-wide text-slate-600">PRIVATE</span>;
}

/** Renders provider-authored catalogue prose only when meaningful content exists. */
function IntegrationDescription({ description }: { description?: string }) {
  const content = description?.trim();
  // Missing descriptions leave the card quiet instead of inventing catalogue copy.
  if (!content) return null;
  return <p className="mt-4 whitespace-pre-wrap break-words text-sm leading-6 text-slate-600">{content}</p>;
}

/** Presents compact contract breadth without implying runtime usage analytics. */
function ServiceAnalytics({ endpointCount, webhookCount }: { endpointCount?: number; webhookCount?: number }) {
  const hasEndpointCount = typeof endpointCount === "number";
  const hasWebhookCount = typeof webhookCount === "number";
  // Registry outages leave workspace counts unknown, so absent values must not be presented as zero.
  if (!hasEndpointCount && !hasWebhookCount) return null;
  return (
    <div className="mt-4 flex overflow-hidden rounded-lg border border-slate-100 bg-slate-50/80 text-slate-600" aria-label="Service contract totals">
      {/* Each total remains independently optional when Registry projections degrade. */}
      {hasEndpointCount && <span className="flex-1 px-3 py-2"><strong className="block text-sm font-semibold text-slate-800">{endpointCount}</strong><span className="text-[10px] font-medium uppercase tracking-wide">{endpointCount === 1 ? "Endpoint" : "Endpoints"}</span></span>}
      {hasWebhookCount && <span className={`flex-1 px-3 py-2 ${hasEndpointCount ? "border-l border-slate-200/70" : ""}`}><strong className="block text-sm font-semibold text-slate-800">{webhookCount}</strong><span className="text-[10px] font-medium uppercase tracking-wide">{webhookCount === 1 ? "Webhook" : "Webhooks"}</span></span>}
    </div>
  );
}

// defaultAPIURL selects the declared default or production server before the service fallback.
function defaultAPIURL(service: ListableService): string {
  // Server declarations take precedence because they communicate the version's executable target.
  if (service.servers && service.servers.length > 0) {
    const explicitDefault = service.servers.find((server) => server.is_default);
    const production = service.servers.find((server) => isProductionServer(server.description));
    return (explicitDefault || production || service.servers[0]).url;
  }
  // Service base URLs preserve the Registry's effective execution fallback when servers are absent.
  return service.base_url ?? "";
}

// isProductionServer centralizes the description heuristic used to pick a
// primary server without duplicating case normalization.
function isProductionServer(description?: string): boolean {
  const normalized = description?.toLowerCase() ?? "";
  return normalized.includes("prod") || normalized.includes("production");
}

// IntegrationActions chooses the one action allowed by the current view and
// service activation state.
function IntegrationActions(props: IntegrationCardProps) {
  // Workspace removal is intentionally reserved for the service detail page where impact is visible.
  if (props.viewType === "workspace") {
    return null;
  }
  // Catalog additions are offered only to authenticated users for inactive services.
  if (canAddCatalogIntegration(props)) {
    const pending = Boolean(props.pendingServiceIds?.includes(props.service.id));
    return (
      <button
        data-track="add_workspace_service"
        onClick={(event) => props.handleAddWorkspace?.(event, props.service.id, props.service.name)}
        disabled={pending}
        aria-busy={pending}
        className="inline-flex items-center gap-1.5 rounded-lg bg-slate-900 px-3 py-1 text-xs font-medium text-white shadow-sm transition-all hover:bg-slate-700 disabled:cursor-wait disabled:opacity-70"
        title="Add to workspace"
      >
        {/* A visible spinner makes the single admitted activation request explicit and discourages retries. */}
        {pending && <Loader2 className="h-3.5 w-3.5 animate-spin" />}
        {pending ? "Adding…" : "Add to workspace"}
      </button>
    );
  }
  // An explicit marker explains why an authenticated catalogue card has no add button.
  if (props.isAuth && props.activeServiceIds?.includes(props.service.id)) {
    return <span className="inline-flex items-center gap-1 text-xs font-medium text-emerald-700"><Check className="h-3.5 w-3.5" /> In workspace</span>;
  }
  return null;
}

// canAddCatalogIntegration prevents duplicate workspace activation and avoids
// presenting an action when its authenticated mutation handler is unavailable.
function canAddCatalogIntegration(props: IntegrationCardProps): boolean {
  return Boolean(props.isAuth && props.handleAddWorkspace && !props.activeServiceIds?.includes(props.service.id));
}

// IntegrationPagination hides pagination during filtered/loading states and
// otherwise keeps page controls independent of list rows.
function IntegrationPagination({ loading, query, totalPages, totalItems, page, onPageChange }: IntegrationResultsProps) {
  // Search results and incomplete loads do not represent the unfiltered page count.
  if (loading || query || totalPages <= 0) {
    return null;
  }
  return (
    <div className="mt-6 flex items-center justify-between gap-3 border-t border-slate-100 px-1 py-3">
      <p className="text-xs text-slate-500">{integrationPageRange(totalItems, page)}</p>
      <div className="flex items-center gap-1">
        <button type="button" data-track="paginate_previous" onClick={() => onPageChange(page - 1)} disabled={page === 1} className="rounded-md p-1.5 text-slate-500 hover:bg-slate-100 disabled:opacity-40 disabled:hover:bg-transparent" aria-label="Previous page" title="Previous">
          <ChevronLeft className="w-4 h-4" />
        </button>
        <span className="text-xs text-slate-500 pl-2">Page</span>
        <select className="bg-white border border-slate-200 rounded px-2 py-1 text-xs focus:outline-none focus:ring-2 focus:ring-blue-500 mx-1 cursor-pointer" value={page} onChange={(event) => onPageChange(parseInt(event.target.value, 10))}>
          {Array.from({ length: totalPages }, (_, index) => index + 1).map((pageNumber) => <option key={pageNumber} value={pageNumber}>{pageNumber}</option>)}
        </select>
        <span className="text-xs font-medium text-slate-500 pr-2">of {totalPages}</span>
        <button type="button" data-track="paginate_next" onClick={() => onPageChange(page + 1)} disabled={page >= totalPages} className="rounded-md p-1.5 text-slate-500 hover:bg-slate-100 disabled:opacity-40 disabled:hover:bg-transparent" aria-label="Next page" title="Next">
          <ChevronRight className="w-4 h-4" />
        </button>
      </div>
    </div>
  );
}

// integrationPageRange formats the server's fixed ten-item pagination window
// and stays empty when the response omitted a total.
function integrationPageRange(totalItems: number | undefined, page: number): string {
  // The API may omit totals, in which case a guessed range would be misleading.
  if (totalItems === undefined) {
    return "";
  }
  const first = totalItems === 0 ? 0 : (page - 1) * 10 + 1;
  return `${first}-${Math.min(totalItems, page * 10)} of ${totalItems}`;
}
