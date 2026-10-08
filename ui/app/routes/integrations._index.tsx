import { useState, useEffect, useRef, useCallback, type ComponentProps, type FormEvent } from "react";
import { useNavigate, useSearchParams, useRouteLoaderData, type MetaFunction } from "@remix-run/react";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m) => m.id === "root").flatMap((m) => m.meta ?? []);
  return [
    ...parentMeta.filter((m) => !('title' in m)),
    { title: "Services - Fused" },
  ];
};
import { api, type Service, type SpecificationImportPlan, type ActivatedService, type DiscoverySnapshot } from "~/lib/api";
import ExtractionWizard from "~/components/ExtractionWizard";
import IntegrationsListTab, { fromService, fromActivatedService } from "~/components/IntegrationsListTab";
import IntegrationsPendingTab from "~/components/IntegrationsPendingTab";
import { DefineServiceDrawer } from "~/components/DefineServiceDrawer";
import { useToast } from "~/components/Toast";
import { isImportVersionRequired } from "~/lib/authorization-error";
import {
  closeDiscoverySessionQuery,
  discoveryNavigationFromQuery,
  openDiscoverySessionQuery,
} from "~/lib/discovery-navigation";

type ImportSource = { url?: string; content?: string };
type ImportIdentity = { name: string; slug?: string; version?: string };
type ServicesView = "workspace" | "pending";
type CatalogPage = {
  data: Service[];
  total: number;
  page: number;
  limit: number;
};
type CatalogLoadOptions = { knownWorkspaceServiceIds?: string[]; query?: string };

const CATALOG_SEARCH_QUERY = `
  query($q: String!) {
    searchServices(q: $q, publicOnly: true) {
      id name description icon_url endpoint_count webhook_count base_url servers { url description }
      is_public is_owner slug provider { name handle } canonical_ref
    }
  }
`;

// searchCatalogServices keeps the global catalog public-only even for owners,
// whose private services remain available through Workspace services.
async function searchCatalogServices(q: string): Promise<Service[]> {
  const response = await api.graphql<{ searchServices: Service[] | null }>(CATALOG_SEARCH_QUERY, { q });
  return response.searchServices || [];
}

// fetchCatalogPage describes one bounded browse or search result as an honest UI page.
async function fetchCatalogPage(query: string = ""): Promise<CatalogPage> {
  const data = await searchCatalogServices(query);
  // Registry search is deliberately capped in SQL, so advertising a second
  // page would promise a pagination contract this query does not have.
  return { data, total: data.length, page: 1, limit: Math.max(data.length, 1) };
}

function importSource(method: "openapi" | "docs", sourceType: "url" | "text", url: string, content: string): ImportSource {
  if (method === "openapi" && sourceType === "text") {
    return { content: content.trim() };
  }
  return { url: url.trim() };
}

function canStartImport(method: "openapi" | "docs", sourceType: "url" | "text", name: string, version: string, source: ImportSource, requireVersion: boolean): boolean {
  if (!name.trim()) return false;
  if ((requireVersion || method === "docs") && !version.trim()) return false;
  return sourceType === "text" ? Boolean(source.content) : Boolean(source.url);
}

// createSpecificationPlan lets source-aware Registry validation reveal fields
// that cannot be inferred reliably in the browser.
async function createSpecificationPlan(
  source: ImportSource,
  identity: ImportIdentity,
  setRequireVersion: (required: boolean) => void,
) {
  try {
    return await api.integrations.planImport({
      ...identity,
      source_url: source.url,
      source_content: source.content,
    });
  } catch (error: unknown) {
    // Only the Registry parser can reliably determine whether each supported
    // specification format declares a version.
    if (isImportVersionRequired(error)) setRequireVersion(true);
    throw error;
  }
}

// selectedServicesView converts an untrusted URL tab into one supported view.
function selectedServicesView(tab: string | null): ServicesView {
  // Imports retain a separate progress view; legacy catalogue links now resolve to the combined workspace surface.
  if (tab === "pending") return tab;
  return "workspace";
}

// workspaceListProjection derives stable empty-state and pagination values from one optional page response.
function workspaceListProjection(page: { data: ActivatedService[]; total: number } | null) {
  const data = page?.data ?? [];
  const total = page?.total ?? 0;
  return { integrations: data.map(fromActivatedService), total, pages: Math.ceil(total / 10) || 1 };
}

// ServicesTabs renders authenticated navigation without coupling its decisions to page state management.
function ServicesTabs({ isAuth, view, activeSessions, setView }: {
  isAuth: boolean;
  view: ServicesView;
  activeSessions: DiscoverySnapshot[];
  setView: (view: ServicesView) => void;
}) {
  if (!isAuth) return null;
  return (
    <div className="flex bg-slate-100 p-1 rounded-lg w-full sm:w-fit mb-6">
      <button data-track="view_workspace_tab" type="button" onClick={() => setView("workspace")}
        className={`flex-1 sm:flex-none px-3 sm:px-4 py-1.5 text-sm font-medium rounded-md transition-all ${view === "workspace" ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700 hover:bg-slate-200/50"} cursor-pointer`}>
        Workspace
      </button>
      <button data-track="view_imports_tab" type="button" onClick={() => setView("pending")}
        className={`relative flex-1 sm:flex-none px-3 sm:px-4 py-1.5 text-sm font-medium rounded-md transition-all ${view === "pending" ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700 hover:bg-slate-200/50"} cursor-pointer`}>
        Imports
        {activeSessions.length > 0 && <span className="ml-2 text-[10px] font-bold text-amber-700">{activeSessions.length}</span>}
      </button>
    </div>
  );
}

// EmptyWorkspaceCatalogIntro explains why catalogue discovery is expanded for a new workspace.
function EmptyWorkspaceCatalogIntro({ visible }: { visible: boolean }) {
  // Established workspaces do not need onboarding copy above their optional catalogue collection.
  if (!visible) return null;
  return (
    <div className="mb-6 rounded-xl border border-blue-100 bg-blue-50/70 px-5 py-4">
      <p className="text-sm font-semibold text-slate-900">Start with the service catalog</p>
      <p className="mt-1 text-sm text-slate-600">Add a service to make its operations and versions available to this workspace.</p>
    </div>
  );
}

// catalogVisible always includes Registry discovery in search; the toggle controls browse mode only.
function catalogVisible(isAuth: boolean, preference: boolean | null, serviceIDs: string[] | null, query: string = ""): boolean {
  // Public visitors have only the catalogue surface available.
  if (!isAuth) return true;
  // Search must discover services available to add even when catalogue browsing is collapsed.
  if (query.trim()) return true;
  // Clearing search restores the user's browse preference instead of permanently expanding discovery.
  if (preference !== null) return preference;
  return serviceIDs !== null && serviceIDs.length === 0;
}

// workspaceIsEmpty distinguishes an authoritative empty membership response from the initial unknown state.
function workspaceIsEmpty(isAuth: boolean, serviceIDs: string[] | null): boolean {
  return Boolean(isAuth && serviceIDs !== null && serviceIDs.length === 0);
}

// catalogErrorForAuth prevents the combined authenticated page from rendering the same request error twice.
function catalogErrorForAuth(isAuth: boolean, error: string): string {
  // Public visitors have only the catalogue collection, so it owns request feedback there.
  return isAuth ? "" : error;
}

// PendingImports renders active extraction work only in its authenticated view.
function PendingImports({ isAuth, view, props }: {
  isAuth: boolean;
  view: ServicesView;
  props: ComponentProps<typeof IntegrationsPendingTab>;
}) {
  if (!isAuth || view !== "pending") return null;
  return <div className="mb-6"><IntegrationsPendingTab {...props} /></div>;
}

// CatalogToggle keeps discovery subordinate to Workspace instead of presenting it as another navigation destination.
function CatalogToggle({ checked, onChange }: { checked: boolean; onChange: () => void }) {
  return (
    <div className="mb-6 flex items-center justify-between gap-5 rounded-xl border border-slate-200 bg-slate-50/70 px-4 py-3">
      <div>
        <p className="text-sm font-semibold text-slate-900">Show catalog</p>
        <p className="mt-1 text-xs text-slate-500">Browse public services beneath your workspace services.</p>
      </div>
      {/* The switch's color and thumb position share the accessible checked state. */}
      <button type="button" role="switch" aria-checked={checked} onClick={onChange} data-track="toggle_service_catalogue"
        className={`relative h-6 w-11 shrink-0 rounded-full transition-colors ${checked ? "bg-blue-600" : "bg-slate-300"}`}>
        <span className={`absolute left-0.5 top-0.5 h-5 w-5 rounded-full bg-white shadow-sm transition-transform ${checked ? "translate-x-5" : "translate-x-0"}`} />
      </button>
    </div>
  );
}

// combinedSearchPage fills a single ten-row page with workspace matches followed by Registry-only matches.
function combinedSearchPage(workspace: ComponentProps<typeof IntegrationsListTab>, catalog: ComponentProps<typeof IntegrationsListTab>) {
  const offset = (workspace.page - 1) * 10;
  const workspaceTotal = workspace.totalItems ?? 0;
  const seen = new Set(workspace.integrations.map(service => service.id));
  // A Registry identity can appear only once, even if a response repeats a match.
  const available = catalog.integrations.filter(service => {
    if (seen.has(service.id)) return false;
    seen.add(service.id);
    return true;
  });
  const catalogOffset = Math.max(0, offset - workspaceTotal);
  const totalItems = workspaceTotal + available.length;
  return {
    integrations: [...workspace.integrations, ...available.slice(catalogOffset, catalogOffset + Math.max(0, 10 - workspace.integrations.length))],
    totalItems,
    totalPages: Math.max(1, Math.ceil(totalItems / 10)),
  };
}

// ServicesContent uses one result list during search while preserving optional catalogue browsing.
function ServicesContent({ isAuth, view, showCatalog, emptyWorkspace, onToggleCatalog, catalog, workspace }: {
  isAuth: boolean;
  view: ServicesView;
  showCatalog: boolean;
  emptyWorkspace: boolean;
  onToggleCatalog: () => void;
  catalog: ComponentProps<typeof IntegrationsListTab>;
  workspace: ComponentProps<typeof IntegrationsListTab>;
}) {
  // Anonymous visitors have no local workspace projection to place above the catalogue.
  if (!isAuth) return <IntegrationsListTab {...catalog} />;
  // Import progress owns the content area until the user returns to Workspace.
  if (view !== "workspace") return null;
  const isSearch = Boolean(workspace.query.trim());
  // One list owns loading, pagination and empty feedback across both search sources.
  if (isSearch) return <IntegrationsListTab {...workspace} {...combinedSearchPage(workspace, catalog)}
    viewType="search" loading={workspace.loading || catalog.loading} hideEmptyState={false}
    activeServiceIds={catalog.activeServiceIds} pendingServiceIds={catalog.pendingServiceIds}
    searchPlaceholder="Search workspace and catalog" />;
  return (
    <>
      {/* Search always includes both sources; the optional toggle applies only to browsing. */}
      {!isSearch && <CatalogToggle checked={showCatalog} onChange={onToggleCatalog} />}
      {/* Imported and activated services remain the first collection on the page. */}
      {!emptyWorkspace && <h2 className="mb-4 text-sm font-semibold text-slate-900">Workspace services</h2>}
      {/* A search with no matches needs visible feedback; only a new workspace defers its empty state to the catalogue. */}
      <IntegrationsListTab {...workspace} hideEmptyState={emptyWorkspace && !workspace.query.trim()} searchPlaceholder="Search workspace and catalog" />
      {/* Catalogue cards are appended without changing the active Workspace navigation state. */}
      {showCatalog && <section className="mt-6">
        <EmptyWorkspaceCatalogIntro visible={emptyWorkspace} />
        {!emptyWorkspace && <div className="mb-4"><h2 className="text-sm font-semibold text-slate-900">Service catalog</h2><p className="mt-1 text-xs text-slate-500">Add public services without displacing what is already in your workspace.</p></div>}
        {/* Membership loads before catalogue results; avoid flashing a false empty state during that first request. */}
        <IntegrationsListTab {...catalog} loading={catalog.loading || workspace.loading} showSearch={false} />
      </section>}
    </>
  );
}

// DefineServicePanel keeps drawer visibility out of the route's orchestration complexity.
function DefineServicePanel({ visible, props }: {
  visible: boolean;
  props: ComponentProps<typeof DefineServiceDrawer>;
}) {
  if (!visible) return null;
  return <DefineServiceDrawer {...props} />;
}

// ExtractionSessionPanel renders one active wizard and leaves its lifecycle callbacks with the route owner.
function ExtractionSessionPanel({ sessionID, reviewOnly, onClose, onComplete }: {
  sessionID: string | null;
  reviewOnly: boolean;
  onClose: () => void;
  onComplete: () => void;
}) {
  if (!sessionID) return null;
  return <ExtractionWizard sessionId={sessionID} reviewOnly={reviewOnly} onClose={onClose} onComplete={onComplete} />;
}

// IntegrationsIndex coordinates the combined Workspace surface and its separate import-progress view.
export default function IntegrationsIndex() {
  const toast = useToast();
  const rootData = useRouteLoaderData<{ isAuth: boolean }>("root");
  const isAuth = rootData?.isAuth ?? false;
  // View state driven by URL search parameter "tab"
  const [searchParams, setSearchParams] = useSearchParams();

  const [integrations, setIntegrations] = useState<Service[]>([]);
  const [activeWorkspaceServiceIds, setActiveWorkspaceServiceIds] = useState<string[] | null>(null);
  const pendingWorkspaceServiceIdsRef = useRef<Set<string>>(new Set());
  const [pendingWorkspaceServiceIds, setPendingWorkspaceServiceIds] = useState<string[]>([]);
  const [workspaceServicePageData, setWorkspaceServicePageData] = useState<{ data: ActivatedService[]; total: number } | null>(null);
  const [workspaceLoading, setWorkspaceLoading] = useState(isAuth && !searchParams.get("q"));
  const [catalogLoading, setCatalogLoading] = useState(!isAuth && !searchParams.get("q"));
  const [catalogPreference, setCatalogPreference] = useState<boolean | null>(null);
  const [error, setError] = useState("");
  const [query, setQuery] = useState(searchParams.get("q") ?? "");
  const [searching, setSearching] = useState(false);
  const activeSearch = searchParams.get("q")?.trim() ?? "";
  const workspaceRequest = useRef(0);
  const catalogRequest = useRef(0);
  const [activeSessions, setActiveSessions] = useState<DiscoverySnapshot[]>([]);
  const [totalPages, setTotalPages] = useState(1);
  const [totalItems, setTotalItems] = useState(0);
  const navigate = useNavigate();
  const urlTab = searchParams.get("tab");
  const view = selectedServicesView(urlTab);
  const discoveryNavigation = discoveryNavigationFromQuery(searchParams);
  const activeDiscoverySessionID = discoveryNavigation.sessionID;

    // Pagination
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

  const setView = (newTab: ServicesView) => {
    setSearchParams(prev => {
      prev.set("tab", newTab);
      return prev;
    }, { replace: true });
  };

  // Side panel states
  const [showNewPanel, setShowNewPanel] = useState(false);
  const [newName, setNewName] = useState("");
  const [newSlug, setNewSlug] = useState("");
  const [isSlugManuallyEdited, setIsSlugManuallyEdited] = useState(false);
  const [newVersion, setNewVersion] = useState("");
  const [requireVersion, setRequireVersion] = useState(false);
  const [importMethod, setImportMethod] = useState<"openapi" | "docs">("openapi");
  const [sourceType, setSourceType] = useState<"url" | "text">("url");
  const [newUrl, setNewUrl] = useState("");
  const [newContent, setNewContent] = useState("");
  const [fileName, setFileName] = useState("");
  const [starting, setStarting] = useState(false);
  const [startError, setStartError] = useState("");

  const [importPlan, setImportPlan] = useState<SpecificationImportPlan | null>(null);

  // openUIDiscoverySession records resumable UI work in the URL and strips any prior CLI authority marker.
  function openUIDiscoverySession(sessionID: string) {
    setSearchParams((current) => openDiscoverySessionQuery(current, sessionID), { replace: true });
  }

  // closeDiscoverySession removes both the opaque identity and its handoff origin while preserving the active tab.
  function closeDiscoverySession() {
    setSearchParams((current) => closeDiscoverySessionQuery(current), { replace: true });
  }

  // loadCatalogData normalizes authenticated and public catalogue pages for UI state.
  const loadCatalogData = useCallback(async (options: CatalogLoadOptions = {}) => {
    const request = ++catalogRequest.current;
    setCatalogLoading(true);
    try {
      const [catalogPage, workspaceServiceIds] = await Promise.all([
        fetchCatalogPage(options.query ?? activeSearch),
        // Workspace loading already knows membership, so reuse it instead of repeating the Engine query.
        options.knownWorkspaceServiceIds !== undefined
          ? Promise.resolve(options.knownWorkspaceServiceIds)
          : isAuth ? api.workspace.getServiceIds() : Promise.resolve([]),
      ]);
      // An older search must not replace results from a newer query.
      if (request !== catalogRequest.current) return;
      // Existing memberships come from the workspace search; retain newly added cards until the next search refresh.
      const membership = new Set(workspaceServiceIds);
      const catalogMatches = (options.query ?? activeSearch).trim()
        ? catalogPage.data.filter(service => !membership.has(service.id))
        : catalogPage.data;
      setIntegrations(catalogMatches);
      setActiveWorkspaceServiceIds(workspaceServiceIds);
      setTotalPages(Math.ceil(catalogPage.total / catalogPage.limit) || 1);
      setTotalItems(catalogPage.total);

    } catch (e) {
      // Ignore errors from superseded searches.
      if (request !== catalogRequest.current) return;
      setError(e instanceof Error ? e.message : "Failed to load services");
    } finally {
      // Only the latest request owns the visible loading state.
      if (request === catalogRequest.current) setCatalogLoading(false);
    }
  }, [isAuth, activeSearch]);

  // loadWorkspaceData keeps local services first and optionally loads catalogue cards beneath them.
  const loadWorkspaceData = useCallback(async (p: number = page, searchQuery: string = activeSearch) => {
    const request = ++workspaceRequest.current;
    // Invalidate catalog results tied to the previous workspace search.
    ++catalogRequest.current;
    setWorkspaceLoading(true);
    setError("");
    try {
      const [wsPageRes, workspaceServiceIds] = await Promise.all([
        isAuth ? api.workspace.getServicesPage(10, (p - 1) * 10, undefined, searchQuery) : null,
        isAuth ? api.workspace.getServiceIds() : Promise.resolve([]),
      ]);
      // Paging and fast typing can complete out of order; only the newest request may publish.
      if (request !== workspaceRequest.current) return;
      setActiveWorkspaceServiceIds(workspaceServiceIds);
      // The workspace tab renders only the requested page, so it must not issue a second unpaginated membership projection.
      setWorkspaceServicePageData(wsPageRes);
      // Search includes catalogue matches regardless of the separate browse preference.
      if (catalogVisible(isAuth, catalogPreference, workspaceServiceIds, searchQuery)) {
        await loadCatalogData({ knownWorkspaceServiceIds: workspaceServiceIds, query: searchQuery });
      }
    } catch (e) {
      // Superseded failures must not mask the latest successful result.
      if (request !== workspaceRequest.current) return;
      setError(e instanceof Error ? e.message : "Failed to load services");
    } finally {
      // Earlier requests cannot clear the latest search spinner.
      if (request === workspaceRequest.current) setWorkspaceLoading(false);
    }
  }, [page, isAuth, activeSearch, catalogPreference, loadCatalogData]);

  // loadVisibleData routes URL-backed search and paging to the visible collection.
  const loadVisibleData = useCallback(async (p: number = page) => {
    // Public browsing has no workspace collection to request first.
    if (!isAuth) {
      await loadCatalogData();
      return;
    }

    // Workspace owns both local results and the optional catalogue below them.
    if (view === "workspace") {
      await loadWorkspaceData(p);
      return;
    }

    // Import progress does not participate in service search.
    if (view === "pending") refreshSessions();
  }, [view, isAuth, page, loadCatalogData, loadWorkspaceData]);

  // URL changes are the single trigger for search, browser history, and pagination loads.
  useEffect(() => {
    void loadVisibleData(page);
    // Navigation invalidates in-flight results without discarding the last visible page.
    return () => { ++workspaceRequest.current; ++catalogRequest.current; };
  }, [page, loadVisibleData]);

  // Back/forward navigation must restore the search field as well as its result set.
  useEffect(() => { setQuery(activeSearch); }, [activeSearch]);

  // refreshSessions reads only authoritative version-one snapshots for the pending view.
  const refreshSessions = () => {
    api.integrations.getActiveDiscoverySessions()
      .then((sessions) => setActiveSessions(sessions || []))
      .catch((err) => console.error("Failed to load active sessions:", err));
  };

  useEffect(() => {
    if (view === "pending") {
      refreshSessions();
    }
  }, [view]);



  // runSearch commits text and resets pagination together; the URL-driven loader fetches the resulting page.
  function runSearch(q: string) {
    const normalized = q.trim();
    setSearchParams(prev => {
      const next = new URLSearchParams(prev);
      // Empty text restores browse mode, whether removed by typing or the clear button.
      if (normalized) next.set("q", normalized);
      else next.delete("q");
      next.set("page", "1");
      return next;
    }, { replace: true });
    setSearching(false);
  }

  // Debounce only uncommitted text; initial links and pagination are handled by the URL-driven loader.
  useEffect(() => {
    // Matching URL text has already been submitted, including a cleared search.
    if (query.trim() === activeSearch) { setSearching(false); return; }
    setSearching(true);
    const id = setTimeout(() => runSearch(query), 400);
    return () => clearTimeout(id);
  }, [query, activeSearch]);

  // Explicit submission applies the current text immediately, including empty text.
  function handleSearch(e?: FormEvent) {
    // Tool callers may submit without a browser event.
    if (e) e.preventDefault();
    runSearch(query);
  }

  // Clearing synchronizes the field and URL so stale results cannot survive an empty query.
  function handleClear() {
    setQuery("");
    runSearch("");
  }

  async function handleStart(e: FormEvent) {
    e.preventDefault();
    if (starting) return;
    const source = importSource(importMethod, sourceType, newUrl, newContent);
    if (!canStartImport(importMethod, sourceType, newName, newVersion, source, requireVersion)) return;
    setStarting(true);
    setStartError("");
    try {
      if (importMethod === "openapi") {
        await handleSpecificationImport(source);
      } else {
        await handleDocsImport(source);
      }
    } catch (e: unknown) {
      setStartError(e instanceof Error ? e.message : "Failed to start");
    } finally {
      setStarting(false);
    }
  }

  async function handleSpecificationImport(source: ImportSource) {
    if (!importPlan) {
      const plan = await createSpecificationPlan(source, {
        name: newName.trim(),
        slug: newSlug.trim() || undefined,
        version: newVersion.trim() || undefined,
      }, setRequireVersion);
      // The Registry parser is authoritative for declared source versions;
      // reflecting its result avoids presenting a fallback and parsed version
      // as two competing values.
      setNewVersion(plan.target_version);
      setRequireVersion(false);
      setImportPlan(plan);
      return;
    }
    const applied = await api.integrations.applyImport(importPlan.plan_id, importPlan.review_hash);
    setShowNewPanel(false);
    setImportPlan(null);
    navigate(`/integrations/${applied.service_id}`);
  }

  function handleChangeImportSource() {
    // A resolved version belongs to the planned source, so retaining it while
    // editing another source could silently apply stale metadata on retry.
    setImportPlan(null);
    setNewVersion("");
    setRequireVersion(false);
    setStartError("");
  }

  // handleDocsImport gives a documentation URL the deterministic spec-first path before bounded crawling.
  async function handleDocsImport(source: ImportSource) {
    const res = await api.integrations.startDiscovery({
      name: newName.trim(),
      slug: newSlug.trim(),
      version: newVersion.trim(),
      source_url: source.url || "",
      source_mode: "auto",
      requested_workers: 0,
      crawl: { max_pages: 0, max_depth: 0 },
    });
    openUIDiscoverySession(res.session_id);
    setShowNewPanel(false);
    setNewName("");
    setNewSlug("");
    setNewVersion("");
    setNewUrl("");
    setNewContent("");
    setImportPlan(null);
  }

  // handleAddWorkspace owns immediate per-card feedback and prevents duplicate activation writes for one service.
  async function handleAddWorkspace(e: React.MouseEvent, id: string, name: string) {
    e.preventDefault();
    // The synchronous ref closes the gap before React renders the disabled button.
    if (pendingWorkspaceServiceIdsRef.current.has(id)) return;
    pendingWorkspaceServiceIdsRef.current.add(id);
    setPendingWorkspaceServiceIds(Array.from(pendingWorkspaceServiceIdsRef.current));
    try {
      await api.workspace.addService(id, name, "", "");
      // Keep discovery visible after the first activation so the successful card remains in context until the next page refresh.
      if (catalogPreference === null && activeWorkspaceServiceIds?.length === 0) setCatalogPreference(true);
      // Successful activation updates membership locally without another database-backed page reload.
      setActiveWorkspaceServiceIds((current) => Array.from(new Set([...(current ?? []), id])));
      toast.success(`${name} added to your workspace.`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed to add service to workspace");
    } finally {
      pendingWorkspaceServiceIdsRef.current.delete(id);
      setPendingWorkspaceServiceIds(Array.from(pendingWorkspaceServiceIdsRef.current));
    }
  }

  const workspaceList = workspaceListProjection(workspaceServicePageData);
  const showCatalog = catalogVisible(isAuth, catalogPreference, activeWorkspaceServiceIds, activeSearch);
  // Onboarding copy is reserved for authenticated workspaces whose membership has been authoritatively loaded as empty.
  const emptyWorkspace = workspaceIsEmpty(isAuth, activeWorkspaceServiceIds);
  // Authenticated requests share one error above Workspace; public visitors need it on their only collection.
  const catalogError = catalogErrorForAuth(isAuth, error);

  // The visible-data effect loads the catalogue once when this preference changes.
  function handleCatalogToggle() {
    setCatalogPreference(!showCatalog);
  }

  return (
    <div>
      <div className="flex flex-col sm:flex-row sm:items-start justify-between gap-4 mb-6">
        <div className="min-w-0 max-w-xl">
          <h1 className="text-xl font-semibold text-slate-900">Services</h1>
          <p className="text-slate-500 text-sm mt-1">Choose and configure the services your apps and MCP servers can use.</p>
        </div>
        <div className="flex w-full sm:w-auto items-center gap-3">
          <button
            data-track="open_new_service_panel"
            onClick={() => setShowNewPanel(true)}
            className="flex-1 sm:flex-none px-4 py-2 bg-slate-950 hover:bg-slate-800 text-white text-sm font-medium rounded-lg shadow-sm cursor-pointer"
          >
            Define service
          </button>
        </div>
      </div>

      <ServicesTabs
        isAuth={isAuth}
        view={view}
        activeSessions={activeSessions}
        setView={setView}
      />
      <PendingImports isAuth={isAuth} view={view} props={{ activeSessions, setNewSessionId: openUIDiscoverySession, onRefresh: refreshSessions }} />
      <ServicesContent
        isAuth={isAuth}
        view={view}
        showCatalog={showCatalog}
        emptyWorkspace={emptyWorkspace}
        onToggleCatalog={handleCatalogToggle}
        catalog={{
          integrations: integrations.map(fromService), loading: catalogLoading, error: catalogError, query, setQuery, handleSearch, handleClear,
          searching, setShowNewPanel, page: 1, onPageChange: setPage, totalPages, totalItems, isAuth,
          viewType: "catalog", handleAddWorkspace,
          activeServiceIds: activeWorkspaceServiceIds ?? [],
          pendingServiceIds: pendingWorkspaceServiceIds,
        }}
        workspace={{
          integrations: workspaceList.integrations, loading: workspaceLoading, error, query,
          setQuery, handleSearch, handleClear, searching, setShowNewPanel, page, onPageChange: setPage,
          totalPages: workspaceList.pages, totalItems: workspaceList.total, isAuth, viewType: "workspace",
          handleAddWorkspace,
        }}
      />

      <DefineServicePanel
        visible={showNewPanel}
        props={{
          isAuth,
          onClose: () => setShowNewPanel(false),
          onCancel: () => {
            setShowNewPanel(false);
            setImportPlan(null);
            setNewName("");
            setNewSlug("");
            setNewVersion("");
            setNewUrl("");
            setNewContent("");
            setFileName("");
            setStartError("");
          },
          importPlan, newName, setNewName, isSlugManuallyEdited, setIsSlugManuallyEdited, newSlug, setNewSlug,
          requireVersion, setRequireVersion, importMethod, setImportMethod, newVersion, setNewVersion, sourceType,
          setSourceType, newUrl, setNewUrl, newContent, setNewContent, fileName, setFileName, starting, startError,
          setStartError, handleStart, handleChangeImportSource,
        }}
      />
      <ExtractionSessionPanel
        sessionID={activeDiscoverySessionID}
        reviewOnly={discoveryNavigation.cliHandoff}
        onClose={() => {
          // Closing preserves the durable session so it can be resumed from Imports.
          closeDiscoverySession();
          loadVisibleData();
        }}
        onComplete={closeDiscoverySession}
      />
    </div>
  );
}
