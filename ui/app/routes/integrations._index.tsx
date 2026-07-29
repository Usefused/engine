import { useState, useEffect, useRef, useCallback, type FormEvent } from "react";
import { useNavigate, useSearchParams, useRouteLoaderData, type MetaFunction } from "@remix-run/react";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m: any) => m.id === "root").flatMap((m: any) => m.meta ?? []);
  return [
    ...parentMeta.filter((m: any) => !('title' in m)),
    { title: "Integrations - Fused" },
  ];
};
import { api, type Service, type SpecificationImportPlan, type ActivatedService } from "~/lib/api";
import ExtractionWizard from "~/components/ExtractionWizard";
import IntegrationsListTab, { fromService, fromActivatedService } from "~/components/IntegrationsListTab";
import IntegrationsAnalyticsTab from "~/components/IntegrationsAnalyticsTab";
import IntegrationsPendingTab from "~/components/IntegrationsPendingTab";
import { UploadCloud, ChevronDown, Database, FileCode } from "lucide-react";
import { useToast } from "~/components/Toast";

type ImportSource = { url?: string; content?: string };
type ImportIdentity = { name: string; slug?: string; version?: string };

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
    if (isMissingImportVersion(error)) setRequireVersion(true);
    throw error;
  }
}

function isMissingImportVersion(error: unknown): boolean {
  return error instanceof Error && error.message.includes("version is required when the imported source does not declare one");
}

export default function IntegrationsIndex() {
  const toast = useToast();
  const rootData = useRouteLoaderData<{ isAuth: boolean }>("root");
  const isAuth = rootData?.isAuth ?? false;
  // View state driven by URL search parameter "tab"
  const [searchParams, setSearchParams] = useSearchParams();

  const [integrations, setIntegrations] = useState<Service[]>([]);
  const [workspaceServices, setWorkspaceServices] = useState<ActivatedService[]>([]);
  const [workspaceServicePageData, setWorkspaceServicePageData] = useState<{ data: ActivatedService[]; total: number } | null>(null);
  const [loading, setLoading] = useState(isAuth && !searchParams.get("q"));
  const [error, setError] = useState("");
  const [query, setQuery] = useState(searchParams.get("q") ?? "");
  const [searching, setSearching] = useState(false);
  const [activeSessions, setActiveSessions] = useState<any[]>([]);
  const [totalPages, setTotalPages] = useState(1);
  const [totalItems, setTotalItems] = useState(0);
  const navigate = useNavigate();
  const urlTab = searchParams.get("tab");
  const view = (urlTab === "analytics" || urlTab === "pending" || urlTab === "catalog") ? urlTab : "workspace";

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

  const setView = (newTab: "workspace" | "catalog" | "analytics" | "pending") => {
    setSearchParams(prev => {
      prev.set("tab", newTab);
      return prev;
    }, { replace: true });
  };

  const [analytics, setAnalytics] = useState<any>(null);

  // Side panel states
  const [showMoreMenu, setShowMoreMenu] = useState(false);
  const [showNewPanel, setShowNewPanel] = useState(false);
  const [newSessionId, setNewSessionId] = useState<string | null>(null);
  const [newName, setNewName] = useState("");
  const [newSlug, setNewSlug] = useState("");
  const [isSlugManuallyEdited, setIsSlugManuallyEdited] = useState(false);
  const [newVersion, setNewVersion] = useState("");
  const [requireVersion, setRequireVersion] = useState(false);
  const [importMethod, setImportMethod] = useState<"openapi" | "docs">("openapi");
  const [sourceType, setSourceType] = useState<"url" | "text">("url");
  const [newUrl, setNewUrl] = useState("");
  const [targetContext, setTargetContext] = useState("");
  const [newContent, setNewContent] = useState("");
  const [fileName, setFileName] = useState("");
  const [starting, setStarting] = useState(false);
  const [startError, setStartError] = useState("");

  const [importPlan, setImportPlan] = useState<SpecificationImportPlan | null>(null);

  const loadCatalogData = useCallback(async (p: number = page) => {
    setLoading(true);
    try {
      const queryStr = `
        query($page: Int, $limit: Int) {
          services(page: $page, limit: $limit) {
            data {
              id
              name
              base_url
              servers {
                url
                description
              }
              is_public
              is_owner
              slug
              provider { name handle }
              canonical_ref
            }
            total
            page
            limit
          }
        }
      `;
      const [res, wsRes, wsPageRes] = await Promise.all([
        api.graphql<{ services: any, globalServiceAnalytics: any }>(queryStr, { page: p, limit: 10 }),
        isAuth ? api.workspace.getServices() : Promise.resolve([]),
        Promise.resolve(null)
      ]);
      setIntegrations(res.services.data);
      setWorkspaceServices(wsRes);
      setWorkspaceServicePageData(wsPageRes);
      lastLoadedPageRef.current = { page: res.services.page, isAuth, view: "catalog" };
      if (res.services.page !== page) {
        setPage(res.services.page);
      }
      setTotalPages(Math.ceil(res.services.total / res.services.limit) || 1);
      setTotalItems(res.services.total);

    } catch (e: any) {
      setError(e.message);
    } finally {
      setLoading(false);
    }
  }, [page, isAuth, query]);

  const loadWorkspaceData = useCallback(async (p: number = page) => {
    setLoading(true);
    try {
      const [wsRes, wsPageRes] = await Promise.all([
        isAuth ? api.workspace.getServices() : Promise.resolve([]),
        isAuth ? api.workspace.getServicesPage(10, (p - 1) * 10, query ? [query] : undefined) : Promise.resolve(null)
      ]);
      setWorkspaceServices(wsRes);
      setWorkspaceServicePageData(wsPageRes);
      lastLoadedPageRef.current = { page: p, isAuth, view: "workspace" };
    } catch (e: any) {
      setError(e.message);
    } finally {
      setLoading(false);
    }
  }, [page, isAuth, query]);

  const loadAnalyticsData = useCallback(async () => {
    setLoading(true);
    try {
      const queryStr = `
        query {
          globalServiceAnalytics {
            total_services
            total_endpoints
            total_drift_events
            total_sdk_upgrades
            total_outdated_sdks
          }
        }
      `;
      const res = await api.graphql<{ globalServiceAnalytics: any }>(queryStr);
      setAnalytics(res.globalServiceAnalytics);
      lastLoadedPageRef.current = { page, isAuth, view: "analytics" };
    } catch (e: any) {
      setError(e.message);
    } finally {
      setLoading(false);
    }
  }, [page, isAuth]);

  const loadVisibleData = useCallback(async (p: number = page) => {
    if (view === "catalog" || !isAuth) {
      await loadCatalogData(p);
      return;
    }

    if (view === "workspace") {
      await loadWorkspaceData(p);
      return;
    }

    if (view === "analytics") {
      await loadAnalyticsData();
      return;
    }

    if (view === "pending") {
      refreshSessions();
      lastLoadedPageRef.current = { page, isAuth, view: "pending" };
    }
  }, [view, isAuth, page, loadCatalogData, loadWorkspaceData, loadAnalyticsData]);

  useEffect(() => {
    if (!isAuth || !query) {
      if (
        lastLoadedPageRef.current.page !== page ||
        lastLoadedPageRef.current.isAuth !== isAuth ||
        lastLoadedPageRef.current.view !== view
      ) {
        loadVisibleData(page);
      }
    }
  }, [page, isAuth, view, loadVisibleData, query]);

  const refreshSessions = () => {
    api.integrations.getActiveSessions()
      .then((sessions) => setActiveSessions(sessions || []))
      .catch((err) => console.error("Failed to load active sessions:", err));
  };

  useEffect(() => {
    if (view === "pending") {
      refreshSessions();
    }
  }, [view]);



  async function runSearch(q: string) {
    if (!q.trim()) return;
    setSearchParams(prev => {
      const next = new URLSearchParams(prev);
      next.set("q", q);
      return next;
    }, { replace: true });
    setSearching(true);
    setError("");
    try {
      const queryStr = `
        query($q: String!) {
          searchServices(q: $q) {
            id
            name
            base_url
            servers {
              url
              description
            }
            is_public
            is_owner
            slug
            provider { name handle }
            canonical_ref
          }
        }
      `;
      const res = await api.graphql<{ searchServices: any }>(queryStr, { q });
      setIntegrations(res.searchServices);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Search failed");
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
    setLoading(false); // ensure the services spinner doesn't block search results
    setSearching(true);
    const id = setTimeout(() => runSearch(query), 400);
    return () => clearTimeout(id);
  }, [query]);

  async function handleSearch(e?: FormEvent) {
    if (e) e.preventDefault();
    if (!query.trim()) return;
    if (view === "workspace") {
      setSearching(true);
      setPage(1);
      loadVisibleData(1).finally(() => setSearching(false));
    } else {
      runSearch(query);
    }
  }

  async function handleClear() {
    setQuery("");
    setSearchParams(prev => {
      const next = new URLSearchParams(prev);
      next.delete("q");
      return next;
    }, { replace: true });
    if (page === 1) {
      loadVisibleData(1);
    } else {
      setPage(1);
    }
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
    const applied = await api.integrations.applyImport(importPlan.plan_id, importPlan.source_hash);
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

  async function handleDocsImport(source: ImportSource) {
    const res = await api.integrations.start(
      newName.trim(), newSlug.trim(), newVersion.trim(), source.url || "", "docs", undefined,
      undefined, targetContext.trim() || undefined, undefined,
    );
    if (!res.session_id) throw new Error("No session ID returned");
    setNewSessionId(res.session_id);
    setShowNewPanel(false);
    setNewName("");
    setNewSlug("");
    setNewVersion("");
    setNewUrl("");
    setNewContent("");
    setTargetContext("");
    setImportPlan(null);
  }

  // Why: Separating the Workspace add/remove logic from Registry delete logic enforces the 
  // clear boundary between the local Engine state and the global cloud catalog.
  async function handleAddWorkspace(e: React.MouseEvent, id: string, name: string) {
    e.preventDefault();
    try {
      await api.workspace.addService(id, name, "", "");
      toast.success(`${name} added to your workspace.`);
      // Reload to reflect the newly added service
      loadVisibleData();
    } catch (err: any) {
      toast.error(err.message || "Failed to add service to workspace");
    }
  }

  async function handleRemoveWorkspace(e: React.MouseEvent, id: string) {
    e.preventDefault();
    const confirmed = await toast.confirm("Are you sure you want to remove this service? It will be uninstalled from your workspace.");
    if (confirmed) {
      try {
        await api.workspace.removeService(id);
        setWorkspaceServices(prev => prev.filter(s => s.service_id !== id && s.id !== id));
        toast.success("Service removed from workspace.");
      } catch (err: any) {
        toast.error(err.message || "Failed to remove service");
      }
    }
  }

  async function handleDelete(e: React.MouseEvent, id: string) {
    e.preventDefault(); // Prevent navigating to the Service detail page
    const confirmed = await toast.confirm("Are you sure you want to permanently delete this service? This will destroy this service for everyone using it.");
    if (confirmed) {
      try {
        await api.integrations.delete(id);
        // Optimistically remove from local state immediately (avoids stale cache showing deleted item)
        setIntegrations(prev => prev.filter(s => s.id !== id));
        setWorkspaceServices(prev => prev.filter(s => s.service_id !== id && s.id !== id));
        if (query.trim()) {
          // Re-run search to get fresh results from server
          runSearch(query);
        } else {
          loadVisibleData();
        }
        toast.success("Service deleted successfully.");
      } catch (err: any) {
        toast.error(err.message || "Failed to delete service");
      }
    }
  }

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <div>
          <h1 className="text-xl font-semibold text-slate-900">Services</h1>
          <p className="text-slate-500 text-sm mt-1">Manage and track your API integrations.</p>
        </div>
        <div className="flex items-center gap-3">
          {isAuth && (
            <div className="relative">
              <button
                type="button"
                disabled={view === "catalog"}
                onClick={() => setShowMoreMenu(!showMoreMenu)}
                className={`px-4 py-2 bg-white border border-slate-200 text-sm font-medium rounded-lg flex items-center gap-2 transition-colors ${view === "catalog" ? 'opacity-60 cursor-not-allowed text-slate-400' : 'hover:bg-slate-50 text-slate-700 cursor-pointer'}`}
              >
                More <ChevronDown className="w-4 h-4" />
              </button>
              {showMoreMenu && (
                <>
                  <div className="fixed inset-0 z-10" onClick={() => setShowMoreMenu(false)} />
                  <div className="absolute right-0 mt-2 w-56 bg-white rounded-lg shadow-lg border border-slate-100 py-1 z-20">
                    <button
                      onClick={() => { setView("analytics"); setShowMoreMenu(false); }}
                      className="w-full text-left px-4 py-2 text-sm text-slate-700 hover:bg-slate-50 flex items-center gap-2"
                    >
                      <Database className="w-4 h-4 text-slate-400" />
                      Analytics
                    </button>
                    <button
                      onClick={() => { setView("pending"); setShowMoreMenu(false); }}
                      className="w-full text-left px-4 py-2 text-sm text-slate-700 hover:bg-slate-50 flex items-center justify-between"
                    >
                      <div className="flex items-center gap-2">
                        <FileCode className="w-4 h-4 text-slate-400" />
                        Pending Imports
                      </div>
                      {activeSessions.length > 0 && (
                        <span className="bg-amber-100 text-amber-700 py-0.5 px-2 rounded-full text-[10px] font-bold">
                          {activeSessions.length}
                        </span>
                      )}
                    </button>
                  </div>
                </>
              )}
            </div>
          )}
          <button
            data-track="open_new_service_panel"
            onClick={() => setShowNewPanel(true)}
            className="px-4 py-2 bg-blue-500 hover:bg-blue-600 shadow-blue-200 text-white text-sm font-medium rounded-lg cursor-pointer"
          >
            + New
          </button>
        </div>
      </div>

      {isAuth && (
        <div className="flex bg-slate-100 p-1 rounded-lg w-fit mb-6">
          <button
            data-track="view_workspace_tab"
            type="button"
            onClick={() => setView("workspace")}
            className={`px-4 py-1.5 text-sm font-medium rounded-md transition-all ${(view === "workspace" || view === "analytics" || view === "pending") ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700 hover:bg-slate-200/50"} cursor-pointer`}
          >
            Workspace
          </button>
          <button
            data-track="view_catalog_tab"
            type="button"
            onClick={() => setView("catalog")}
            className={`px-4 py-1.5 text-sm font-medium rounded-md transition-all ${view === "catalog" ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700 hover:bg-slate-200/50"} cursor-pointer`}
          >
            Catalog
          </button>
        </div>
      )}

      {/* Active Sessions View */}
      {isAuth && view === "pending" && (
        <div className="mb-6">
          <button onClick={() => setView("workspace")} className="text-blue-500 hover:text-blue-600 text-sm font-medium mb-4 flex items-center gap-1">
            ← Back to Workspace Services
          </button>
          <IntegrationsPendingTab
            activeSessions={activeSessions}
            setNewSessionId={setNewSessionId}
            onRefresh={refreshSessions}
          />
        </div>
      )}

      {isAuth && view === "analytics" && analytics ? (
        <div className="mb-6">
          <button onClick={() => setView("workspace")} className="text-blue-500 hover:text-blue-600 text-sm font-medium mb-4 flex items-center gap-1">
            ← Back to Workspace Services
          </button>
          <IntegrationsAnalyticsTab analytics={analytics} />
        </div>
      ) : view === "catalog" || !isAuth ? (
          <IntegrationsListTab
            integrations={integrations.map(fromService)}
            loading={loading}
            error={error}
            query={query}
            setQuery={setQuery}
            handleSearch={handleSearch}
            handleClear={handleClear}
            searching={searching}
            handleDelete={handleDelete}
            setShowNewPanel={setShowNewPanel}
            page={page}
            onPageChange={setPage}
            totalPages={totalPages}
            totalItems={totalItems}
            isAuth={isAuth}
            viewType="catalog"
            handleAddWorkspace={handleAddWorkspace}
            handleRemoveWorkspace={handleRemoveWorkspace}
            activeServiceIds={workspaceServices.map(s => s.service_id)}
          />
      ) : view === "workspace" && isAuth ? (
          <IntegrationsListTab
            integrations={(workspaceServicePageData?.data || []).map(fromActivatedService)}
            loading={loading}
            error={error}
            query={query}
            setQuery={setQuery}
            handleSearch={handleSearch}
            handleClear={handleClear}
            searching={searching}
            handleDelete={handleDelete}
            setShowNewPanel={setShowNewPanel}
            page={page}
            onPageChange={setPage}
            totalPages={Math.ceil((workspaceServicePageData?.total || 0) / 10) || 1}
            totalItems={workspaceServicePageData?.total || 0}
            isAuth={isAuth}
            viewType="workspace"
            handleAddWorkspace={handleAddWorkspace}
            handleRemoveWorkspace={handleRemoveWorkspace}
          />
      ) : null}

      {/* New Service Side Panel */}
      {showNewPanel && (
        <>
          <div 
            className="fixed inset-0 bg-slate-900/20 z-40 transition-opacity" 
            onClick={() => setShowNewPanel(false)}
          />
          <div className="fixed inset-y-0 right-0 w-full md:w-[600px] bg-white shadow-2xl z-50 overflow-y-auto transform transition-transform border-l border-slate-200 flex flex-col">
            <div className="p-6 border-b border-slate-100 flex items-center justify-between sticky top-0 bg-white/90 backdrop-blur z-10">
              <h2 className="text-lg font-semibold text-slate-900">New Service</h2>
              <button
                data-track="close_new_service_panel"
                onClick={() => setShowNewPanel(false)}
                className="p-2 text-slate-400 hover:text-slate-600 hover:bg-slate-100 rounded-full transition-colors cursor-pointer"
              >
                ✕
              </button>
            </div>
            
            <div className="p-6">
              {!isAuth ? (
                <div className="flex flex-col items-center justify-center py-12 text-center gap-4">
                  <p className="text-slate-600 text-sm">You need to be logged in to create a service.</p>
                  <a
                    href="/login"
                    className="px-5 py-2 bg-blue-500 hover:bg-blue-600 text-white text-sm font-medium rounded-lg transition-colors"
                  >
                    Log in to get started
                  </a>
                </div>
              ) : (
              <form 
                onSubmit={handleStart} 
                className="space-y-6"
                toolname="create_new_service"
                tooldescription="Create a new service by importing an OpenAPI spec or Docs URL."
              >
                <div>
                  <label className="block text-sm font-medium text-slate-700 mb-2">
                    Service name
                  </label>
                  <input
                    type="text"
                    value={newName}
                    onChange={(e) => {
                      const val = e.target.value;
                      setNewName(val);
                      if (!isSlugManuallyEdited) {
                        setNewSlug(val.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, ''));
                      }
                    }}
                    placeholder="Stripe"
                    className="w-full px-4 py-3 rounded-lg border border-slate-300 text-[15px] focus:outline-none focus:ring-2 focus:ring-blue-500/50 focus:border-blue-500 transition-all"
                    autoFocus
                  />
                </div>
                <div>
                  <label className="block text-sm font-medium text-slate-700 mb-2">
                    Service slug
                  </label>
                  <input
                    type="text"
                    value={newSlug}
                    onChange={(e) => {
                      setNewSlug(e.target.value);
                      setIsSlugManuallyEdited(true);
                    }}
                    placeholder="stripe"
                    className="w-full px-4 py-3 rounded-lg border border-slate-300 text-[15px] focus:outline-none focus:ring-2 focus:ring-blue-500/50 focus:border-blue-500 transition-all"
                  />
                </div>
                {(requireVersion || importMethod === "docs" || importPlan) && (
                  <div>
                    <label className="block text-sm font-medium text-slate-700 mb-2">
                      Service version
                      {!importPlan && (
                        <span className="text-red-500 font-normal"> * (required)</span>
                      )}
                    </label>
                    <input
                      type="text"
                      value={newVersion}
                      onChange={(e) => setNewVersion(e.target.value)}
                      readOnly={Boolean(importPlan)}
                      placeholder="1.0"
                      className="w-full px-4 py-3 rounded-lg border border-slate-300 text-[15px] focus:outline-none focus:ring-2 focus:ring-blue-500/50 focus:border-blue-500 transition-all read-only:bg-slate-50 read-only:text-slate-600"
                    />
                  </div>
                )}
                <div>
                  {importPlan ? (
                    <div className="space-y-4">
                      <div className="border-y border-slate-200 py-4 space-y-3">
                        <div className="grid grid-cols-3 gap-3 pt-1 text-center">
                          <div><div className="text-lg font-semibold text-emerald-700">{importPlan.diff.added}</div><div className="text-xs text-slate-500">Added</div></div>
                          <div><div className="text-lg font-semibold text-amber-700">{importPlan.diff.changed}</div><div className="text-xs text-slate-500">Changed</div></div>
                          <div><div className="text-lg font-semibold text-red-700">{importPlan.diff.removed}</div><div className="text-xs text-slate-500">Removed</div></div>
                        </div>
                      </div>
                      <button
                        type="button"
                        onClick={handleChangeImportSource}
                        disabled={starting}
                        className="text-sm font-medium text-blue-500 hover:text-blue-600 disabled:cursor-not-allowed disabled:text-slate-400"
                      >
                        Change source
                      </button>
                    </div>
                  ) : (
                    <>
                      <label className="block text-sm font-medium text-slate-700 mb-2">
                        Import Method
                      </label>
                      <div className="flex gap-4 mb-4 border-b border-slate-200">
                        <button
                          data-track="select_import_method_openapi"
                          type="button"
                          onClick={() => setImportMethod("openapi")}
                          className={`pb-2 text-sm font-medium ${importMethod === "openapi" ? "text-blue-500 border-b-2 border-blue-500" : "text-slate-500 hover:text-slate-700"} cursor-pointer`}
                        >
                          {"OpenAPI / GraphQL / AsyncAPI / Postman"}
                        </button>
                        <button
                          data-track="select_import_method_docs"
                          type="button"
                          onClick={() => setImportMethod("docs")}
                          className={`pb-2 text-sm font-medium ${importMethod === "docs" ? "text-blue-500 border-b-2 border-blue-500" : "text-slate-500 hover:text-slate-700"} cursor-pointer`}
                        >
                          Docs URL
                          <span className="ml-1.5 inline-flex items-center px-1.5 py-0.5 rounded text-[9px] font-semibold bg-amber-50 text-amber-700 border border-amber-200 uppercase tracking-wider">
                            Experimental
                          </span>
                        </button>
                      </div>

                      {importMethod === "openapi" && (
                        <>
                          <div className="flex bg-slate-100 p-1 rounded-lg mb-6 w-fit">
                            <button
                              data-track="select_source_type_url"
                              type="button"
                              onClick={() => setSourceType("url")}
                              className={`px-4 py-1.5 text-sm font-medium rounded-md transition-all ${sourceType === "url" ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700 hover:bg-slate-200/50"} cursor-pointer`}
                            >
                              Provide URL
                            </button>
                            <button
                              data-track="select_source_type_file"
                              type="button"
                              onClick={() => setSourceType("text")}
                              className={`px-4 py-1.5 text-sm font-medium rounded-md transition-all ${sourceType === "text" ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700 hover:bg-slate-200/50"} cursor-pointer`}
                            >
                              Upload File
                            </button>
                          </div>

                          {sourceType === "url" ? (
                            <div>
                              <label className="block text-sm font-medium text-slate-700 mb-1">
                                {"Schema / Collection URL"}
                              </label>
                              <p className="text-xs text-slate-500 mb-3">Note: Postman Collection support is currently experimental.</p>
                              <input
                                type="url"
                                value={newUrl}
                                onChange={(e) => {
                                  setNewUrl(e.target.value);
                                  setRequireVersion(false);
                                  setNewVersion("");
                                }}
                                placeholder="https://api.example.com/openapi.json"
                                className="w-full px-4 py-3 rounded-lg border border-slate-300 text-[15px] focus:outline-none focus:ring-2 focus:ring-blue-500/50 focus:border-blue-500 transition-all"
                              />
                            </div>
                          ) : (
                            <div>
                              <label className="block text-sm font-medium text-slate-700 mb-1">
                                {"Upload Schema / Collection File"}
                              </label>
                              <p className="text-xs text-slate-500 mb-3">Note: Postman Collection support is currently experimental.</p>
                              <div className="flex flex-col gap-2">
                                <div className="relative border-2 border-dashed border-slate-300 rounded-xl p-8 text-center hover:bg-slate-50 hover:border-blue-500 transition-all group">
                                  <input
                                    type="file"
                                    className="absolute inset-0 w-full h-full opacity-0 cursor-pointer"
                                    onChange={(e) => {
                                      const file = e.target.files?.[0];
                                      if (!file) {
                                        setNewContent("");
                                        setFileName("");
                                        return;
                                      }
                                      const ext = file.name.split('.').pop()?.toLowerCase();
                                      if (!['json', 'yaml', 'yml', 'graphql'].includes(ext || '')) {
                                        setStartError("Only JSON, YAML, or GraphQL files are supported.");
                                        setNewContent("");
                                        setFileName("");
                                        e.target.value = '';
                                        return;
                                      }
                                      if (file.size > 5 * 1024 * 1024) {
                                        setStartError("File is too large. Maximum size is 5MB.");
                                        setNewContent("");
                                        setFileName("");
                                        e.target.value = '';
                                        return;
                                      }
                                      setStartError("");
                                      setFileName(file.name);
                                      const reader = new FileReader();
                                      reader.onload = (ev) => {
                                        const text = ev.target?.result as string;
                                        setNewContent(text);
                                        setRequireVersion(false);
                                        setNewVersion("");
                                      };
                                      reader.readAsText(file);
                                    }}
                                  />
                                  <div className="flex flex-col items-center gap-2 pointer-events-none">
                                    <UploadCloud className="w-8 h-8 text-slate-400 group-hover:text-blue-500 transition-colors" />
                                    <p className="text-sm font-medium text-slate-700">Click to upload or drag and drop</p>
                                    <p className="text-xs text-slate-500">JSON, YAML, or GraphQL (max 5MB)</p>
                                  </div>
                                </div>
                                {fileName && (
                                  <p className="text-sm text-green-600 font-medium">✓ Loaded {fileName}</p>
                                )}
                              </div>
                            </div>
                          )}
                        </>
                      )}

                      {importMethod === "docs" && (
                        <div className="space-y-4">
                          <div>
                            <label className="block text-sm font-medium text-slate-700 mb-2">
                              Service Docs URL
                            </label>
                            <input
                              type="url"
                              value={newUrl}
                              onChange={(e) => setNewUrl(e.target.value)}
                              placeholder="https://stripe.com/docs/api"
                              className="w-full px-4 py-3 rounded-lg border border-slate-300 text-[15px] focus:outline-none focus:ring-2 focus:ring-blue-500/50 focus:border-blue-500 transition-all"
                            />
                          </div>
                        </div>
                      )}
                    </>
                  )}
                </div>
                <div>
                  {startError && (
                    <p className="text-sm text-red-600 bg-red-50 border border-red-100 p-3 rounded-lg">
                      {startError}
                    </p>
                  )}
                  <div className="pt-4 flex justify-end gap-3">
                    <button
                      data-track="cancel_new_service"
                      type="button"
                      onClick={() => {
                        setShowNewPanel(false);
                        setImportPlan(null);
                        setNewName("");
                        setNewSlug("");
                        setNewVersion("");
                        setNewUrl("");
                        setNewContent("");
                        setFileName("");
                        setTargetContext("");
                        setStartError("");
                      }}
                      className="px-4 py-2 text-sm font-medium text-slate-600 hover:text-slate-800 cursor-pointer"
                    >
                      Cancel
                    </button>
                    <button
                      data-track="submit_new_service"
                      type="submit"
                      disabled={starting || !newName.trim() || (!importPlan && requireVersion && !newVersion.trim()) || (importMethod === "docs" && (!newUrl.trim() || !newVersion.trim())) || (importMethod === "openapi" && !importPlan && sourceType === "url" && !newUrl.trim()) || (importMethod === "openapi" && !importPlan && sourceType === "text" && !newContent.trim())}
                      className="px-6 py-2 bg-blue-600 hover:bg-blue-700 active:bg-blue-800 disabled:opacity-50 text-white font-medium rounded-lg transition-colors flex justify-center items-center gap-2"
                    >
                      {starting 
                        ? "Processing..." 
                        : importMethod === "openapi" 
                          ? importPlan ? "Apply Import" : "Create Plan"
                          : "Analyze Docs"}
                  </button>
                </div>
              </div>
            </form>
              )}
          </div>
        </div>
        </>
      )}

      {/* Extraction Wizard Overlay */}
      {newSessionId && (
        <ExtractionWizard 
          sessionId={newSessionId} 
          onClose={() => {
            if (newSessionId) {
              api.integrations.cancelSession(newSessionId).catch(console.error);
              api.integrations.deleteSession(newSessionId).catch(console.error);
            }
            setNewSessionId(null);
            loadVisibleData();
          }}
          onComplete={() => {
            setNewSessionId(null);
          }}
        />
      )}
    </div>
  );
}
