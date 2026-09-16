import { useState, useEffect, isValidElement, type ReactNode } from "react";
import { useParams, Link, useNavigate, useSearchParams, type MetaFunction } from "@remix-run/react";
import { Download, Copy, Check, Database } from "lucide-react";
import { api, type NotificationServiceRef, type WorkspaceNotification } from "~/lib/api";
import { useToast } from "~/components/Toast";
import { readBucketsForSDK } from "~/lib/buckets";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { NotificationBanner } from "~/components/notifications/NotificationBanner";
import { useWorkspaceNotifications } from "~/components/notifications/useWorkspaceNotifications";
import { isPending, matchesConfig } from "~/components/notifications/notificationHelpers";
import { AppRequestsPanel } from "~/components/activity/AppRequestsPanel";
import { AppActivityOverview } from "~/components/activity/AppActivityOverview";
import { type NestedActivityTabOption } from "~/components/activity/NestedActivityTabs";
import { AppDetailBackLink, AppDetailHeader, AppDetailPrimaryAction, AppDetailTabs, AppVersionSwitcher, type AppDetailTab } from "~/components/apps/AppDetailChrome";
import { AppActivityBody, AppChangesBody, AppDetailBody, AppDetailSection, AppOverviewBody } from "~/components/apps/AppDetailBody";
import { type AppVersionHistoryItem } from "~/components/apps/AppVersionHistory";
import { type Bucket } from "~/lib/api";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasAnyPermission, hasResourcePermission, hasWorkspacePermission } from "~/lib/current-actor-access";
import type { CurrentActorAccess } from "~/lib/current-actor-access";
import { appConnectedServiceSelections, type AppConnectedServiceSelection, type AppServiceSummary } from "~/lib/app-connected-services";
import type { AppSelectionPayload } from "~/lib/app-selection-v3";

type SdkSelection = AppConnectedServiceSelection;

type SdkPrimaryTab = "overview" | "docs" | "activity" | "changes";
type SdkActivitySection = "overview" | "requests";

/** Resolves the primary detail tab from a URL value. */
function sdkPrimaryTab(value: string | null): SdkPrimaryTab {
  // Primary navigation is shared across SDK and REST details; only Docs remains package-specific.
  if (value === "docs" || value === "activity" || value === "changes") return value;
  // Older Activity links continue to land on execution activity without preserving the obsolete label.
  if (value === "analytics") return "activity";
  return "overview";
}

/** Resolves the nested activity section from a URL value. */
function sdkActivitySection(value: string | null): SdkActivitySection {
  // SDK and REST add only Requests beneath the shared Activity shell.
  if (value === "requests") return value;
  return "overview";
}

/** Returns a node only when its presentation condition is satisfied. */
function optionalNode(show: boolean, node: ReactNode): ReactNode {
  return show ? node : null;
}

/** Identifies apps that expose a hosted MCP sandbox URL. */
function hasSandboxURL(sdk: Sdk): boolean {
  return sdk.target_type === "mcp" && Boolean(sdk.sandbox_url);
}

/** Checks whether documentation content is selected and available. */
function showsSdkDocs(tab: SdkPrimaryTab, sdk: Sdk): boolean {
  return tab === "docs" && Boolean(sdk.readme);
}

/** Maps an app target to the execution transport contract. */
function sdkTransport(sdk: Sdk): "sdk" | "mcp" {
  return sdk.target_type === "mcp" ? "mcp" : "sdk";
}

/** Filters outstanding notifications to the current immutable app version. */
function pendingSdkNotifications(sdk: Sdk | null, items: WorkspaceNotification[]): WorkspaceNotification[] {
  if (!sdk) return [];
  const configKey = `${sdk.target_type}:${sdk.name}:${sdk.version}`;
  return items.filter((item) => isPending(item) && sdkSelectionsMatchNotification(sdk, item, configKey));
}

/** Checks whether any bundled service matches one notification. */
function sdkSelectionsMatchNotification(sdk: Sdk, item: WorkspaceNotification, configKey: string): boolean {
  return (sdk.detailed_selections ?? []).some((selection) =>
    matchesConfig(item, configKey, selection.service_id, selection.service_version_name ?? "")
  );
}

/** Combines app-family read access with workspace audit access. */
function canReadSdkActivity(access: CurrentActorAccess | null, sdk: Sdk | null): boolean {
  if (!sdk) return false;
  return hasResourcePermission(access, "app.read", "APP", sdk.app_family_id) && hasWorkspacePermission(access, "audit.read");
}

/** Checks exact family management without adding permission branches to the detail controller. */
function canManageAppVersions(access: CurrentActorAccess | null, sdk: Sdk | null): boolean {
  // Version deletion cannot be authorized until the loaded app establishes its family identity.
  if (!sdk) return false;
  return hasResourcePermission(access, "app.manage", "APP", sdk.app_family_id);
}

/** Resolves permission-safe detail tabs without mounting execution views for lifecycle-only managers. */
function accessibleSdkDetailState(requestedTab: SdkPrimaryTab, requestedSection: SdkActivitySection, canReadActivity: boolean) {
  // A direct Activity URL falls back unless the actor can read execution data; Changes remains a separate lifecycle surface.
  const activeTab = requestedTab === "activity" && !canReadActivity ? "overview" : requestedTab;
  return { activeTab, activitySection: requestedSection };
}

/** Lists only activity sections the current family permissions can safely mount. */
function sdkActivityOptions(canReadActivity: boolean): Array<NestedActivityTabOption<SdkActivitySection>> {
  // Denied Activity views never mount their data consumers even if stale URL state remains.
  if (!canReadActivity) return [];
  return [
    { value: "overview", label: "Overview" },
    { value: "requests", label: "Requests" },
  ];
}

type Sdk = {
  app_id: string;
  app_family_id: string;
  name: string;
  description?: string;
  version: string;
  kind: string;
  delivery_mode: string;
  target_type: string;
  target_language?: string;
  sandbox_url?: string;
  is_downloadable?: boolean;
  created_at?: string;
  downloads?: string | null;
  readme?: string;
  detailed_selections?: SdkSelection[];
  status: string;
};

/** Builds only the SDK detail tabs supported by the loaded version and current permissions. */
function sdkPrimaryTabs(sdk: Sdk, canReadActivity: boolean): Array<AppDetailTab<SdkPrimaryTab>> {
  const tabs: Array<AppDetailTab<SdkPrimaryTab>> = [{ value: "overview", label: "Overview" }];
  // Documentation is meaningful only when this immutable version carries a README.
  if (sdk.readme) tabs.push({ value: "docs", label: "Docs" });
  // Execution Activity remains permission-gated independently from readable lifecycle history.
  if (canReadActivity) tabs.push({ value: "activity", label: "Activity" });
  tabs.push({ value: "changes", label: "Changes" });
  return tabs;
}

interface SdkVersionDeletionOptions {
  sdk: Sdk | null;
  currentId?: string;
  versions: AppVersionHistoryItem[];
  setVersions: (versions: AppVersionHistoryItem[]) => void;
  navigate: (path: string) => void;
  toast: ReturnType<typeof useToast>;
}

/** Owns exact SDK/REST version deletion so the detail controller stays below the view-complexity bound. */
function useSdkVersionDeletion({ sdk, currentId, versions, setVersions, navigate, toast }: SdkVersionDeletionOptions) {
  const [deletingVersionId, setDeletingVersionId] = useState("");

  /** Permanently removes one exact app version and keeps the family detail view on a surviving sibling when possible. */
  const deleteVersion = async (version: AppVersionHistoryItem) => {
    // The loaded family supplies both permission scope and delivery-specific user copy.
    if (!sdk) return;
    const kind = sdk.delivery_mode === "api" ? "REST API" : "SDK";
    const confirmed = await toast.confirm(`Delete ${kind} "${sdk.name}" version "${version.version}"? This permanently removes its runtime${sdk.delivery_mode === "api" ? "" : " and package"}.`);
    // Cancellation must leave the immutable version and current route unchanged.
    if (!confirmed) return;
    setDeletingVersionId(version.id);
    try {
      await api.sdks.deactivate(version.id);
      const remaining = versions.filter((candidate) => candidate.id !== version.id);
      toast.success(`${kind} version "${version.version}" deleted.`);
      // Removing the open version requires a new exact route because its detail resource no longer exists.
      if (version.id === currentId) {
        // A surviving sibling keeps the operator in Changes; an empty family returns to its owning catalogue tab.
        if (remaining.length > 0) navigate(`/integrations/sdks/${remaining[0].id}?tab=changes`);
        else navigate(sdkCataloguePath());
        return;
      }
      setVersions(remaining);
    } catch (cause) {
      toast.error(`Failed to delete ${kind} version: ${cause instanceof Error ? cause.message : "Unknown error"}`);
    } finally {
      setDeletingVersionId("");
    }
  };

  return { deletingVersionId, deleteVersion };
}

/** Returns the unified Apps catalogue for every SDK-kind delivery mode. */
function sdkCataloguePath(): string {
  // Delivery affects detail actions, but no longer fragments family discovery.
  return "/integrations/sdks";
}

/** Resolves the exact loaded version while route parameters settle during navigation. */
function sdkCurrentVersionId(routeId: string | undefined, sdk: Sdk): string {
  // The immutable app identity is the authoritative fallback for a transiently absent route value.
  return routeId ?? sdk.app_id;
}

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m) => m.id === "root").flatMap((m) => m.meta ?? []);
  return [
    ...parentMeta.filter((m) => !('title' in m)),
    { title: "App details - Fused" },
  ];
};

function LanguageBadge({ targetLanguage }: { targetLanguage?: string }) {
  if (targetLanguage === "python") {
    return (
      <span className="inline-flex items-center justify-center w-5 h-5" title="Python" aria-label="Python">
        <svg viewBox="0 0 32 32" className="w-4 h-4 shrink-0" aria-hidden="true">
          <path fill="#3776AB" d="M15.9 3c-6.3 0-5.9 2.7-5.9 2.7v2.8h6v.9H7.6S3 8.9 3 15.9 7 22.6 7 22.6h2.4v-3.4s-.1-4 4-4h6.7s3.7.1 3.7-3.5V5.9S24.4 3 18 3h-2.1Zm-3.3 2a1.2 1.2 0 1 1 0 2.3 1.2 1.2 0 0 1 0-2.3Z"/>
          <path fill="#FFD43B" d="M16.1 29c6.3 0 5.9-2.7 5.9-2.7v-2.8h-6v-.9h8.4s4.6.5 4.6-6.5-4-6.7-4-6.7h-2.4v3.4s.1 4-4 4H12s-3.7-.1-3.7 3.5v5.8S7.6 29 14 29h2.1Zm3.3-2a1.2 1.2 0 1 1 0-2.3 1.2 1.2 0 0 1 0 2.3Z"/>
        </svg>
      </span>
    );
  }

  return (
    <span className="inline-flex items-center justify-center w-5 h-5" title="TypeScript" aria-label="TypeScript">
      <svg viewBox="0 0 32 32" className="w-4 h-4 shrink-0" aria-hidden="true">
        <rect x="3" y="3" width="26" height="26" rx="3" fill="#3178C6" />
        <path fill="#fff" d="M11.3 13.1h9.8v2.2h-3.7V26h-2.4V15.3h-3.7v-2.2Zm10.6 0h-2.4v8.5c0 2.5 1.3 4.4 4.7 4.4 1.2 0 2.5-.3 3.5-.8v-2.1c-.9.5-1.8.8-2.7.8-1.7 0-3.1-.8-3.1-2.6v-8.2Z"/>
      </svg>
    </span>
  );
}

function isTableCellEmpty(cell: ReactNode): boolean {
  const c = (cell as { props?: { children?: ReactNode } } | null)?.props?.children;
  if (!c) return true;
  if (typeof c === 'string') return c.trim() === '';
  if (Array.isArray(c)) return c.every((ch: ReactNode) => !ch || (typeof ch === 'string' && ch.trim() === ''));
  return false;
}

function splitNoteContent(content: ReactNode): { prose: ReactNode[]; blocks: string[] } {
  const items: ReactNode[] = Array.isArray(content) ? content : (content ? [content] : []);
  const prose: ReactNode[] = [];
  const blocks: string[] = [];
  for (const item of items) {
    if (isValidElement(item) && item.type === 'code') {
      const codeText = (item.props as { children?: string }).children ?? '';
      if (codeText.length > 30) { blocks.push(codeText); continue; }
    }
    prose.push(item);
  }
  return { prose, blocks };
}

function CodeBlock({ code, language }: { code: string; language: string }) {
  const [copied, setCopied] = useState(false);

  const handleCopy = () => {
    navigator.clipboard.writeText(code);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  return (
    <div className="relative group my-5 rounded-lg border border-slate-200 overflow-hidden bg-white">
      <button
        onClick={handleCopy}
        aria-label="Copy code"
        className="absolute top-2 right-2 z-10 px-2 py-1 rounded border border-slate-200 bg-white text-slate-500 hover:text-slate-700 hover:bg-slate-50 opacity-0 group-hover:opacity-100 transition-all text-xs flex items-center gap-1.5 font-medium shadow-sm"
      >
        {copied ? <Check className="w-3 h-3" /> : <Copy className="w-3 h-3" />}
        {copied ? "Copied!" : "Copy"}
      </button>
      {/* Plain rendering avoids Prism's async language fan-out. In the embedded
          Engine UI, hundreds of tiny language chunks make browser navigation
          look like slow Go embed responses even when the server is sub-ms. */}
      <pre className="m-0 overflow-x-auto bg-transparent p-4 text-sm leading-relaxed text-slate-800">
        <code className={`language-${language}`}>{code}</code>
      </pre>
    </div>
  );
}

/** Renders an app version with permission-aware bucket and activity sections. */
export default function SdkDetails() {
  const { access } = useCurrentActorAccess();
  const { id } = useParams();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const toast = useToast();

  const [sdk, setSdk] = useState<Sdk | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const requestedActiveTab = sdkPrimaryTab(searchParams.get("tab"));
  const requestedActivitySection = sdkActivitySection(searchParams.get("activity"));

  const [versions, setVersions] = useState<AppVersionHistoryItem[]>([]);
  const [bucket, setBucket] = useState<Bucket | null>(null);
  const versionDeletion = useSdkVersionDeletion({ sdk, currentId: id, versions, setVersions, navigate, toast });

  // Contextual notification banner: filtered to just this SDK/MCP config.
  // config_key follows the Engine's own "sdk:<name>:<version>" /
  // "mcp:<name>:<version>" convention (see validateSDKConfigKey /
  // mcp_config_handlers.go), used as a fast path; falls back to matching any
  // of this config's bundled services + versions -- see matchesConfig and
  // plans/plan-service-changelog.md's Phase 4 "Correction from an earlier
  // draft of this doc" note.
  const {
    unresolved: allNotifications,
    serviceRefs: notificationServiceRefs,
    markRead: markNotificationRead,
    dismiss: dismissNotification,
    canUpdate: canUpdateNotifications,
  } = useWorkspaceNotifications();
  // Keep the detail banner focused on outstanding work; acknowledged
  // notifications remain visible from the bell and full notifications page.
  const sdkNotifications = pendingSdkNotifications(sdk, allNotifications);

  /** Loads one immutable app version and its selected services. */
  const fetchSdk = (appId: string) => {
    setLoading(true);
    const queryStr = `
      query($appId: String!) {
        app(app_id: $appId) {
          app_id
          app_family_id
          name
          description
          version
          kind
          delivery_mode
          target_language
          created_at
          readme
          status
          downloads
          selections { service_id service_version_id schema_version endpoint_ids operation_names webhook_ids webhook_names select_all webhook_select_all }
        }
        appServices(app_id: $appId) { service_id service_slug service_name version select_all endpoint_count webhook_count }
      }
    `;
    api.mcpGraphql<{ app: Sdk & { selections: AppSelectionPayload[] }; appServices: AppServiceSummary[] }>(queryStr, { appId })
      .then(res => {
        // Shared catalogue reads still validate the adapter boundary so an MCP
        // identifier cannot acquire SDK download or documentation controls.
        if (res.app.kind !== "sdk") throw new Error("App not found");
        const detailedSelections = appConnectedServiceSelections(res.app.selections, res.appServices);
        // Direct REST shares SDK lifecycle storage but must not expose package-only controls.
        const isDirectREST = res.app.delivery_mode === "api";
        const local = { ...res.app, detailed_selections: detailedSelections, target_type: isDirectREST ? "api" : res.app.kind, is_downloadable: !isDirectREST };
        setSdk(local);
        fetchVersions(local.app_family_id);
      })
      .catch(e => setError(e.message))
      .finally(() => setLoading(false));
  };

  // Related bucket reads require both bucket visibility and this app family.
  useEffect(() => {
    if (!sdk) return;
    if (!hasAnyPermission(access, "bucket.read")) return;
    if (!hasResourcePermission(access, "app.read", "APP", sdk.app_family_id)) return;
    readBucketsForSDK(sdk.app_family_id).then(state => {
      if (state.sdkBuckets.length > 0) setBucket(state.sdkBuckets[0]);
      else setBucket(state.buckets.find(candidate => candidate.is_default) ?? null);
    }).catch(() => {});
  }, [access, sdk?.app_family_id]);

  const canReadActivity = canReadSdkActivity(access, sdk);
  const canManageVersions = canManageAppVersions(access, sdk);
  const { activeTab, activitySection } = accessibleSdkDetailState(requestedActiveTab, requestedActivitySection, canReadActivity);

  /** Loads the readable immutable versions in one app family. */
  const fetchVersions = (appFamilyId: string) => {
    const queryStr = `
      query($appFamilyId: String!) {
        appVersions(app_family_id: $appFamilyId) {
            app_id
            version
            created_at
        }
      }
    `;
    api.mcpGraphql<{ appVersions: Array<{ app_id: string; version: string; created_at: string }> }>(queryStr, { appFamilyId })
      .then(res => {
        setVersions(res.appVersions.map(version => ({ ...version, id: version.app_id })));
      })
      .catch(() => {}); // non-fatal
  };

  useEffect(() => {
    if (!id) return;
    fetchSdk(id);
  }, [id]);

  useEffect(() => {
    if (sdk?.name) document.title = `${sdk.name} - Fused`;
  }, [sdk?.name]);

  useEffect(() => {
    if (!loading && activeTab === "docs" && !sdk?.readme) {
      const next = new URLSearchParams(searchParams);
      next.delete("tab");
      setSearchParams(next, { replace: true });
    }
  }, [loading, activeTab, sdk?.readme, searchParams, setSearchParams]);

  /** Stores only non-default primary app tabs in the exact-version URL. */
  const setActiveTab = (tab: SdkPrimaryTab) => {
    const next = new URLSearchParams(searchParams);
    // Overview is the canonical detail URL and therefore owns no explicit tab parameter.
    if (tab === "overview") {
      next.delete("tab");
    } else {
      next.set("tab", tab);
    }
    setSearchParams(next, { replace: true });
  };

  /** Stores the adapter-specific Activity subsection without coupling it to lifecycle Changes. */
  const setActivitySection = (section: SdkActivitySection) => {
    const next = new URLSearchParams(searchParams);
    next.set("tab", "activity");
    // Activity Overview is canonical beneath the primary Activity tab.
    if (section === "overview") next.delete("activity");
    else next.set("activity", section);
    setSearchParams(next, { replace: true });
  };

  const handleVersionSwitch = (newId: string) => {
    // Selecting the already-open immutable version must not add redundant navigation history.
    if (newId === id) return;
    navigate(`/integrations/sdks/${newId}`);
  };

  const handleDownload = async () => {
    if (!sdk) return;
    try {
      await api.sdks.download(sdk.app_id, sdk.name, sdk.version);
      await fetchSdk(sdk.app_id);
    } catch {
      toast.error("Failed to download app package");
    }
  };

  if (loading) return (
    <div className="flex flex-col items-center justify-center py-20 text-slate-400">
      <div className="w-8 h-8 border-4 border-blue-500 border-t-transparent rounded-full animate-spin mb-4" />
      <p className="animate-pulse font-medium text-slate-500">Loading app details...</p>
    </div>
  );

  if (error || !sdk) return (
    <div className="p-6">
      <AppDetailBackLink to={sdkCataloguePath()} className="mb-6" />
      <div className="bg-red-50 border border-red-200 text-red-600 p-4 rounded-lg">
        {error || "App not found"}
      </div>
    </div>
  );

  return <SdkLoadedContent
    sdk={sdk}
    bucket={bucket}
    notifications={sdkNotifications}
    notificationServiceRefs={notificationServiceRefs}
    markNotificationRead={markNotificationRead}
    dismissNotification={dismissNotification}
    canUpdateNotifications={canUpdateNotifications}
    versions={versions}
    currentId={id}
    activeTab={activeTab}
    activitySection={activitySection}
    canReadActivity={canReadActivity}
    canManageVersions={canManageVersions}
    deletingVersionId={versionDeletion.deletingVersionId}
    onDownload={handleDownload}
    onVersionSwitch={handleVersionSwitch}
    onDeleteVersion={versionDeletion.deleteVersion}
    onTabChange={setActiveTab}
    onActivityChange={setActivitySection}
    onCopySandbox={(url) => { navigator.clipboard.writeText(url); toast.success("Sandbox URL copied!"); }}
  />;
}

type SdkLoadedContentProps = {
  sdk: Sdk;
  bucket: Bucket | null;
  notifications: WorkspaceNotification[];
  notificationServiceRefs: Record<string, NotificationServiceRef>;
  markNotificationRead: (id: string) => void;
  dismissNotification: (id: string) => void;
  canUpdateNotifications: boolean;
  versions: AppVersionHistoryItem[];
  currentId?: string;
  activeTab: SdkPrimaryTab;
  activitySection: SdkActivitySection;
  canReadActivity: boolean;
  canManageVersions: boolean;
  deletingVersionId: string;
  onDownload: () => void;
  onVersionSwitch: (id: string) => void;
  onDeleteVersion: (version: AppVersionHistoryItem) => void;
  onTabChange: (tab: SdkPrimaryTab) => void;
  onActivityChange: (section: SdkActivitySection) => void;
  onCopySandbox: (url: string) => void;
};

/** Renders the loaded app version after query and permission state settle. */
function SdkLoadedContent({
  sdk,
  bucket,
  notifications: sdkNotifications,
  notificationServiceRefs,
  markNotificationRead,
  dismissNotification,
  canUpdateNotifications,
  versions,
  currentId: id,
  activeTab,
  activitySection,
  canReadActivity,
  canManageVersions,
  deletingVersionId,
  onDownload: handleDownload,
  onVersionSwitch: handleVersionSwitch,
  onDeleteVersion: handleDeleteVersion,
  onTabChange: setActiveTab,
  onActivityChange: setActivitySection,
  onCopySandbox,
}: SdkLoadedContentProps) {
  const currentVersionId = sdkCurrentVersionId(id, sdk);

  return (
    <div className="space-y-6">
      <AppDetailBackLink to={sdkCataloguePath()} />

      {/* Adapter-owned metadata and actions slot into one shared app identity hierarchy. */}
      <AppDetailHeader
        name={sdk.name}
        summary="A reusable interface for the services and operations this app can use."
        status={sdk.status}
        version={sdk.version}
        createdAt={sdk.created_at}
        leadingMetadata={optionalNode(sdk.target_type === "sdk", <LanguageBadge targetLanguage={sdk.target_language} />)}
        trailingMetadata={optionalNode(Boolean(bucket), (
          <span className="flex items-center gap-1.5 rounded border border-slate-200 bg-slate-50 px-2 py-0.5 text-slate-600">
            <Database className="h-3.5 w-3.5 text-slate-400" />
            <Link to={`/integrations/buckets?bucket=${encodeURIComponent(bucket?.id ?? "")}`} className="transition-colors hover:text-blue-600">
              {bucket?.name}
            </Link>
          </span>
        ))}
        action={optionalNode(Boolean(sdk.is_downloadable), <AppDetailPrimaryAction icon={<Download className="h-4 w-4" />} label="Download package" onClick={handleDownload} />)}
      />

      {/* The immutable app key resets disclosure state when switching versions or families. */}
      {optionalNode(sdkNotifications.length > 0, (
        <NotificationBanner
          key={sdk.app_id}
          items={sdkNotifications}
          serviceRefs={notificationServiceRefs}
          onMarkRead={markNotificationRead}
          onDismiss={dismissNotification}
          canUpdate={canUpdateNotifications}
        />
      ))}

      <AppVersionSwitcher label="App version" versions={versions} currentId={currentVersionId} onSelect={handleVersionSwitch} />

      <AppDetailTabs label="App details" active={activeTab} tabs={sdkPrimaryTabs(sdk, canReadActivity)} onChange={setActiveTab} />

      <AppDetailBody>
        {optionalNode(activeTab === "overview", (
          <AppOverviewBody
            selections={sdk.detailed_selections ?? []}
            adapterDetails={optionalNode(hasSandboxURL(sdk), (
              <AppDetailSection title="Hosted Sandbox URL">
                <div className="rounded-xl border border-slate-200 bg-slate-50 p-6 shadow-sm">
                  <p className="text-sm text-slate-700 mb-4 max-w-2xl">
                    Use this URL in your MCP client (Cursor, Claude Desktop) to connect to this server instantly without running it locally.
                  </p>
                  <div className="flex items-center gap-3">
                    <code className="flex-1 px-4 py-3 rounded-lg border border-slate-200 bg-white text-slate-800 font-mono text-sm break-all">
                      {sdk.sandbox_url}
                    </code>
                    <button
                      onClick={() => onCopySandbox(sdk.sandbox_url ?? "")}
                      className="p-3 bg-white border border-slate-200 text-slate-600 rounded-lg hover:bg-slate-50 transition-colors shadow-sm cursor-pointer"
                    >
                      <Copy className="w-5 h-5" />
                    </button>
                  </div>
                </div>
              </AppDetailSection>
            ))}
          />
        ))}

        {optionalNode(showsSdkDocs(activeTab, sdk), (
          <div className="bg-white rounded-xl border border-slate-200 p-8 shadow-sm prose prose-slate max-w-none prose-headings:font-semibold prose-headings:text-slate-900 prose-p:text-slate-600 prose-p:leading-relaxed prose-li:text-slate-600 prose-strong:text-slate-800 prose-strong:font-semibold [&_h1_code]:bg-transparent [&_h1_code]:border-0 [&_h1_code]:px-0 [&_h1_code]:py-0 [&_h2_code]:bg-transparent [&_h2_code]:border-0 [&_h2_code]:px-0 [&_h2_code]:py-0 [&_h3_code]:bg-transparent [&_h3_code]:border-0 [&_h3_code]:px-0 [&_h3_code]:py-0 [&_h4_code]:bg-transparent [&_h4_code]:border-0 [&_h4_code]:px-0 [&_h4_code]:py-0 [&_h5_code]:bg-transparent [&_h5_code]:border-0 [&_h5_code]:px-0 [&_h5_code]:py-0">
            <ReactMarkdown
              remarkPlugins={[remarkGfm]}
              components={{
                pre(props) {
                  return <>{props.children}</>;
                },
                h3(props) {
                  return <h3 className="mt-10 mb-3 text-base font-semibold text-slate-700 uppercase tracking-wide border-b border-slate-100 pb-2" {...props} />;
                },
                h4(props) {
                  return (
                    <div className="mt-6 mb-3">
                      <h4 className="inline-flex items-center gap-2 font-mono text-sm font-semibold text-blue-700 bg-blue-50 border border-blue-100 rounded-lg px-3 py-1.5 m-0" {...props} />
                    </div>
                  );
                },
                h5(props) {
                  return <h5 className="mt-5 mb-2 text-xs font-semibold text-slate-500 uppercase tracking-widest" {...props} />;
                },
                table(props) {
                  return (
                    <div className="my-6 overflow-x-auto rounded-lg border border-slate-200">
                      <table className="min-w-full border-collapse text-sm" {...props} />
                    </div>
                  );
                },
                thead(props) {
                  return <thead className="bg-slate-50" {...props} />;
                },
                th(props) {
                  return <th className="border-b border-slate-200 px-4 py-2 text-left font-semibold text-slate-700" {...props} />;
                },
                td(props) {
                  return <td className="border-t border-slate-200 px-4 py-2 align-top text-slate-600" {...props} />;
                },
                tr(props) {
                  const { children } = props;
                  const cells: ReactNode[] = Array.isArray(children) ? children : (children ? [children] : []);
                  const emptyCells = cells.filter(isTableCellEmpty);
                  if (cells.length > 1 && emptyCells.length === cells.length - 1) {
                    const contentCell = cells.find((c: ReactNode) => !isTableCellEmpty(c)) as { props?: { children?: ReactNode } } | null;
                    const { prose, blocks } = splitNoteContent(contentCell?.props?.children);
                    return (
                      <tr>
                        <td colSpan={99} className="border-t border-slate-200 px-4 py-3 text-sm text-slate-500 leading-relaxed bg-slate-50/70">
                          <span>{prose}</span>
                          {blocks.map((code, i) => (
                            <code key={i} className="mt-2 block font-mono text-xs bg-white border border-slate-200 rounded-md px-3 py-2 text-slate-700 whitespace-pre-wrap break-all">
                              {code}
                            </code>
                          ))}
                        </td>
                      </tr>
                    );
                  }
                  return <tr {...props} />;
                },
                hr(props) {
                  return <hr className="my-10 border-slate-200" {...props} />;
                },
                code(props) {
                  // eslint-disable-next-line @typescript-eslint/no-unused-vars -- destructured out of rest so it isn't spread onto the DOM element
                  const {children, className, node, ...rest} = props;
                  const match = /language-(\w+)/.exec(className || '');
                  return match ? (
                    <CodeBlock code={String(children).replace(/\n$/, '')} language={match[1]} />
                  ) : (
                    <code {...rest} className="bg-slate-100 text-slate-800 rounded px-1.5 py-0.5 text-[0.875em] font-mono font-medium border border-slate-200">
                      {children}
                    </code>
                  );
                }
              }}
            >
              {sdk.readme}
            </ReactMarkdown>
          </div>
        ))}

        {optionalNode(activeTab === "activity" && canReadActivity, (
          <AppActivityBody
            active={activitySection}
            ariaLabel="App activity"
            onChange={setActivitySection}
            options={sdkActivityOptions(canReadActivity)}
          >
            {optionalNode(canReadActivity && activitySection === "overview", (
              <AppActivityOverview
                appId={sdk.app_id}
                downloads={sdk.downloads ?? null}
                pendingDriftCount={0}
                services={sdk.detailed_selections ?? []}
              />
            ))}

            {optionalNode(canReadActivity && activitySection === "requests", (
              <AppRequestsPanel appId={sdk.app_id} consumerName={sdk.name} transport={sdkTransport(sdk)} />
            ))}
          </AppActivityBody>
        ))}

        {optionalNode(activeTab === "changes", (
          <AppChangesBody
            versions={versions}
            currentId={currentVersionId}
            canDelete={canManageVersions}
            deletingVersionId={deletingVersionId}
            onSelect={handleVersionSwitch}
            onDelete={handleDeleteVersion}
          />
        ))}
      </AppDetailBody>
    </div>
  );
}
