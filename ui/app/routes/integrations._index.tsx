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
      id name description base_url servers { url description }
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

// catalogVisible defaults discovery on only for a proven-empty workspace while preserving an explicit user choice.
function catalogVisible(isAuth: boolean, preference: boolean | null, serviceIDs: string[] | null): boolean {
  // Public visitors have only the catalogue surface available.
  if (!isAuth) return true;
  // Once the user operates the toggle, membership changes must not override that choice.
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

// needsVisibleLoad ensures bookmarked searches resolve membership before the combined workspace chooses its default catalogue state.
function needsVisibleLoad(isAuth: boolean, query: string, serviceIDs: string[] | null): boolean {
  // Anonymous browsing, unfiltered views, and unknown authenticated membership all require an initial request.
  return !isAuth || !query || serviceIDs === null;
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

// ServicesContent keeps workspace-owned data first and appends catalogue discovery only when requested.
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
  // Shared search copy names the catalogue only while that collection participates in results.
  const searchPlaceholder = showCatalog ? "Search workspace and catalog" : "Search your workspace services";
  return (
    <>
      <CatalogToggle checked={showCatalog} onChange={onToggleCatalog} />
      {/* Imported and activated services remain the first collection on the page. */}
      {!emptyWorkspace && <h2 className="mb-4 text-sm font-semibold text-slate-900">Workspace services</h2>}
      <IntegrationsListTab {...workspace} hideEmptyState searchPlaceholder={searchPlaceholder} />
      {/* Catalogue cards are appended without changing the active Workspace navigation state. */}
      {showCatalog && <section className="mt-6">
        <EmptyWorkspaceCatalogIntro visible={emptyWorkspace} />
        {!emptyWorkspace && <div className="mb-4"><h2 className="text-sm font-semibold text-slate-900">Service catalog</h2><p className="mt-1 text-xs text-slate-500">Add public services without displacing what is already in your workspace.</p></div>}
        <IntegrationsListTab {...catalog} showSearch={false} />
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
  
  // Ref to track last loaded page/view to prevent duplicate requests/flickers
  const lastLoadedPageRef = useRef<{ page: number | null; isAuth: boolean; view: string | null }>({
    page: null,
    isAuth,
    view: null,
  });

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
    setCatalogLoading(true);
    try {
      const [catalogPage, workspaceServiceIds] = await Promise.all([
        fetchCatalogPage(options.query ?? query.trim()),
        // Workspace loading already knows membership, so reuse it instead of repeating the Engine query.
        options.knownWorkspaceServiceIds !== undefined
          ? Promise.resolve(options.knownWorkspaceServiceIds)
          : isAuth ? api.workspace.getServiceIds() : Promise.resolve([]),
      ]);
      setIntegrations(catalogPage.data);
      setActiveWorkspaceServiceIds(workspaceServiceIds);
      setTotalPages(Math.ceil(catalogPage.total / catalogPage.limit) || 1);
      setTotalItems(catalogPage.total);

    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load services");
    } finally {
      setCatalogLoading(false);
    }
  }, [isAuth, query]);

  // loadWorkspaceData keeps local services first and optionally loads catalogue cards beneath them.
  const loadWorkspaceData = useCallback(async (p: number = page, searchQuery: string = query) => {
    setWorkspaceLoading(true);
    try {
      const [wsPageRes, workspaceServiceIds] = await Promise.all([
        isAuth ? api.workspace.getServicesPage(10, (p - 1) * 10, searchQuery ? [searchQuery] : undefined) : null,
        isAuth ? api.workspace.getServiceIds() : Promise.resolve([]),
      ]);
      setActiveWorkspaceServiceIds(workspaceServiceIds);
      // The workspace tab renders only the requested page, so it must not issue a second unpaginated membership projection.
      setWorkspaceServicePageData(wsPageRes);
      lastLoadedPageRef.current = { page: p, isAuth, view: "workspace" };
      // An explicit toggle choice wins; otherwise only a proven-empty workspace loads catalogue discovery by default.
      if (catalogVisible(isAuth, catalogPreference, workspaceServiceIds)) {
        await loadCatalogData({ knownWorkspaceServiceIds: workspaceServiceIds, query: searchQuery });
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load services");
    } finally {
      setWorkspaceLoading(false);
    }
  }, [page, isAuth, query, catalogPreference, loadCatalogData]);

  const loadVisibleData = useCallback(async (p: number = page) => {
    // Public browsing has no workspace collection to request first.
    if (!isAuth) {
      await loadCatalogData();
      return;
    }

    if (view === "workspace") {
      await loadWorkspaceData(p);
      return;
    }

    if (view === "pending") {
      refreshSessions();
      lastLoadedPageRef.current = { page, isAuth, view: "pending" };
    }
  }, [view, isAuth, page, loadCatalogData, loadWorkspaceData]);

  useEffect(() => {
    // Membership must load even when the initial URL already contains a search query.
    if (needsVisibleLoad(isAuth, query, activeWorkspaceServiceIds)) {
      if (
        lastLoadedPageRef.current.page !== page ||
        lastLoadedPageRef.current.isAuth !== isAuth ||
        lastLoadedPageRef.current.view !== view
      ) {
        loadVisibleData(page);
      }
    }
  }, [page, isAuth, view, loadVisibleData, query, activeWorkspaceServiceIds]);

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



  // runSearch refreshes every collection currently represented by the shared Workspace search field.
  async function runSearch(q: string) {
    const normalized = q.trim();
    setSearchParams(prev => {
      const next = new URLSearchParams(prev);
      // Clearing search removes the query entirely so refreshes return to ordinary browse semantics.
      if (normalized) next.set("q", normalized);
      else next.delete("q");
      return next;
    }, { replace: true });
    setSearching(true);
    setError("");
    try {
      // Authenticated Workspace searches preserve local-first ordering and refresh the optional catalogue beneath it.
      if (isAuth && view === "workspace") await loadWorkspaceData(1, normalized);
      else await loadCatalogData({ query: normalized });
      // Search results are one bounded page, so retaining another page number would mislabel their range.
      if (page !== 1) setPage(1);
    } finally {
      setSearching(false);
    }
  }

  // Debounced search on type — show loader immediately so user gets feedback
  useEffect(() => {
    if (!query.trim()) {
      setSearching(false);
      return;
    }
    setSearching(true);
    const id = setTimeout(() => runSearch(query), 400);
    return () => clearTimeout(id);
  }, [query]);

  async function handleSearch(e?: FormEvent) {
    if (e) e.preventDefault();
    if (!query.trim()) return;
    await runSearch(query);
  }

  async function handleClear() {
    setQuery("");
    await runSearch("");
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

  // handleRemoveWorkspace updates local membership markers after Engine confirms removal.
  async function handleRemoveWorkspace(e: React.MouseEvent, id: string) {
    e.preventDefault();
    const confirmed = await toast.confirm("Are you sure you want to remove this service? It will be uninstalled from your workspace.");
    if (confirmed) {
      try {
        await api.workspace.removeService(id);
        setActiveWorkspaceServiceIds(prev => prev?.filter(serviceID => serviceID !== id) ?? []);
        toast.success("Service removed from workspace.");
        // Removing the final service should return to catalogue onboarding rather than retain a stale workspace card.
        await loadWorkspaceData(1);
      } catch (err) {
        toast.error(err instanceof Error ? err.message : "Failed to remove service");
      }
    }
  }

  // handleDelete removes an owned catalogue service and its matching local activation marker.
  async function handleDelete(e: React.MouseEvent, id: string) {
    e.preventDefault(); // Prevent navigating to the Service detail page
    const confirmed = await toast.confirm("Are you sure you want to permanently delete this service? This will destroy this service for everyone using it.");
    if (confirmed) {
      try {
        await api.integrations.delete(id);
        // Optimistically remove from local state immediately (avoids stale cache showing deleted item)
        setIntegrations(prev => prev.filter(s => s.id !== id));
        setActiveWorkspaceServiceIds(prev => prev?.filter(serviceID => serviceID !== id) ?? []);
        if (query.trim()) {
          // Re-run search to get fresh results from server
          runSearch(query);
        } else {
          loadVisibleData();
        }
        toast.success("Service deleted successfully.");
      } catch (err) {
        toast.error(err instanceof Error ? err.message : "Failed to delete service");
      }
    }
  }

  const workspaceList = workspaceListProjection(workspaceServicePageData);
  const showCatalog = catalogVisible(isAuth, catalogPreference, activeWorkspaceServiceIds);
  // Onboarding copy is reserved for authenticated workspaces whose membership has been authoritatively loaded as empty.
  const emptyWorkspace = workspaceIsEmpty(isAuth, activeWorkspaceServiceIds);
  // Authenticated requests share one error above Workspace; public visitors need it on their only collection.
  const catalogError = catalogErrorForAuth(isAuth, error);

  // handleCatalogToggle records an explicit choice and fetches catalogue data only when it becomes visible.
  async function handleCatalogToggle() {
    const nextPreference = !showCatalog;
    setCatalogPreference(nextPreference);
    // Enabling discovery should populate the collection immediately with the current shared search scope.
    if (nextPreference) {
      await loadCatalogData({
        knownWorkspaceServiceIds: activeWorkspaceServiceIds ?? undefined,
        query: query.trim(),
      });
    }
  }

  return (
    <div>
      <div className="flex flex-col sm:flex-row sm:items-start justify-between gap-4 mb-6">
        <div className="min-w-0 max-w-xl">
          <h1 className="text-xl font-semibold text-slate-900">Services</h1>
          <p className="text-slate-500 text-sm mt-1">Choose and configure the services your apps, MCP servers, and workflows can use.</p>
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
          searching, handleDelete, setShowNewPanel, page: 1, onPageChange: setPage, totalPages, totalItems, isAuth,
          viewType: "catalog", handleAddWorkspace, handleRemoveWorkspace,
          activeServiceIds: activeWorkspaceServiceIds ?? [],
          pendingServiceIds: pendingWorkspaceServiceIds,
        }}
        workspace={{
          integrations: workspaceList.integrations, loading: workspaceLoading, error, query,
          setQuery, handleSearch, handleClear, searching, handleDelete, setShowNewPanel, page, onPageChange: setPage,
          totalPages: workspaceList.pages, totalItems: workspaceList.total, isAuth, viewType: "workspace",
          handleAddWorkspace, handleRemoveWorkspace,
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
