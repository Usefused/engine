import { useState, useEffect, useRef, useCallback, type FormEvent } from "react";
import { useNavigate, useSearchParams, useRouteLoaderData, type MetaFunction } from "@remix-run/react";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m) => m.id === "root").flatMap((m) => m.meta ?? []);
  return [
    ...parentMeta.filter((m) => !('title' in m)),
    { title: "Services - Fused" },
  ];
};
import { api, type Service, type SpecificationImportPlan, type ActivatedService, type AgentSession } from "~/lib/api";
import ExtractionWizard from "~/components/ExtractionWizard";
import IntegrationsListTab, { fromService, fromActivatedService } from "~/components/IntegrationsListTab";
import IntegrationsPendingTab from "~/components/IntegrationsPendingTab";
import { DefineServiceDrawer } from "~/components/DefineServiceDrawer";
import { useToast } from "~/components/Toast";
import { isImportVersionRequired } from "~/lib/authorization-error";

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
  const [activeSessions, setActiveSessions] = useState<AgentSession[]>([]);
  const [totalPages, setTotalPages] = useState(1);
  const [totalItems, setTotalItems] = useState(0);
  const navigate = useNavigate();
  const urlTab = searchParams.get("tab");
  const view = (urlTab === "pending" || urlTab === "catalog") ? urlTab : "workspace";

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

  const setView = (newTab: "workspace" | "catalog" | "pending") => {
    setSearchParams(prev => {
      prev.set("tab", newTab);
      return prev;
    }, { replace: true });
  };

  // Side panel states
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
        api.graphql<{ services: { data: Service[]; total: number; page: number; limit: number }; globalServiceAnalytics: unknown }>(queryStr, { page: p, limit: 10 }),
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

    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load services");
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
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load services");
    } finally {
      setLoading(false);
    }
  }, [page, isAuth, query]);

  const loadVisibleData = useCallback(async (p: number = page) => {
    if (view === "catalog" || !isAuth) {
      await loadCatalogData(p);
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
      const res = await api.graphql<{ searchServices: Service[] }>(queryStr, { q });
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
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed to add service to workspace");
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
      } catch (err) {
        toast.error(err instanceof Error ? err.message : "Failed to remove service");
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
      } catch (err) {
        toast.error(err instanceof Error ? err.message : "Failed to delete service");
      }
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

      {isAuth && (
        <div className="flex bg-slate-100 p-1 rounded-lg w-full sm:w-fit mb-6">
          <button
            data-track="view_workspace_tab"
            type="button"
            onClick={() => setView("workspace")}
            className={`flex-1 sm:flex-none px-3 sm:px-4 py-1.5 text-sm font-medium rounded-md transition-all ${view === "workspace" ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700 hover:bg-slate-200/50"} cursor-pointer`}
          >
            Workspace services
          </button>
          <button
            data-track="view_catalog_tab"
            type="button"
            onClick={() => setView("catalog")}
            className={`flex-1 sm:flex-none px-3 sm:px-4 py-1.5 text-sm font-medium rounded-md transition-all ${view === "catalog" ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700 hover:bg-slate-200/50"} cursor-pointer`}
          >
            Catalog
          </button>
          <button
            data-track="view_imports_tab"
            type="button"
            onClick={() => setView("pending")}
            className={`relative flex-1 sm:flex-none px-3 sm:px-4 py-1.5 text-sm font-medium rounded-md transition-all ${view === "pending" ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700 hover:bg-slate-200/50"} cursor-pointer`}
          >
            Imports
            {activeSessions.length > 0 && <span className="ml-2 text-[10px] font-bold text-amber-700">{activeSessions.length}</span>}
          </button>
        </div>
      )}

      {/* Active Sessions View */}
      {isAuth && view === "pending" && (
        <div className="mb-6">
          <IntegrationsPendingTab
            activeSessions={activeSessions}
            setNewSessionId={setNewSessionId}
            onRefresh={refreshSessions}
          />
        </div>
      )}

      {view === "catalog" || !isAuth ? (
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

      {/* Define a service side panel */}
      {showNewPanel && (
        <DefineServiceDrawer
          isAuth={isAuth}
          onClose={() => setShowNewPanel(false)}
          onCancel={() => {
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
          importPlan={importPlan}
          newName={newName}
          setNewName={setNewName}
          isSlugManuallyEdited={isSlugManuallyEdited}
          setIsSlugManuallyEdited={setIsSlugManuallyEdited}
          newSlug={newSlug}
          setNewSlug={setNewSlug}
          requireVersion={requireVersion}
          setRequireVersion={setRequireVersion}
          importMethod={importMethod}
          setImportMethod={setImportMethod}
          newVersion={newVersion}
          setNewVersion={setNewVersion}
          sourceType={sourceType}
          setSourceType={setSourceType}
          newUrl={newUrl}
          setNewUrl={setNewUrl}
          newContent={newContent}
          setNewContent={setNewContent}
          fileName={fileName}
          setFileName={setFileName}
          starting={starting}
          startError={startError}
          setStartError={setStartError}
          handleStart={handleStart}
          handleChangeImportSource={handleChangeImportSource}
        />
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
