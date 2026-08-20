import { useState, useEffect, isValidElement, type ReactNode } from "react";
import { useParams, Link, useNavigate, useSearchParams, type MetaFunction } from "@remix-run/react";
import { ArrowLeft, Download, ChevronDown, ChevronRight, Copy, Check, Loader2, Database } from "lucide-react";
import { api, type NotificationServiceRef, type WorkspaceNotification } from "~/lib/api";
import { useToast } from "~/components/Toast";
import { readBucketsForSDK } from "~/lib/buckets";
import { serviceDetailPath } from "~/lib/service-navigation";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { EndpointRow, WebhookRow } from "~/components/EndpointRow";
import { NotificationBanner } from "~/components/notifications/NotificationBanner";
import { useWorkspaceNotifications } from "~/components/notifications/useWorkspaceNotifications";
import { isPending, matchesConfig } from "~/components/notifications/notificationHelpers";
import { AppRequestsPanel } from "~/components/activity/AppRequestsPanel";
import { AppActivityOverview } from "~/components/activity/AppActivityOverview";
import { NestedActivityTabs } from "~/components/activity/NestedActivityTabs";
import { AppRuntimeStatus } from "~/components/apps/AppRuntimeStatus";
import { type Bucket } from "~/lib/api";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasAnyPermission, hasResourcePermission, hasWorkspacePermission } from "~/lib/current-actor-access";
import type { CurrentActorAccess } from "~/lib/current-actor-access";
import {
  requireAppSelectionsV3,
  type AppSelectionPayload,
  type AppSelectionV3,
} from "~/lib/app-selection-v3";

type SdkSelection = AppSelectionV3 & {
  service_name?: string;
  service_slug?: string;
  service_provider?: string;
  service_version_name?: string;
};

type SdkEndpointRow = {
  id: string;
  method?: string;
  path?: string;
  name?: string;
  description?: string;
  resource_name?: string;
};

type SdkWebhookRow = {
  id: string;
  method?: string;
  name?: string;
};

type SdkPrimaryTab = "overview" | "docs" | "analytics";
type SdkActivitySection = "overview" | "requests" | "changes";

/** Resolves the primary detail tab from a URL value. */
function sdkPrimaryTab(value: string | null): SdkPrimaryTab {
  if (value === "docs" || value === "analytics") return value;
  return "overview";
}

/** Resolves the nested activity section from a URL value. */
function sdkActivitySection(value: string | null): SdkActivitySection {
  if (value === "requests" || value === "changes") return value;
  return "overview";
}

/** Returns a node only when its presentation condition is satisfied. */
function optionalNode(show: boolean, node: ReactNode): ReactNode {
  return show ? node : null;
}

/** Selects the active or inactive primary-tab class. */
function sdkTabClass(active: boolean): string {
  const tone = active ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700";
  return `px-4 py-1.5 text-sm font-medium rounded-md transition-all cursor-pointer shrink-0 ${tone}`;
}

/** Selects the active or inactive version-button class. */
function sdkVersionClass(active: boolean): string {
  const tone = active ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700";
  return `px-3 py-1 text-xs font-medium rounded-md transition-all cursor-pointer ${tone}`;
}

/** Formats an optional creation date without branching inside the main view. */
function sdkCreatedDate(value?: string): string {
  return value ? new Date(value).toLocaleDateString() : "";
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

type Sdk = {
  app_id: string;
  app_family_id: string;
  name: string;
  description?: string;
  version: string;
  target_type: string;
  target_language?: string;
  sandbox_url?: string;
  is_downloadable?: boolean;
  created_at?: string;
  downloads?: number;
  readme?: string;
  detailed_selections?: SdkSelection[];
  status: string;
};

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m) => m.id === "root").flatMap((m) => m.meta ?? []);
  return [
    ...parentMeta.filter((m) => !('title' in m)),
    { title: "App details - Fused" },
  ];
};

const capitalizeFirstLetter = (str: string) => {
  if (!str) return "";
  return str.charAt(0).toUpperCase() + str.slice(1);
};

/** Builds the canonical service slug used by detail links. */
function bundledServiceSlug(selection: SdkSelection): string | undefined {
  if (!selection.service_provider) return selection.service_slug;
  if (!selection.service_slug) return undefined;
  return `@${selection.service_provider}/${selection.service_slug}`;
}

/** Summarizes the immutable resources selected for one bundled service. */
function bundledSelectionSummary(selection: SdkSelection): string {
  if (selection.select_all) return "All operations";
  const labels = [resourceCountLabel(selection.endpoint_ids?.length, "operation"), resourceCountLabel(selection.webhook_ids?.length, "webhook")].filter(Boolean);
  return labels.join(" · ") || "0 resources";
}

/** Formats a nonzero resource count for the bundled-service summary. */
function resourceCountLabel(count: number | undefined, label: string): string {
  if (!count) return "";
  return `${count} ${label}${count === 1 ? "" : "s"}`;
}

/** Renders the open or closed service disclosure indicator. */
function ServiceDisclosureIcon({ open }: { open: boolean }) {
  if (open) return <ChevronDown className="w-3.5 h-3.5 shrink-0" />;
  return <ChevronRight className="w-3.5 h-3.5 shrink-0" />;
}

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

/** Read-only bundled services display — mirrors EndpointSelectionList visual style */
function BundledServicesSection({ selections }: { selections: SdkSelection[] }) {
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const [expandedResourceGroups, setExpandedResourceGroups] = useState<Record<string, boolean>>({});
  const [resourceGroupsData, setResourceGroupsData] = useState<Record<string, string[]>>({});
  const [endpointsData, setEndpointsData] = useState<Record<string, SdkEndpointRow[]>>({});
  const [webhooksData, setWebhooksData] = useState<Record<string, SdkWebhookRow[]>>({});
  const [loadingServices, setLoadingServices] = useState<Record<string, boolean>>({});
  const [loadingEndpoints, setLoadingEndpoints] = useState<Record<string, boolean>>({});

  const fetchServiceMetadata = (serviceId: string) => {
    setLoadingServices(prev => ({ ...prev, [serviceId]: true }));
    const selection = selections.find(item => item.service_id === serviceId);
    // Exact selections are persisted with the app version. Present those IDs
    // directly instead of consulting Registry state that may have changed.
    setResourceGroupsData(prev => ({ ...prev, [serviceId]: selection?.endpoint_ids?.length ? ["Selected operations"] : [] }));
    setWebhooksData(prev => ({ ...prev, [serviceId]: (selection?.webhook_ids ?? []).map(id => ({ id })) }));
    setLoadingServices(prev => ({ ...prev, [serviceId]: false }));
  };

  const fetchResourceEndpoints = (serviceId: string, _resourceName: string, groupKey: string) => {
    setLoadingEndpoints(prev => ({ ...prev, [groupKey]: true }));
    const selection = selections.find(item => item.service_id === serviceId);
    setEndpointsData(prev => ({ ...prev, [groupKey]: (selection?.endpoint_ids ?? []).map(id => ({ id, name: id })) }));
    setLoadingEndpoints(prev => ({ ...prev, [groupKey]: false }));
  };

  const toggle = (serviceId: string) => {
    setExpanded(prev => {
      const isNowOpen = !prev[serviceId];
      if (isNowOpen && !resourceGroupsData[serviceId] && !loadingServices[serviceId]) {
        fetchServiceMetadata(serviceId);
      }
      return { ...prev, [serviceId]: isNowOpen };
    });
  };

  const toggleResourceGroup = (serviceId: string, resourceName: string) => {
    const groupKey = `${serviceId}-${resourceName}`;
    setExpandedResourceGroups(prev => {
      const isNowOpen = !prev[groupKey];
      if (isNowOpen && !endpointsData[groupKey] && !loadingEndpoints[groupKey]) {
        fetchResourceEndpoints(serviceId, resourceName, groupKey);
      }
      return { ...prev, [groupKey]: isNowOpen };
    });
  };

  if (!selections || selections.length === 0) {
    return <p className="text-sm text-slate-400 py-2">No services connected.</p>;
  }

  return (
    <div className="rounded-lg border border-slate-200 overflow-hidden divide-y divide-slate-100">
      {/* Projects each immutable selection without re-querying Registry state. */}
      {selections.map((sel, idx: number) => {
        const key = sel.service_id ?? String(idx);
        const canonicalSlug = bundledServiceSlug(sel);
        const isOpen = !!expanded[key];
        const webhooks = webhooksData[key] || [];
        const isLoading = loadingServices[key];

        const resources = resourceGroupsData[key] || [];
        const sortedResourceNames = [...resources].sort((a, b) => a.localeCompare(b));

        return (
          <div key={key}>
            {/* Service header */}
            <div
              className="w-full flex flex-col sm:flex-row sm:items-center sm:justify-between gap-2 px-4 py-3 bg-slate-50 hover:bg-slate-100 transition-colors text-left select-none cursor-pointer"
            >
              <div className="flex min-w-0 w-full sm:w-auto items-start gap-2">
                <button
                  type="button"
                  onClick={() => toggle(key)}
                  className="rounded p-0.5 text-slate-400 hover:bg-slate-200 hover:text-slate-700"
                  aria-label={`Toggle ${sel.service_name}`}
                >
                  <ServiceDisclosureIcon open={isOpen} />
                </button>
                <div className="flex min-w-0 flex-1 flex-col sm:flex-row sm:items-center gap-1 sm:gap-2">
                  <a
                    href={serviceDetailPath(sel.service_id, canonicalSlug)}
                    className="text-sm font-semibold text-slate-700 hover:text-blue-600 transition-colors"
                  >
                    {capitalizeFirstLetter(sel.service_name || "")}
                  </a>
                  {optionalNode(Boolean(sel.service_version_name), (
                    <span className="max-w-full break-all text-xs text-slate-500 bg-white border border-slate-200 rounded px-1.5 py-0.5">
                      {sel.service_version_name}
                    </span>
                  ))}
                </div>
              </div>
              <button
                type="button"
                onClick={() => toggle(key)}
                className="self-end rounded px-1 py-0.5 text-xs text-slate-400 hover:bg-slate-200 hover:text-slate-700 sm:self-auto shrink-0"
              >
                {bundledSelectionSummary(sel)}
              </button>
            </div>

            {/* Expanded rows */}
            {isOpen && (
              <div className="bg-white">
                {isLoading ? (
                  <div className="px-5 py-4 text-sm text-slate-400 flex items-center justify-center">
                    <div className="w-4 h-4 border-2 border-blue-500 border-t-transparent rounded-full animate-spin mr-2" />
                    Loading resources...
                  </div>
                ) : (resources.length === 0 && webhooks.length === 0) ? (
                  <div className="px-5 py-4 text-sm text-slate-400 italic text-center">No operations or events.</div>
                ) : (
                  <div className="pb-2">
                    {/* Render Grouped Endpoints */}
                    {sortedResourceNames.map(resName => {
                      const groupKey = `${key}-${resName}`;
                      const isGroupCollapsed = !expandedResourceGroups[groupKey];
                      const eps = endpointsData[groupKey] || [];
                      const isEpsLoading = loadingEndpoints[groupKey];
                      return (
                        <div key={groupKey}>
                          <div
                            className="bg-slate-50 px-5 py-2.5 border-y border-slate-100 flex justify-between items-center cursor-pointer hover:bg-slate-100 transition-colors select-none"
                            onClick={() => toggleResourceGroup(key, resName)}
                          >
                            <div className="flex items-center gap-2 flex-wrap">
                              {isGroupCollapsed ? <ChevronRight className="w-4 h-4 text-slate-400" /> : <ChevronDown className="w-4 h-4 text-slate-400" />}
                              <h3 className="text-xs font-semibold text-slate-600 uppercase tracking-wider">{resName}</h3>
                              {isEpsLoading && <Loader2 className="w-3.5 h-3.5 animate-spin text-slate-400 ml-2" />}
                            </div>
                          </div>
                          {!isGroupCollapsed && !isEpsLoading && (
                            <div className="divide-y divide-slate-50">
                              {eps.map((ep) => (
                                <EndpointRow key={ep.id} ep={ep} />
                              ))}
                              {eps.length === 0 && <div className="px-5 py-4 text-xs text-slate-400">No operations found.</div>}
                            </div>
                          )}
                        </div>
                      );
                    })}
                    
                    {/* Render Webhooks Group if any */}
                    {webhooks.length > 0 && (() => {
                      const groupKey = `${key}-webhooks`;
                      const isGroupCollapsed = !expandedResourceGroups[groupKey];
                      return (
                        <div>
                          <div
                            className="bg-slate-50 px-5 py-2.5 border-y border-slate-100 flex justify-between items-center cursor-pointer hover:bg-slate-100 transition-colors select-none"
                            onClick={() => setExpandedResourceGroups(prev => ({ ...prev, [groupKey]: !prev[groupKey] }))}
                          >
                            <div className="flex items-center gap-2 flex-wrap">
                              {isGroupCollapsed ? <ChevronRight className="w-4 h-4 text-slate-400" /> : <ChevronDown className="w-4 h-4 text-slate-400" />}
                              <h3 className="text-xs font-semibold text-slate-600 uppercase tracking-wider">Webhooks</h3>
                              <span className="text-xs text-slate-400 ml-2">({webhooks.length})</span>
                            </div>
                          </div>
                          {!isGroupCollapsed && (
                            <div className="divide-y divide-slate-50">
                              {webhooks.map((wh) => (
                                <WebhookRow key={wh.id} wh={wh} />
                              ))}
                            </div>
                          )}
                        </div>
                      );
                    })()}
                  </div>
                )}
              </div>
            )}
          </div>
        );
      })}
    </div>
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
  const activeTab = sdkPrimaryTab(searchParams.get("tab"));
  const activitySection = sdkActivitySection(searchParams.get("activity"));

  const [versions, setVersions] = useState<Array<{ id: string; version: string; created_at: string }>>([]);
  const [bucket, setBucket] = useState<Bucket | null>(null);

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
          target_language
          created_at
          readme
          status
          selections { service_id service_version_id definition_schema_version endpoint_ids webhook_ids select_all webhook_select_all }
        }
        appServices(app_id: $appId) { service_id service_slug service_name version select_all endpoint_count webhook_count }
      }
    `;
    type AppService = { service_id: string; service_slug: string; service_name: string; version?: string; select_all: boolean };
    api.mcpGraphql<{ app: Sdk & { selections: AppSelectionPayload[] }; appServices: AppService[] }>(queryStr, { appId })
      .then(res => {
        const serviceById = new Map(res.appServices.map(service => [service.service_id, service]));
        const detailedSelections = requireAppSelectionsV3(res.app.selections).map(selection => {
          const service = serviceById.get(selection.service_id);
          return {
            ...selection,
            service_slug: service?.service_slug,
            service_name: service?.service_name,
            service_version_name: service?.version,
          };
        });
        const local = { ...res.app, detailed_selections: detailedSelections, target_type: "sdk", is_downloadable: true };
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

  const setActiveTab = (tab: "overview" | "docs" | "analytics") => {
    const next = new URLSearchParams(searchParams);
    if (tab === "overview") {
      next.delete("tab");
    } else {
      next.set("tab", tab);
    }
    setSearchParams(next, { replace: true });
  };

  const setActivitySection = (section: "overview" | "requests" | "changes") => {
    const next = new URLSearchParams(searchParams);
    next.set("tab", "analytics");
    if (section === "overview") next.delete("activity");
    else next.set("activity", section);
    setSearchParams(next, { replace: true });
  };

  const handleVersionSwitch = (newId: string) => {
    if (newId === id) return;
    navigate(`/integrations/sdks/${newId}`);
  };

  const handleDownload = async () => {
    if (!sdk) return;
    try {
      await api.sdks.download(sdk.app_id, sdk.name, sdk.version);
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
      <Link to="/integrations/sdks" className="inline-flex items-center text-sm text-slate-500 hover:text-slate-800 mb-6 transition-colors">
        <ArrowLeft className="w-4 h-4 mr-2" />
        Back to apps
      </Link>
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
    onDownload={handleDownload}
    onVersionSwitch={handleVersionSwitch}
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
  versions: Array<{ id: string; version: string; created_at: string }>;
  currentId?: string;
  activeTab: SdkPrimaryTab;
  activitySection: SdkActivitySection;
  canReadActivity: boolean;
  onDownload: () => void;
  onVersionSwitch: (id: string) => void;
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
  onDownload: handleDownload,
  onVersionSwitch: handleVersionSwitch,
  onTabChange: setActiveTab,
  onActivityChange: setActivitySection,
  onCopySandbox,
}: SdkLoadedContentProps) {

  return (
    <div className="space-y-6">
      <Link to="/integrations/sdks" className="inline-flex items-center text-sm text-slate-500 hover:text-slate-800 transition-colors">
        <ArrowLeft className="w-4 h-4 mr-2" />
        Back to apps
      </Link>

      {/* Header */}
      <div className="flex flex-col md:flex-row md:items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold text-slate-900">{sdk.name}</h1>
          <p className="text-slate-500 mt-1">A reusable interface for the services and operations this app can use.</p>
          <AppRuntimeStatus className="mt-1.5" status={sdk.status} />
          <div className="flex flex-wrap items-center gap-3 mt-3 text-sm">
            <span className="px-2.5 py-1 bg-slate-100 text-slate-700 rounded font-medium">{sdk.version}</span>
            {optionalNode(sdk.target_type === "sdk", <LanguageBadge targetLanguage={sdk.target_language} />)}
            <span className="text-slate-600">Created {sdkCreatedDate(sdk.created_at)}</span>
            {optionalNode(Boolean(bucket), (
              <span className="flex items-center gap-1.5 text-slate-600 bg-slate-50 px-2 py-0.5 rounded border border-slate-200">
                <Database className="w-3.5 h-3.5 text-slate-400" />
                <Link to={`/integrations/buckets?bucket=${encodeURIComponent(bucket?.id ?? "")}`} className="hover:text-blue-600 transition-colors">
                  {bucket?.name}
                </Link>
              </span>
            ))}
          </div>
        </div>

        {optionalNode(Boolean(sdk.is_downloadable), (
          <button
            onClick={handleDownload}
            className="inline-flex w-full md:w-auto items-center justify-center gap-2 px-4 py-2 bg-blue-600 hover:bg-blue-700 text-white text-sm font-medium rounded-lg transition-colors shadow-sm cursor-pointer"
          >
            <Download className="w-4 h-4" />
            Download package
          </button>
        ))}
      </div>

      {optionalNode(sdkNotifications.length > 0, (
        <NotificationBanner
          items={sdkNotifications}
          serviceRefs={notificationServiceRefs}
          onMarkRead={markNotificationRead}
          onDismiss={dismissNotification}
          canUpdate={canUpdateNotifications}
        />
      ))}

      {/* Version switcher */}
      {optionalNode(versions.length > 1, (
        <div className="flex items-center gap-2 flex-wrap">
          <span className="text-xs font-medium text-slate-500 uppercase tracking-wider mr-1">App version</span>
          <div className="flex p-1 bg-slate-100/80 rounded-lg gap-0.5 flex-wrap">
            {versions.map(v => (
              <button
                key={v.id}
                onClick={() => handleVersionSwitch(v.id)}
                className={sdkVersionClass(v.id === id)}
              >
                {v.version}
              </button>
            ))}
          </div>
        </div>
      ))}

      {/* Pill Tabs — matches integrations.$id.tsx */}
      <div className="flex overflow-x-auto p-1 bg-slate-100/80 rounded-lg whitespace-nowrap max-w-full [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
        <button
          onClick={() => setActiveTab("overview")}
          className={sdkTabClass(activeTab === "overview")}
        >
          Overview
        </button>
        {optionalNode(Boolean(sdk.readme), (
          <button
            onClick={() => setActiveTab("docs")}
            className={sdkTabClass(activeTab === "docs")}
          >
            Docs
          </button>
        ))}
        <button
          onClick={() => setActiveTab("analytics")}
          className={sdkTabClass(activeTab === "analytics")}
        >
          Activity
        </button>
      </div>

      {/* Tab Content */}
      <div className="p-1 md:p-1">
        {optionalNode(activeTab === "overview", (
          <div className="space-y-2">

            {/* Connected services — plain subsection, no card */}
            <div>
              <h4 className="text-sm font-semibold text-slate-700 uppercase tracking-wider mb-3">
                Connected services
              </h4>
              <BundledServicesSection
                selections={sdk.detailed_selections ?? []}
              />
            </div>

            {/* MCP Sandbox URL */}
            {optionalNode(hasSandboxURL(sdk), (
              <div className="p-6 bg-slate-50 border border-slate-200 rounded-xl shadow-sm">
                <h3 className="text-lg font-semibold text-slate-900 mb-2">Hosted Sandbox URL</h3>
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
            ))}
          </div>
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

        {optionalNode(activeTab === "analytics", (
          <div className="space-y-6">
            <NestedActivityTabs
              active={activitySection}
              ariaLabel="App activity"
              onChange={setActivitySection}
              options={[
                { value: "overview", label: "Overview" },
                { value: "requests", label: "Requests" },
                { value: "changes", label: "Changes" },
              ]}
            />

            {optionalNode(!canReadActivity && activitySection !== "changes", (
              <div className="rounded-lg border border-slate-200 bg-white px-6 py-12 text-center text-sm text-slate-500">
                App activity access is not available for your account.
              </div>
            ))}

            {optionalNode(canReadActivity && activitySection === "overview", (
              <AppActivityOverview
                appId={sdk.app_id}
                downloads={sdk.downloads ?? 0}
                pendingDriftCount={0}
                services={sdk.detailed_selections ?? []}
              />
            ))}

            {optionalNode(canReadActivity && activitySection === "requests", (
              <AppRequestsPanel appId={sdk.app_id} transport={sdkTransport(sdk)} />
            ))}

            {optionalNode(activitySection === "changes", <div className="space-y-6">
              <div className="overflow-hidden rounded-xl border border-slate-200 bg-white">
                <div className="border-b border-slate-100 bg-slate-50 px-5 py-3"><h4 className="text-sm font-semibold text-slate-800">Version history</h4></div>
                <div className="divide-y divide-slate-100">
                  {versions.map((version) => (
                    <div key={version.id} className="flex items-center justify-between px-5 py-3 text-sm">
                      <button type="button" onClick={() => handleVersionSwitch(version.id)} className="font-medium text-slate-800 hover:text-blue-600">{version.version}</button>
                      <span className="text-xs text-slate-400">{new Date(version.created_at).toLocaleDateString()}</span>
                    </div>
                  ))}
                </div>
              </div>
            </div>)}
          </div>
        ))}
      </div>
    </div>
  );
}
