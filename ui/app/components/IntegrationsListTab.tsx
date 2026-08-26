import { FormEvent } from "react";
import { Link } from "@remix-run/react";
import { Loader2, Search, X , ChevronLeft, ChevronRight } from "lucide-react";
import { Service, ActivatedService, serviceHref } from "~/lib/api";
import { formatServiceName, formatVersion } from "~/lib/format";
import { openServiceLink } from "~/lib/service-navigation";

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
  is_owner?: boolean;
  is_public?: boolean;
  base_url?: string;
  servers?: { url: string; description?: string }[];
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
    base_url: s.base_url,
    servers: s.servers,
  };
}

/** Normalise a workspace ActivatedService into a ListableService. */
export function fromActivatedService(s: ActivatedService): ListableService {
  return {
    id: s.service_id, // Use the Registry service_id as the stable ID
    name: s.service_name,
    slug: s.service_slug,
    service_id: s.service_id,
    service_slug: s.service_slug,
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
  handleDelete: (e: React.MouseEvent, id: string) => void;
  setShowNewPanel: (show: boolean) => void;
  page: number;
  totalPages: number;
  totalItems?: number;
  onPageChange: (page: number) => void;
  viewType?: "workspace" | "catalog";
  handleAddWorkspace?: (e: React.MouseEvent, id: string, name: string) => void;
  handleRemoveWorkspace?: (e: React.MouseEvent, id: string) => void;
  activeServiceIds?: string[];
  isAuth?: boolean;
}

export default function IntegrationsListTab({
  integrations,
  loading,
  error,
  query,
  setQuery,
  handleSearch,
  handleClear,
  searching,
  handleDelete,
  setShowNewPanel,
  page,
  totalPages,
  totalItems,
  onPageChange,
  isAuth,
  viewType = "catalog",
  handleAddWorkspace,
  handleRemoveWorkspace,
  activeServiceIds = [],
}: IntegrationsListTabProps) {
  return (
    <>
      <IntegrationSearch
        query={query}
        setQuery={setQuery}
        handleSearch={handleSearch}
        handleClear={handleClear}
        searching={searching}
        isAuth={isAuth}
        viewType={viewType}
      />
      <IntegrationError error={error} />
      <IntegrationResults
        integrations={integrations}
        loading={loading}
        searching={searching}
        query={query}
        setShowNewPanel={setShowNewPanel}
        viewType={viewType}
        isAuth={isAuth}
        handleDelete={handleDelete}
        handleAddWorkspace={handleAddWorkspace}
        handleRemoveWorkspace={handleRemoveWorkspace}
        activeServiceIds={activeServiceIds}
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
>;

// IntegrationSearch isolates search-state decisions from list rendering so
// asynchronous feedback cannot make the row component harder to reason about.
function IntegrationSearch({ query, setQuery, handleSearch, handleClear, searching, isAuth, viewType }: IntegrationSearchProps) {
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
        placeholder={integrationSearchPlaceholder(isAuth, viewType)}
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
  | "handleDelete"
  | "handleAddWorkspace"
  | "handleRemoveWorkspace"
  | "activeServiceIds"
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
  // Empty results need guidance rather than an empty bordered collection.
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

// IntegrationCollection renders loaded rows and delegates pagination policy to
// a separate footer so row actions remain independent of page state.
function IntegrationCollection(props: IntegrationResultsProps) {
  return (
    <div className="bg-white border-y border-slate-200 divide-y divide-slate-100">
      {props.integrations.map((service) => (
        <IntegrationRow key={service.id} service={service} {...props} />
      ))}
      <IntegrationPagination {...props} />
    </div>
  );
}

type IntegrationRowProps = IntegrationResultsProps & { service: ListableService };

// IntegrationRow owns only one service's presentation and delegates action
// policy; the shared link handler honors cancellation from catalog/workspace actions.
function IntegrationRow(props: IntegrationRowProps) {
  const { service, viewType } = props;
  const href = detailHref(service);
  return (
    <Link
      to={href}
      target="_blank"
      rel="noopener noreferrer"
      onClick={(event) => openServiceLink(event, href)}
      className="flex items-center justify-between px-5 py-4 hover:bg-slate-50 transition-colors group"
    >
      <div>
        <div className="flex items-center gap-2">
          <p className="text-sm font-medium text-slate-900">{formatServiceName(service.name)}</p>
          {service.is_public && <span className="px-2 py-0.5 bg-blue-100 text-blue-700 text-[10px] font-semibold rounded-full tracking-wide">PUBLIC</span>}
          {viewType === "workspace" && service.version && (
            <span className="px-2 py-0.5 bg-slate-100 text-slate-600 text-[10px] font-medium rounded-md">{formatVersion(service.version)}</span>
          )}
        </div>
        <p className="text-xs text-slate-400 mt-0.5 truncate max-w-xs">{integrationSummary(service, viewType)}</p>
      </div>
      <IntegrationActions {...props} />
    </Link>
  );
}

// integrationSummary selects the provider's preferred production server and
// falls back to stable workspace copy when no server metadata exists.
function integrationSummary(service: ListableService, viewType: "workspace" | "catalog"): string {
  // Server declarations take precedence because they communicate environment choice.
  if (service.servers && service.servers.length > 0) {
    const productionIndex = service.servers.findIndex((server) => isProductionServer(server.description));
    const primary = productionIndex >= 0 ? service.servers[productionIndex] : service.servers[0];
    // Multiple servers need context that a bare primary URL would hide.
    if (service.servers.length > 1) {
      return `${service.servers.length} Environments (Primary: ${primary.url})`;
    }
    return primary.url;
  }
  // A catalog row without a URL stays visually quiet, while workspace rows remain identifiable.
  return service.base_url ?? (viewType === "workspace" ? "Connected Workspace Service" : "");
}

// isProductionServer centralizes the description heuristic used to pick a
// primary server without duplicating case normalization.
function isProductionServer(description?: string): boolean {
  const normalized = description?.toLowerCase() ?? "";
  return normalized.includes("prod") || normalized.includes("production");
}

// IntegrationActions chooses the one action allowed by the current view and
// service activation state.
function IntegrationActions(props: IntegrationRowProps) {
  // Workspace rows can be deleted by owners or merely removed by consumers.
  if (props.viewType === "workspace") {
    return <WorkspaceIntegrationAction {...props} />;
  }
  // Catalog additions are offered only to authenticated users for inactive services.
  if (canAddCatalogIntegration(props)) {
    return (
      <button
        data-track="add_workspace_service"
        onClick={(event) => props.handleAddWorkspace?.(event, props.service.id, props.service.name)}
        className="px-3 py-1 text-xs font-medium text-blue-700 bg-blue-50 hover:bg-blue-100 rounded-lg shadow-sm transition-all"
        title="Add to workspace"
      >
        Add to workspace
      </button>
    );
  }
  return null;
}

// canAddCatalogIntegration prevents duplicate workspace activation and avoids
// presenting an action when its authenticated mutation handler is unavailable.
function canAddCatalogIntegration(props: IntegrationRowProps): boolean {
  return Boolean(props.isAuth && props.handleAddWorkspace && !props.activeServiceIds?.includes(props.service.id));
}

// WorkspaceIntegrationAction labels and styles destructive ownership changes
// separately from reversible workspace removal.
function WorkspaceIntegrationAction(props: IntegrationRowProps) {
  const owned = Boolean(props.service.is_owner);
  return (
    <button
      data-track={owned ? "delete_workspace_service" : "remove_workspace_service"}
      onClick={(event) => mutateWorkspaceIntegration(event, props)}
      className={`opacity-0 group-hover:opacity-100 px-3 py-1 text-xs font-medium bg-white border rounded-lg shadow-sm transition-all ${owned ? "text-red-600 border-red-200 hover:bg-red-50" : "text-slate-600 hover:text-red-600 border-slate-200 hover:border-red-200 hover:bg-red-50"}`}
      title={owned ? "Delete service from Registry" : "Remove from workspace"}
    >
      {owned ? "Delete" : "Remove"}
    </button>
  );
}

// mutateWorkspaceIntegration sends owner and consumer rows to their distinct
// mutation handlers while preserving the service's Registry identity.
function mutateWorkspaceIntegration(event: React.MouseEvent, props: IntegrationRowProps): void {
  const serviceID = props.service.service_id || props.service.id;
  // Owners mutate the Registry definition rather than just workspace membership.
  if (props.service.is_owner) {
    props.handleDelete(event, serviceID);
    return;
  }
  // Consumer rows are removable only when the workspace handler is available.
  if (props.handleRemoveWorkspace) {
    props.handleRemoveWorkspace(event, serviceID);
  }
}

// IntegrationPagination hides pagination during filtered/loading states and
// otherwise keeps page controls independent of list rows.
function IntegrationPagination({ loading, query, totalPages, totalItems, page, onPageChange }: IntegrationResultsProps) {
  // Search results and incomplete loads do not represent the unfiltered page count.
  if (loading || query || totalPages <= 0) {
    return null;
  }
  return (
    <div className="flex items-center justify-between gap-3 border-t border-slate-100 px-5 py-3 bg-white">
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
