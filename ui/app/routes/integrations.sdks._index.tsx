import { hasAnyAppPermission } from "~/lib/current-actor-access";
import { useState, useEffect } from "react";
import { useNavigate, useSearchParams, type MetaFunction } from "@remix-run/react";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m) => m.id === "root").flatMap((m) => m.meta ?? []);
  return [
    ...parentMeta.filter((m) => !('title' in m)),
    { title: "Apps - Fused" },
  ];
};
import { Archive as ArchiveIcon, Download, Globe2, Package, Play, ServerCrash, TerminalSquare, Trash2, Loader2, Search, X } from "lucide-react";
import { api } from "~/lib/api";
import { useToast } from "~/components/Toast";
import { AppRuntimeStatus } from "~/components/apps/AppRuntimeStatus";
import { formatAppDownloadCount } from "~/lib/app-downloads";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasResourcePermission, hasWorkspacePermission } from "~/lib/current-actor-access";
import { CreateAppMenu } from "~/components/apps/CreateAppMenu";

interface SdkListItem {
  app_id: string | null;
  app_family_id: string;
  name: string;
  description?: string;
  version: string | null;
  version_count: number;
  kind: "sdk" | "mcp";
  delivery_mode?: "sdk" | "api";
  target_type: string;
  target_language?: string;
  sandbox_url?: string;
  is_downloadable?: boolean;
  has_update_available?: boolean;
  has_deprecated_endpoints?: boolean;
  created_at?: string;
  killed_at?: string;
  downloads?: string | null;
  status: string | null;
  archived_at?: string;
}

type SdkPage = { items: SdkListItem[]; total: number };
type AppCatalogueState = "active" | "archive";
type McpLifecycleAction = "deprecate" | "restore";

const SDK_PAGE_SIZE = 20;

/** Resolves live or archived family discovery from URL state. */
function appCatalogueState(value: string | null): AppCatalogueState {
  // Only the explicit archive value may expose retired family history.
  if (value === "archive") return "archive";
  return "active";
}

/** Names the server-side catalogue scope currently searched by the text field. */
function appSearchPlaceholder(state: AppCatalogueState): string {
  // Archived discovery is distinct enough to name explicitly; the default keeps the established concise label.
  if (state === "archive") return "Search archived apps...";
  return "Search apps...";
}

/** Returns the existing exact-version detail route for the row's runtime adapter. */
function appDetailPath(app: SdkListItem, appId: string): string {
  // MCP retains its transport-specific detail controls; SDK and REST share the SDK-kind detail projection.
  if (app.target_type === "mcp") return `/integrations/mcp/${appId}`;
  return `/integrations/sdks/${appId}`;
}

/** Describes the delivery-specific state removed by app deactivation. */
function appRemovalScope(targetType: string, plural = false): string {
  // Generated SDKs own package artifacts in addition to the runtime shared by every app view.
  if (targetType === "sdk") return plural ? "runtimes and packages" : "runtime and package";
  return plural ? "runtimes" : "runtime";
}

/** Converts persisted kind and delivery metadata into the row's user-facing delivery type. */
function appTargetType(app: Pick<SdkListItem, "kind" | "delivery_mode">): "sdk" | "mcp" | "api" {
  // MCP owns its runtime adapter; SDK-kind families split only by generated-package or direct-REST delivery.
  if (app.kind === "mcp") return "mcp";
  return app.delivery_mode === "api" ? "api" : "sdk";
}

/** Returns concise product copy for one delivery type without changing family identity. */
function appTypeLabel(targetType: string): string {
  // Labels describe delivery, not a third persistence kind.
  if (targetType === "mcp") return "MCP server";
  if (targetType === "api") return "REST API";
  return "SDK";
}

/** Reads one mixed Engine-grouped application page, matching the CLI catalogue contract. */
function readAppPage(query: string, page: number, state: AppCatalogueState): Promise<SdkPage> {
  const document = `
    query Applications($search: String!, $archived: Boolean!, $limit: Int!, $offset: Int!) {
      appFamilies(search: $search, archived: $archived, limit: $limit, offset: $offset) {
        items {
          app_family_id
          app_id: latest_version_id
          name
          version: latest_version
          version_count
          kind
          delivery_mode
          target_language
          created_at: latest_created_at
          status: latest_status
          archived_at
          downloads
        }
        total
      }
    }
  `;
  return api
    .mcpGraphql<{ appFamilies: SdkPage }>(document, {
      search: query.trim(),
      archived: state === "archive",
      limit: SDK_PAGE_SIZE,
      offset: page * SDK_PAGE_SIZE,
    })
    .then(({ appFamilies }) => appFamilies);
}

/** Renders the compact generated-client language mark used in catalogue rows. */
function LanguageBadge({ targetLanguage }: { targetLanguage?: string }) {
  if (targetLanguage === "python") {
    return (
      <span className="inline-flex items-center justify-center w-5 h-5" title="Python" aria-label="Python">
        <svg viewBox="0 0 32 32" className="w-3.5 h-3.5 shrink-0" aria-hidden="true">
          <path fill="#3776AB" d="M15.9 3c-6.3 0-5.9 2.7-5.9 2.7v2.8h6v.9H7.6S3 8.9 3 15.9 7 22.6 7 22.6h2.4v-3.4s-.1-4 4-4h6.7s3.7.1 3.7-3.5V5.9S24.4 3 18 3h-2.1Zm-3.3 2a1.2 1.2 0 1 1 0 2.3 1.2 1.2 0 0 1 0-2.3Z"/>
          <path fill="#FFD43B" d="M16.1 29c6.3 0 5.9-2.7 5.9-2.7v-2.8h-6v-.9h8.4s4.6.5 4.6-6.5-4-6.7-4-6.7h-2.4v3.4s.1 4-4 4H12s-3.7-.1-3.7 3.5v5.8S7.6 29 14 29h2.1Zm3.3-2a1.2 1.2 0 1 1 0-2.3 1.2 1.2 0 0 1 0 2.3Z"/>
        </svg>
      </span>
    );
  }

  return (
    <span className="inline-flex items-center justify-center w-5 h-5" title="TypeScript" aria-label="TypeScript">
      <svg viewBox="0 0 32 32" className="w-3.5 h-3.5 shrink-0" aria-hidden="true">
        <rect x="3" y="3" width="26" height="26" rx="3" fill="#3178C6" />
        <path fill="#fff" d="M11.3 13.1h9.8v2.2h-3.7V26h-2.4V15.3h-3.7v-2.2Zm10.6 0h-2.4v8.5c0 2.5 1.3 4.4 4.7 4.4 1.2 0 2.5-.3 3.5-.8v-2.1c-.9.5-1.8.8-2.7.8-1.7 0-3.1-.8-3.1-2.6v-8.2Z"/>
      </svg>
    </span>
  );
}

interface SdkRowProps {
  sdk: SdkListItem;
  canManage: boolean;
  selectedIds: string[];
  setSelectedIds: React.Dispatch<React.SetStateAction<string[]>>;
  onNavigate: (id: string) => void;
  onDownload: (id: string, name: string, version: string) => void;
  onDeactivate: (id: string, name: string, version: string) => void;
	onArchive: (id: string, name: string) => void;
	onMcpLifecycle: (sdk: SdkListItem, action: McpLifecycleAction) => void;
	archived: boolean;
}

/** Renders app identity and runtime state without duplicating row actions. */
function SdkNameCell({ sdk, archived }: { sdk: SdkListItem; archived: boolean }) {
  return (
    <div className="min-w-0">
      <div className="flex min-w-0 items-center gap-2">
        <span className="block min-w-0 truncate font-semibold text-slate-900">{sdk.name}</span>
        {/* Delivery badges make the mixed catalogue scannable without splitting one app lifecycle into tabs. */}
        {sdk.target_type === "mcp" && (
          <span className="inline-flex items-center px-2 py-0.5 rounded text-[10px] font-bold bg-violet-100 text-violet-700 uppercase tracking-wider">
            MCP
          </span>
        )}
        {sdk.target_type !== "mcp" && (
          <span className="inline-flex items-center px-2 py-0.5 rounded text-[10px] font-bold bg-emerald-100 text-emerald-700 uppercase tracking-wider">
            REST
          </span>
        )}
        {sdk.target_type === "sdk" && (
          <LanguageBadge targetLanguage={sdk.target_language} />
        )}
      </div>
      {/* Archive rows describe deleted logical identities; live empty families remain eligible for explicit deletion. */}
      {archived ? (
		<span className="mt-0.5 block text-xs text-slate-400">Deleted</span>
	  ) : sdk.status ? (
		<AppRuntimeStatus className="mt-0.5" status={sdk.status} />
	  ) : (
		<span className="mt-0.5 block text-xs text-slate-400">No active versions</span>
	  )}
    </div>
  );
}

/** Renders the adapter-specific glyph inside the shared app row container. */
function AppCatalogueIcon({ view }: { view: string }) {
  // The icon mirrors the active delivery surface without changing app-family identity.
  if (view === "mcp") return <TerminalSquare className="w-4 h-4" />;
  if (view === "api") return <Globe2 className="w-4 h-4" />;
  return <Package className="w-4 h-4" />;
}

/** Renders the latest version and family version count for one application. */
function SdkVersionBadges({ sdk }: { sdk: SdkListItem }) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      {/* Latest is catalogue metadata only; detail navigation remains pinned to its exact version ID. */}
      <span className="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-slate-100 text-slate-700 border border-slate-200">
        {sdk.version ?? "—"}
      </span>
      <span className="text-xs text-slate-400">{sdk.version_count} {sdk.version_count === 1 ? "version" : "versions"}</span>
      {sdk.killed_at && (
        <span className="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-slate-100 text-slate-500 border border-slate-200">
          Killed
        </span>
      )}
      {sdk.has_deprecated_endpoints && (
        <span className="inline-flex items-center px-2 py-0.5 rounded text-[10px] font-bold bg-red-100 text-red-700 border border-red-200 uppercase tracking-wider">
          Deprecated Endpoints
        </span>
      )}
      {sdk.has_update_available && (
        <span className="inline-flex items-center px-2 py-0.5 rounded text-[10px] font-bold bg-green-100 text-green-700 border border-green-200 uppercase tracking-wider">
          Update Available
        </span>
      )}
    </div>
  );
}

interface SdkActionButtonsProps {
  sdk: SdkListItem;
  onDownload: (id: string, name: string, version: string) => void;
  onDeactivate: (id: string, name: string, version: string) => void;
	onArchive: (id: string, name: string) => void;
	onMcpLifecycle: (sdk: SdkListItem, action: McpLifecycleAction) => void;
	archived: boolean;
}

/** Renders exact-version actions for the latest version represented by a family row. */
function SdkActionButtons({ sdk, onDownload, onDeactivate, onArchive, onMcpLifecycle, archived }: SdkActionButtonsProps) {
	// Archived history is read-only and cannot acquire runtime or lifecycle actions.
	if (archived) return null;
	// A live empty family exposes only the final deletion action that releases its name.
	if (!sdk.app_id || !sdk.version) {
		return (
		  <button
			onClick={(event) => { event.stopPropagation(); onArchive(sdk.app_family_id, sdk.name); }}
			className="inline-flex items-center justify-center w-8 h-8 text-sm font-medium text-red-600 bg-red-50 hover:bg-red-100 rounded-lg transition-colors cursor-pointer"
			title="Delete app and release its name"
		  >
			<ArchiveIcon className="w-4 h-4" />
		  </button>
		);
	}
  return (
    <div className="flex justify-end gap-1 sm:gap-2">
      {/* Package downloads apply only to generated SDKs; MCP and REST apps retain the same row without a fake action. */}
      {sdk.target_type === "sdk" ? (
        sdk.is_downloadable ? (
          <button
            onClick={(e) => { e.stopPropagation(); onDownload(sdk.app_id!, sdk.name, sdk.version!); }}
            className="inline-flex items-center justify-center w-8 h-8 text-sm font-medium text-blue-600 bg-blue-50 hover:bg-blue-100 rounded-lg transition-colors cursor-pointer"
            title="Download latest SDK version"
          >
            <Download className="w-4 h-4" />
          </button>
        ) : (
          <button
            disabled
            className="inline-flex items-center justify-center w-8 h-8 text-sm font-medium text-slate-400 bg-slate-100 rounded-lg cursor-not-allowed"
            title="This SDK has expired and its files have been cleaned up."
          >
            <Download className="w-4 h-4" />
          </button>
        )
      ) : null}
      {/* MCP keeps its reversible deprecation controls inside the shared app-row action area. */}
      {sdk.target_type === "mcp" && sdk.status === "active" ? (
        <button type="button" onClick={(event) => { event.stopPropagation(); onMcpLifecycle(sdk, "deprecate"); }} className="inline-flex h-8 w-8 items-center justify-center rounded-lg bg-amber-50 text-amber-700 hover:bg-amber-100" title="Deprecate MCP server"><ServerCrash className="h-4 w-4" /></button>
      ) : sdk.target_type === "mcp" && sdk.status === "deprecated" ? (
        <button type="button" onClick={(event) => { event.stopPropagation(); onMcpLifecycle(sdk, "restore"); }} className="inline-flex h-8 w-8 items-center justify-center rounded-lg bg-emerald-50 text-emerald-700 hover:bg-emerald-100" title="Restore MCP server"><Play className="h-4 w-4" /></button>
      ) : null}
      <button
        onClick={(e) => { e.stopPropagation(); onDeactivate(sdk.app_id, sdk.name, sdk.version); }}
        className="inline-flex items-center justify-center w-8 h-8 text-sm font-medium text-red-600 bg-red-50 hover:bg-red-100 rounded-lg transition-colors cursor-pointer"
        title="Deactivate latest app version"
      >
        <Trash2 className="w-4 h-4" />
      </button>
    </div>
  );
}

/** Formats the live creation or retained deletion date used by a catalogue row. */
function appCatalogueDate(sdk: SdkListItem, archived: boolean): string {
  const value = archived ? sdk.archived_at : sdk.created_at;
  // Missing family timestamps remain blank rather than inventing a lifecycle date.
  if (!value) return "";
  return new Date(value).toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" });
}

/** Returns cursor styling only when a row can navigate to an exact immutable version. */
function appRowCursor(appId: string | null): string {
  // Versionless live families and archived families have no detail route.
  return appId ? "cursor-pointer" : "cursor-default";
}

/** Keeps the exact-version selector discoverable without exposing it on versionless rows. */
function selectionControlOpacity(appId: string | null, showCheckbox: boolean): string {
  // A missing immutable identity cannot participate in bulk version actions.
  if (!appId) return "pointer-events-none opacity-0";
  return showCheckbox ? "opacity-100" : "opacity-0 group-hover:opacity-100 focus-within:opacity-100";
}

/** Renders one family catalogue row that opens its latest exact version. */
function SdkRow({ sdk, canManage, selectedIds, setSelectedIds, onNavigate, onDownload, onDeactivate, onArchive, onMcpLifecycle, archived }: SdkRowProps) {
  const appId = sdk.app_id;
  // Only a family with a latest immutable version can participate in version-scoped navigation or selection.
  const isSelected = appId ? selectedIds.includes(appId) : false;
  const showCheckbox = selectedIds.length > 0 || isSelected;
  return (
    <tr
	  className={`hover:bg-slate-50/50 transition-colors group ${appRowCursor(appId)}`}
      onClick={() => {
        // Family rows open the deterministic latest immutable version when one exists.
        if (appId) onNavigate(appId);
      }}
    >
      <td className="px-3 sm:px-6 py-4 min-w-0">
        <div className="flex min-w-0 items-center gap-2 sm:gap-3">
          <div className="relative w-8 h-8 rounded shrink-0">
            {/* Management controls require an exact family grant and never appear for read-only rows. */}
            {canManage && (
			  <div className={`absolute inset-0 z-10 bg-white/90 rounded flex items-center justify-center transition-opacity duration-200 ${selectionControlOpacity(appId, showCheckbox)}`} onClick={(e) => e.stopPropagation()}>
                <input
                  type="checkbox"
                  className="rounded border-slate-300 text-blue-600 focus:ring-blue-500 w-4 h-4 cursor-pointer"
                  checked={isSelected}
                  onChange={() => {
                    // Selection remains exact-version scoped even though each row represents a family.
                    if (!appId) return;
                    setSelectedIds(prev => prev.includes(appId) ? prev.filter(i => i !== appId) : [...prev, appId]);
                  }}
                />
              </div>
            )}
            <div className="absolute inset-0 w-8 h-8 rounded bg-blue-100 flex items-center justify-center text-blue-600">
              <AppCatalogueIcon view={sdk.target_type} />
            </div>
          </div>
		  <SdkNameCell sdk={sdk} archived={archived} />
        </div>
      </td>
      <td className="px-2 sm:px-6 py-4">
        <SdkVersionBadges sdk={sdk} />
      </td>
      <td className="hidden md:table-cell px-6 py-4 text-slate-500 font-medium">
        {formatAppDownloadCount(sdk.downloads)}
      </td>
      <td className="hidden lg:table-cell px-6 py-4 text-slate-500">
        {appCatalogueDate(sdk, archived)}
      </td>
      <td className="px-2 sm:px-6 py-4 text-right">
		{canManage ? <SdkActionButtons sdk={sdk} onDownload={onDownload} onDeactivate={onDeactivate} onArchive={onArchive} onMcpLifecycle={onMcpLifecycle} archived={archived} /> : null}
      </td>
    </tr>
  );
}

interface SdkListContentProps {
	state: AppCatalogueState;
  canCreate: boolean;
  canManage: (sdk: SdkListItem) => boolean;
  loading: boolean;
  searching: boolean;
  sdks: SdkListItem[];
  query: string;
  selectedIds: string[];
  setSelectedIds: React.Dispatch<React.SetStateAction<string[]>>;
  navigate: (path: string) => void;
  onDownload: (id: string, name: string, version: string) => void;
  onDeactivate: (id: string, name: string, version: string) => void;
	onArchive: (id: string, name: string) => void;
	onMcpLifecycle: (sdk: SdkListItem, action: McpLifecycleAction) => void;
}

/** Renders the unified empty catalogue without coupling it to lifecycle-state selection. */
function AppCatalogueEmpty({ state, query, canCreate }: { state: AppCatalogueState; query: string; canCreate: boolean }) {
	const archived = state === "archive";
  return (
    <div className="bg-white rounded-xl border border-slate-200 p-12 text-center">
      <div className="mx-auto mb-4 flex h-12 w-12 items-center justify-center text-slate-300"><Package className="h-6 w-6" /></div>
      {/* Lifecycle-aware copy distinguishes an empty history from a workspace that has never created an app. */}
      <h3 className="text-lg font-medium text-slate-900 mb-1">
		{query ? `No ${archived ? "archived " : ""}apps found` : archived ? "No archived apps" : "No apps yet"}
      </h3>
      <p className="text-slate-500 max-w-md mx-auto">
		{archived
		  ? "Deleted apps appear here after their final version is deactivated and their family is removed."
		  : query
          ? "No apps match your search."
          : "Create an app to give it reusable access to selected services and operations."}
      </p>
	  {/* Creation stays in one catalogue while the menu makes the delivery adapter explicit. */}
	  {!archived && !query && canCreate && (
		<CreateAppMenu className="mt-5 inline-block" />
      )}
    </div>
  );
}

/** Selects the loading, empty, or populated app-list presentation. */
function SdkListContent({ state, canCreate, canManage, loading, searching, sdks, query, selectedIds, setSelectedIds, navigate, onDownload, onDeactivate, onArchive, onMcpLifecycle }: SdkListContentProps) {
	const archived = state === "archive";
	// Loading and debounced search share one bounded placeholder to prevent stale rows from flashing.
  if (loading || searching) {
    return (
      <div className="flex flex-col items-center justify-center py-20 text-slate-400">
        <Loader2 className="w-8 h-8 text-blue-500 animate-spin mb-4" />
        <p className="animate-pulse font-medium text-slate-500">Loading apps...</p>
      </div>
    );
  }
	// The lifecycle-specific empty state explains whether creation or deletion history is absent.
  if (sdks.length === 0) {
	return <AppCatalogueEmpty state={state} query={query} canCreate={canCreate} />;
  }
  // Only families with a live immutable version participate in version-scoped bulk actions.
  const selectableIds = sdks.flatMap((sdk) => sdk.app_id && canManage(sdk) ? [sdk.app_id] : []);
  return (
    <div className="bg-white rounded-xl border border-slate-200 overflow-x-auto">
      <table className="w-full table-fixed md:table-auto text-left text-sm whitespace-nowrap">
        <thead className="bg-slate-50 border-b border-slate-200 text-slate-500">
          <tr>
            <th className="w-[55%] md:w-auto px-3 sm:px-6 py-4 font-medium">
              <div className="flex items-center gap-3">
                <div className="flex items-center justify-center w-8 h-8">
                  <div className={`transition-opacity duration-200 ${selectedIds.length > 0 ? 'opacity-100' : 'opacity-0'}`}>
                    <input
                      type="checkbox"
                      className="rounded border-slate-300 text-blue-600 focus:ring-blue-500 w-4 h-4 cursor-pointer"
                      checked={selectedIds.length === selectableIds.length && selectableIds.length > 0}
                      onChange={() => {
                        // Select-all never manufactures an identity for retained versionless families.
                        if (selectedIds.length === selectableIds.length) {
                          setSelectedIds([]);
                        } else {
                          setSelectedIds(selectableIds);
                        }
                      }}
                    />
                  </div>
                </div>
                <span>App</span>
              </div>
            </th>
            <th className="w-[25%] md:w-auto px-2 sm:px-6 py-4 font-medium">Latest version</th>
            <th className="hidden md:table-cell px-6 py-4 font-medium">Downloads</th>
			<th className="hidden lg:table-cell px-6 py-4 font-medium">{archived ? "Deleted" : "Date"}</th>
            <th className="w-[20%] md:w-auto px-2 sm:px-6 py-4 font-medium text-right">Action</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-slate-100">
          {sdks.map((sdk) => (
            <SdkRow
              key={sdk.app_family_id}
              sdk={sdk}
              canManage={canManage(sdk)}
              selectedIds={selectedIds}
              setSelectedIds={setSelectedIds}
              onNavigate={(id) => navigate(appDetailPath(sdk, id))}
              onDownload={onDownload}
              onDeactivate={onDeactivate}
			  onArchive={onArchive}
			  onMcpLifecycle={onMcpLifecycle}
			  archived={archived}
            />
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** Renders bounded page navigation from the server-reported total. */
function SdkPagination({ page, total, onPage }: {
  page: number;
  total: number;
  onPage: (page: number) => void;
}) {
  const pageCount = Math.max(1, Math.ceil(total / SDK_PAGE_SIZE));
  if (pageCount <= 1) return null;
  return (
    <div className="flex items-center justify-between gap-4 text-sm text-slate-600">
      <span>Page {page + 1} of {pageCount}</span>
      <div className="flex items-center gap-2">
        <button
          type="button"
          disabled={page === 0}
          onClick={() => onPage(page - 1)}
          className="rounded-lg border border-slate-300 px-3 py-1.5 disabled:opacity-40"
        >
          Previous
        </button>
        <button
          type="button"
          disabled={page + 1 >= pageCount}
          onClick={() => onPage(page + 1)}
          className="rounded-lg border border-slate-300 px-3 py-1.5 disabled:opacity-40"
        >
          Next
        </button>
      </div>
    </div>
  );
}

/** Renders the shared Apps heading and family lifecycle actions. */
function AppsCatalogueHeader({ canCreate, selectedCount, deactivating, onDeactivateSelected }: {
  canCreate: boolean;
  selectedCount: number;
  deactivating: boolean;
  onDeactivateSelected: () => void;
}) {
  return (
    <div className="flex flex-col sm:flex-row sm:items-start justify-between gap-4">
      <div className="min-w-0">
        <h1 className="text-xl font-semibold text-slate-900">Apps</h1>
        <p className="text-slate-500 text-sm mt-1">Choose which services and operations an app can use.</p>
      </div>
      <div className="flex w-full sm:w-auto items-center gap-3">
        {/* Bulk lifecycle controls appear only after the user selects manageable exact versions. */}
        {selectedCount > 0 && (
          <button
            onClick={onDeactivateSelected}
            disabled={deactivating}
            className="inline-flex flex-1 sm:flex-none items-center justify-center gap-2 px-4 py-2 bg-rose-50 text-rose-600 hover:bg-rose-100 text-sm font-medium rounded-lg transition-all border border-rose-200 cursor-pointer"
          >
            <Trash2 className="w-4 h-4" />
            {deactivating ? "Deactivating..." : `Deactivate selected (${selectedCount})`}
          </button>
        )}
        {/* One disclosure routes each adapter into the same app builder workflow. */}
        {canCreate && (
		  <CreateAppMenu className="flex-1 sm:flex-none" />
        )}
      </div>
    </div>
  );
}

/** Renders archive as the same compact checkbox filter used by other Fused catalogue lists. */
function AppArchiveFilter({ archived, onChange }: { archived: boolean; onChange: (state: AppCatalogueState) => void }) {
  /** Toggles only the selected app type between its live catalogue and retained family history. */
  const toggleArchive = () => {
    // The switch is binary: turning archive off always returns to the live catalogue.
    onChange(archived ? "active" : "archive");
  };

  return (
    <label className="inline-flex h-9 shrink-0 cursor-pointer items-center gap-1.5 px-1 text-sm text-slate-500 hover:text-slate-800">
      <input
        type="checkbox"
        checked={archived}
        onChange={toggleArchive}
        className="h-4 w-4 cursor-pointer rounded border-slate-300 text-blue-600 focus:ring-blue-500"
      />
      Archived
    </label>
  );
}

/** Renders the paged SDK catalogue and its lifecycle controls. */
export default function SdkHistory() {
  const toast = useToast();
  const { access } = useCurrentActorAccess();
  const [searchParams, setSearchParams] = useSearchParams();
	const state = appCatalogueState(searchParams.get("state"));
  const [sdks, setSdks] = useState<SdkListItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [query, setQuery] = useState(searchParams.get("q") ?? "");
  const [searching, setSearching] = useState(false);
  const [page, setPage] = useState(0);
  const [total, setTotal] = useState(0);
  const [selectedIds, setSelectedIds] = useState<string[]>([]);
  const [isDeactivatingMultiple, setIsDeactivatingMultiple] = useState(false);
  const navigate = useNavigate();
  const canRead = hasAnyAppPermission(access, "read");
  const canCreate = ["sdk", "mcp", "api"].some((kind) => hasWorkspacePermission(access, `app.${kind}.create`));

  /** Checks lifecycle management for the exact family represented by one catalogue row. */
  const canManage = (sdk: SdkListItem) => hasResourcePermission(access, `app.${appTargetType(sdk)}.manage`, "APP", sdk.app_family_id);

  /** Loads one list or search page through the same paged contract. */
  const fetchSdks = (search: string, pageNumber: number) => {
    // Creation does not imply read; an empty catalogue keeps permitted creation available.
    if (!canRead) { setSdks([]); setTotal(0); setLoading(false); setSearching(false); return Promise.resolve(); }
    const isSearch = Boolean(search.trim());
    setLoading(!isSearch);
    setSearching(isSearch);
    setError("");
	return readAppPage(search, pageNumber, state)
      .then(result => {
        // Deactivation can empty the last page; rewinding avoids presenting a
        // false empty state while earlier authorized results still exist.
        if (pageNumber > 0 && result.items.length === 0 && result.total > 0) {
          setPage(pageNumber - 1);
          return;
        }
        // Persisted family metadata, not a UI tab, determines each row's delivery surface.
        setSdks((result.items ?? []).map(item => ({
          ...item,
          target_type: appTargetType(item),
          is_downloadable: item.kind === "sdk" && item.delivery_mode !== "api",
          downloads: item.kind === "sdk" && item.delivery_mode !== "api" ? item.downloads : null,
        })));
        setTotal(result.total);
        setSelectedIds([]);
      })
      .catch(e => {
        setSdks([]);
        setTotal(0);
        setError(e instanceof Error ? e.message : "Failed to load apps");
      })
      .finally(() => {
        setLoading(false);
        setSearching(false);
      });
  };

  /** Selects the first page for an explicitly submitted search. */
  async function runSearch(q: string) {
    if (!q.trim()) return;
    setSearchParams(prev => {
      const next = new URLSearchParams(prev);
      next.set("q", q);
      return next;
    }, { replace: true });
    // Changing pages triggers the shared effect; an already-first page needs
    // the explicit read so submitting can refresh without duplicate requests.
    if (page > 0) setPage(0);
    else await fetchSdks(q, 0);
  }

  // Debounced search on type
  useEffect(() => {
    if (!query.trim()) {
      fetchSdks("", page);
      return;
    }
    const id = setTimeout(() => fetchSdks(query, page), 400);
    return () => clearTimeout(id);
	}, [page, query, state, canRead]);

	/** Switches between live apps and retained deletion history without changing the adapter tab. */
	function selectState(nextState: AppCatalogueState) {
		setPage(0);
		setSelectedIds([]);
		setSearchParams(previous => {
			const next = new URLSearchParams(previous);
			// Live is the default so only Archive needs a persistent URL marker.
			if (nextState === "archive") next.set("state", "archive");
			else next.delete("state");
			return next;
		}, { replace: true });
	}

  /** Runs the current search immediately when the form is submitted. */
  function handleSearch(e: React.FormEvent) {
    e.preventDefault();
    runSearch(query);
  }

  /** Clears search state and lets the list effect load the first page. */
  function handleClear() {
    setQuery("");
    setPage(0);
    setSearchParams(prev => {
      const next = new URLSearchParams(prev);
      next.delete("q");
      return next;
    }, { replace: true });
    setSelectedIds([]);
  }

  /** Downloads the exact immutable package selected by the row. */
  const handleDownload = async (id: string, name: string, version: string) => {
    try {
      await api.sdks.download(id, name, version);
      await fetchSdks(query, page);
    } catch {
      toast.error("Failed to download SDK");
    }
  };

  /** Confirms and deactivates one immutable app version. */
  const handleDeactivate = async (id: string, name: string, version: string) => {
    const app = sdks.find((candidate) => candidate.app_id === id);
    const targetType = app?.target_type ?? "sdk";
    const typeLabel = appTypeLabel(targetType);
    const confirmed = await toast.confirm(`Deactivate ${typeLabel} "${name}" version "${version}"? This permanently removes its ${appRemovalScope(targetType)}.`);
    if (!confirmed) return;
    try {
      await api.sdks.deactivate(id);
      fetchSdks(query, page);
      toast.success(`${typeLabel} version "${name}" deactivated.`);
      setSelectedIds(prev => prev.filter(i => i !== id));
    } catch (err) {
      toast.error(`Failed to deactivate ${typeLabel}: ${err instanceof Error ? err.message : "Unknown error"}`);
    }
  };

	/** Confirms final family deletion, which releases the name and retains a read-only archive row. */
	const handleArchive = async (id: string, name: string) => {
		const app = sdks.find((candidate) => candidate.app_family_id === id);
		const typeLabel = appTypeLabel(app?.target_type ?? "sdk");
		const confirmed = await toast.confirm(`Delete ${typeLabel} "${name}"? Its name becomes reusable and its historical identity remains in Archive.`);
		// Only explicit confirmation may release the canonical name.
		if (!confirmed) return;
		try {
			await api.sdks.archiveFamily(id);
			await fetchSdks(query, page);
			toast.success(`${typeLabel} "${name}" deleted and moved to Archive.`);
		} catch (err) {
			toast.error(`Failed to delete ${typeLabel}: ${err instanceof Error ? err.message : "Unknown error"}`);
		}
	};

  /** Confirms and applies a reversible MCP lifecycle transition without leaving the shared catalogue. */
  const handleMcpLifecycle = async (sdk: SdkListItem, action: McpLifecycleAction) => {
    const label = action === "deprecate" ? "Deprecate" : "Restore";
    const confirmed = await toast.confirm(`${label} MCP server "${sdk.name}"?`);
    // Cancellation must leave the exact immutable version unchanged.
    if (!confirmed || !sdk.app_id) return;
    // Deprecation carries an operator message while restoration only needs immutable app identity.
    const document = action === "deprecate"
      ? `mutation($appId: String!, $message: String!) { deprecateApp(app_id: $appId, message: $message) }`
      : `mutation($appId: String!) { undeprecateApp(app_id: $appId) }`;
    try {
      await api.mcpGraphql(document, {
        appId: sdk.app_id,
        message: "A newer MCP app version is available",
      });
      toast.success(`${sdk.name} ${action === "restore" ? "restored" : "deprecated"}.`);
      await fetchSdks(query, page);
    } catch (cause) {
      toast.error(`${label} failed: ${cause instanceof Error ? cause.message : "Unknown error"}`);
    }
  };

  /** Confirms and deactivates the selected immutable versions in parallel. */
  const handleDeactivateMultiple = async () => {
    if (selectedIds.length === 0) return;
    const confirmed = await toast.confirm(`Deactivate ${selectedIds.length} app version(s)? This permanently removes their runtimes and any generated packages.`);
    if (!confirmed) return;
    
    setIsDeactivatingMultiple(true);
    try {
      await Promise.all(selectedIds.map(id => api.sdks.deactivate(id)));
      fetchSdks(query, page);
      setSelectedIds([]);
      toast.success(`Deactivated ${selectedIds.length} app version(s).`);
    } catch (err) {
      toast.error(`Failed to deactivate some apps: ${err instanceof Error ? err.message : "Unknown error"}`);
      fetchSdks(query, page);
    } finally {
		setIsDeactivatingMultiple(false);
    }
  };

  return (
    <div className="space-y-6">
      <AppsCatalogueHeader
		canCreate={canCreate && state === "active"}
		selectedCount={state === "active" ? selectedIds.length : 0}
        deactivating={isDeactivatingMultiple}
        onDeactivateSelected={handleDeactivateMultiple}
      />

      <div className="flex flex-col gap-3 sm:flex-row sm:items-center">
        <form
          onSubmit={handleSearch}
          className="relative min-w-0 flex-1"
        >
          <button
            type="submit"
            disabled={searching}
            className="absolute left-2.5 top-1/2 -translate-y-1/2 text-slate-400 hover:text-slate-600 disabled:opacity-50 cursor-pointer"
            title="Search"
          >
            {searching ? (
              <Loader2 className="w-3.5 h-3.5 animate-spin" />
            ) : (
              <Search className="w-3.5 h-3.5" />
            )}
          </button>
          <input
            type="text"
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              setPage(0);
            }}
            placeholder={appSearchPlaceholder(state)}
            className="w-full text-sm border border-slate-300 rounded-lg pl-9 pr-8 py-2 focus:outline-none focus:ring-2 focus:ring-blue-500"
          />
          {query && (
            <button
              type="button"
              onClick={handleClear}
              className="absolute right-2.5 top-1/2 -translate-y-1/2 text-slate-400 hover:text-slate-600 cursor-pointer"
              aria-label="Clear search"
            >
              <X className="w-3.5 h-3.5" />
            </button>
          )}
        </form>
        <AppArchiveFilter archived={state === "archive"} onChange={selectState} />
      </div>

      {error && (
        <div className="mb-4 p-3 bg-red-50 border border-red-200 rounded-lg text-sm text-red-700">
          {error}
        </div>
      )}

      <SdkListContent
		state={state}
		canCreate={canCreate && state === "active"}
        canManage={canManage}
        loading={loading}
        searching={searching}
        sdks={sdks}
        query={query}
        selectedIds={selectedIds}
        setSelectedIds={setSelectedIds}
        navigate={navigate}
        onDownload={handleDownload}
        onDeactivate={handleDeactivate}
		onArchive={handleArchive}
        onMcpLifecycle={handleMcpLifecycle}
      />
      {!loading && !searching && <SdkPagination page={page} total={total} onPage={setPage} />}
    </div>
  );
}
