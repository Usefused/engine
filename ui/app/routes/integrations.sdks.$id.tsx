import { useState, useEffect, isValidElement, type ReactNode } from "react";
import { useParams, Link, useNavigate, useSearchParams, type MetaFunction } from "@remix-run/react";
import { ArrowLeft, Download, ChevronDown, ChevronRight, FileCode, Copy, Check, Loader2, AlertTriangle, Database } from "lucide-react";
import { api } from "~/lib/api";
import { useToast } from "~/components/Toast";
import { readBucketsForSDK } from "~/lib/buckets";
import { serviceDetailPath } from "~/lib/service-navigation";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { EndpointRow, WebhookRow } from "~/components/EndpointRow";
import { NotificationBanner } from "~/components/notifications/NotificationBanner";
import { useWorkspaceNotifications } from "~/components/notifications/useWorkspaceNotifications";
import { isPending, matchesConfig } from "~/components/notifications/notificationHelpers";
import { PendingDriftSection, type PendingDriftItem } from "~/components/sdk/PendingDrift";
import { type Bucket } from "~/lib/api";

type SdkSelection = {
  service_id: string;
  service_name?: string;
  service_slug?: string;
  service_provider?: string;
  service_version_id?: string;
  service_version_name?: string;
  select_all?: boolean;
  endpoint_ids?: string[];
  webhook_ids?: string[];
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

type Sdk = {
  id: string;
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
function BundledServicesSection({ artifactId, selections }: { artifactId: string; selections: SdkSelection[] }) {
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const [expandedResourceGroups, setExpandedResourceGroups] = useState<Record<string, boolean>>({});
  const [resourceGroupsData, setResourceGroupsData] = useState<Record<string, string[]>>({});
  const [endpointsData, setEndpointsData] = useState<Record<string, SdkEndpointRow[]>>({});
  const [webhooksData, setWebhooksData] = useState<Record<string, SdkWebhookRow[]>>({});
  const [loadingServices, setLoadingServices] = useState<Record<string, boolean>>({});
  const [loadingEndpoints, setLoadingEndpoints] = useState<Record<string, boolean>>({});
  const toast = useToast();

  const fetchServiceMetadata = (serviceId: string) => {
    setLoadingServices(prev => ({ ...prev, [serviceId]: true }));
    const query = `
      query {
        sdkSelectionResourceGroups(artifactId: "${artifactId}", serviceId: "${serviceId}")
        sdkSelectionWebhooks(artifactId: "${artifactId}", serviceId: "${serviceId}") {
          id
          method
          name
        }
      }
    `;
    api.graphql<{ sdkSelectionResourceGroups: string[], sdkSelectionWebhooks: SdkWebhookRow[] }>(query)
      .then(res => {
        setResourceGroupsData(prev => ({ ...prev, [serviceId]: res.sdkSelectionResourceGroups || [] }));
        setWebhooksData(prev => ({ ...prev, [serviceId]: res.sdkSelectionWebhooks || [] }));
      })
      .catch(() => toast.error("Failed to fetch service metadata"))
      .finally(() => setLoadingServices(prev => ({ ...prev, [serviceId]: false })));
  };

  const fetchResourceEndpoints = (serviceId: string, resourceName: string, groupKey: string) => {
    setLoadingEndpoints(prev => ({ ...prev, [groupKey]: true }));
    const query = `
      query {
        sdkSelectionResources(artifactId: "${artifactId}", serviceId: "${serviceId}", resourceName: "${resourceName}") {
          id
          method
          path
          name
          description
          resource_name
        }
      }
    `;
    api.graphql<{ sdkSelectionResources: SdkEndpointRow[] }>(query)
      .then(res => {
        setEndpointsData(prev => ({ ...prev, [groupKey]: res.sdkSelectionResources || [] }));
      })
      .catch(() => toast.error("Failed to fetch operations"))
      .finally(() => setLoadingEndpoints(prev => ({ ...prev, [groupKey]: false })));
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
      {selections.map((sel, idx: number) => {
        const key = sel.service_id ?? String(idx);
        const canonicalSlug = sel.service_slug && sel.service_provider
          ? `@${sel.service_provider}/${sel.service_slug}`
          : sel.service_slug;
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
                  aria-label={`${isOpen ? "Collapse" : "Expand"} ${sel.service_name}`}
                >
                  {isOpen
                    ? <ChevronDown className="w-3.5 h-3.5 shrink-0" />
                    : <ChevronRight className="w-3.5 h-3.5 shrink-0" />}
                </button>
                <div className="flex min-w-0 flex-1 flex-col sm:flex-row sm:items-center gap-1 sm:gap-2">
                  <a
                    href={serviceDetailPath(sel.service_id, canonicalSlug)}
                    className="text-sm font-semibold text-slate-700 hover:text-blue-600 transition-colors"
                  >
                    {capitalizeFirstLetter(sel.service_name || "")}
                  </a>
                  {sel.service_version_name && (
                    <span className="max-w-full break-all text-xs text-slate-500 bg-white border border-slate-200 rounded px-1.5 py-0.5">
                      {sel.service_version_name}
                    </span>
                  )}
                </div>
              </div>
              <button
                type="button"
                onClick={() => toggle(key)}
                className="self-end rounded px-1 py-0.5 text-xs text-slate-400 hover:bg-slate-200 hover:text-slate-700 sm:self-auto shrink-0"
              >
                {sel.select_all ? "All operations" : [
                  (sel.endpoint_ids?.length || 0) > 0 ? `${sel.endpoint_ids?.length} operation${sel.endpoint_ids?.length !== 1 ? "s" : ""}` : null,
                  (sel.webhook_ids?.length || 0) > 0 ? `${sel.webhook_ids?.length} webhook${sel.webhook_ids?.length !== 1 ? "s" : ""}` : null,
                ].filter(Boolean).join(" · ") || "0 resources"}
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

export default function SdkDetails() {
  const { id } = useParams();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const toast = useToast();

  const [sdk, setSdk] = useState<Sdk | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const urlTab = searchParams.get("tab");
  const activeTab: "overview" | "docs" | "analytics" =
    urlTab === "docs" || urlTab === "analytics" || urlTab === "overview"
      ? urlTab
      : "overview";

  const [versions, setVersions] = useState<Array<{ id: string; version: string; created_at: string }>>([]);
  const [pendingDrift, setPendingDrift] = useState<PendingDriftItem[]>([]);
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
  } = useWorkspaceNotifications();
  const sdkConfigKey = sdk ? `${sdk.target_type}:${sdk.name}:${sdk.version}` : "";
  // Keep the detail banner focused on outstanding work; acknowledged
  // notifications remain visible from the bell and full notifications page.
  const sdkNotifications = sdk
    ? allNotifications.filter((item) =>
        isPending(item) &&
        (sdk.detailed_selections || []).some((sel) =>
          matchesConfig(item, sdkConfigKey, sel.service_id, sel.service_version_name || "")
        )
      )
    : [];

  const fetchSdk = (artifactId: string) => {
    setLoading(true);
    const queryStr = `
      query {
        sdk(id: "${artifactId}") {
          id
          name
          description
          version
          target_type
          target_language
          sandbox_url
          is_downloadable
          created_at
          downloads
          readme
          detailed_selections {
            service_id
            service_name
            service_slug
            service_provider
            endpoint_ids
            webhook_ids
            select_all
            service_version_id
            service_version_name
          }
        }
      }
    `;
    api.graphql<{ sdk: Sdk }>(queryStr)
      .then(res => {
        setSdk(res.sdk);
        // Fetch version history once we have the name
        if (res.sdk?.name) fetchVersions(res.sdk.name);
      })
      .catch(e => setError(e.message))
      .finally(() => setLoading(false));

    readBucketsForSDK(artifactId).then(state => {
      if (state.sdkBuckets.length > 0) {
        setBucket(state.sdkBuckets[0]);
      } else {
        const defaultBucket = state.buckets.find(b => b.is_default);
        if (defaultBucket) setBucket(defaultBucket);
      }
    }).catch(() => {
      // Ignore failures fetching bucket quietly
    });
  };

  const fetchVersions = (name: string) => {
    const queryStr = `
      query {
        sdkAnalytics(name: "${name}") {
          history {
            id
            version
            created_at
          }
          pending_drift {
            id
            status
            integration_object_id
            webhook_object_id
            diff {
              field
              old_value
              new_value
              severity
              description
            }
          }
        }
      }
    `;
    api.graphql<{ sdkAnalytics: { history: Array<{ id: string; version: string; created_at: string }>; pending_drift: PendingDriftItem[] } }>(queryStr)
      .then(res => {
        // Newest first
        const sorted = [...(res.sdkAnalytics?.history ?? [])].sort(
          (a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime()
        );
        setVersions(sorted);
        setPendingDrift(res.sdkAnalytics?.pending_drift ?? []);
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

  const handleVersionSwitch = (newId: string) => {
    if (newId === id) return;
    navigate(`/integrations/sdks/${newId}`);
  };

  const handleDownload = async () => {
    if (!sdk) return;
    try {
      await api.sdks.download(sdk.id, sdk.name, sdk.version);
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
          <div className="flex flex-wrap items-center gap-3 mt-3 text-sm">
            <span className="px-2.5 py-1 bg-slate-100 text-slate-700 rounded font-medium">{sdk.version}</span>
            {sdk.target_type === "sdk" && <LanguageBadge targetLanguage={sdk.target_language} />}
            <span className="text-slate-600">Created {sdk.created_at ? new Date(sdk.created_at).toLocaleDateString() : ""}</span>
            {bucket && (
              <span className="flex items-center gap-1.5 text-slate-600 bg-slate-50 px-2 py-0.5 rounded border border-slate-200">
                <Database className="w-3.5 h-3.5 text-slate-400" />
                <Link to={`/integrations/buckets?bucket=${encodeURIComponent(bucket.id)}`} className="hover:text-blue-600 transition-colors">
                  {bucket.name}
                </Link>
              </span>
            )}
          </div>
        </div>

        {sdk.is_downloadable && (
          <button
            onClick={handleDownload}
            className="inline-flex w-full md:w-auto items-center justify-center gap-2 px-4 py-2 bg-blue-600 hover:bg-blue-700 text-white text-sm font-medium rounded-lg transition-colors shadow-sm cursor-pointer"
          >
            <Download className="w-4 h-4" />
            Download package
          </button>
        )}
      </div>

      {sdkNotifications.length > 0 && (
        <NotificationBanner
          items={sdkNotifications}
          serviceRefs={notificationServiceRefs}
          onMarkRead={markNotificationRead}
          onDismiss={dismissNotification}
        />
      )}

      {/* Version switcher */}
      {versions.length > 1 && (
        <div className="flex items-center gap-2 flex-wrap">
          <span className="text-xs font-medium text-slate-500 uppercase tracking-wider mr-1">App version</span>
          <div className="flex p-1 bg-slate-100/80 rounded-lg gap-0.5 flex-wrap">
            {versions.map(v => (
              <button
                key={v.id}
                onClick={() => handleVersionSwitch(v.id)}
                className={`px-3 py-1 text-xs font-medium rounded-md transition-all cursor-pointer ${
                  v.id === id
                    ? "bg-white text-slate-900 shadow-sm"
                    : "text-slate-500 hover:text-slate-700"
                }`}
              >
                {v.version}
              </button>
            ))}
          </div>
        </div>
      )}

      {/* Pill Tabs — matches integrations.$id.tsx */}
      <div className="flex overflow-x-auto p-1 bg-slate-100/80 rounded-lg whitespace-nowrap max-w-full [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
        <button
          onClick={() => setActiveTab("overview")}
          className={`px-4 py-1.5 text-sm font-medium rounded-md transition-all cursor-pointer shrink-0 ${
            activeTab === "overview"
              ? "bg-white text-slate-900 shadow-sm"
              : "text-slate-500 hover:text-slate-700"
          }`}
        >
          Overview
        </button>
        {sdk.readme && (
          <button
            onClick={() => setActiveTab("docs")}
            className={`px-4 py-1.5 text-sm font-medium rounded-md transition-all cursor-pointer shrink-0 ${
              activeTab === "docs"
                ? "bg-white text-slate-900 shadow-sm"
                : "text-slate-500 hover:text-slate-700"
            }`}
          >
            Docs
          </button>
        )}
        <button
          onClick={() => setActiveTab("analytics")}
          className={`px-4 py-1.5 text-sm font-medium rounded-md transition-all cursor-pointer shrink-0 ${
            activeTab === "analytics"
              ? "bg-white text-slate-900 shadow-sm"
              : "text-slate-500 hover:text-slate-700"
          }`}
        >
          Activity
        </button>
      </div>

      {/* Tab Content */}
      <div className="p-1 md:p-1">
        {activeTab === "overview" && (
          <div className="space-y-2">

            {/* Connected services — plain subsection, no card */}
            <div>
              <h4 className="text-sm font-semibold text-slate-700 uppercase tracking-wider mb-3">
                Connected services
              </h4>
              <BundledServicesSection
                artifactId={sdk.id}
                selections={sdk.detailed_selections ?? []}
              />
            </div>

            {/* MCP Sandbox URL */}
            {sdk.target_type === "mcp" && sdk.sandbox_url && (
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
                    onClick={() => {
                      navigator.clipboard.writeText(sdk.sandbox_url || "");
                      toast.success("Sandbox URL copied!");
                    }}
                    className="p-3 bg-white border border-slate-200 text-slate-600 rounded-lg hover:bg-slate-50 transition-colors shadow-sm cursor-pointer"
                  >
                    <Copy className="w-5 h-5" />
                  </button>
                </div>
              </div>
            )}
          </div>
        )}

        {activeTab === "docs" && sdk.readme && (
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
        )}

        {activeTab === "analytics" && (
          <div className="space-y-6">
            <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-6">
              <div className="bg-slate-50 p-6 rounded-xl border border-slate-200 shadow-sm flex items-center gap-4">
                <div className="w-12 h-12 bg-blue-100 text-blue-600 rounded-full flex items-center justify-center">
                  <Download className="w-6 h-6" />
                </div>
                <div>
                  <p className="text-sm font-medium text-slate-500">Total Downloads</p>
                  <p className="text-2xl font-bold text-slate-900">{sdk?.downloads || 0}</p>
                </div>
              </div>

              <div className="bg-slate-50 p-6 rounded-xl border border-slate-200 shadow-sm flex items-center gap-4">
                <div className="w-12 h-12 bg-slate-200 text-slate-700 rounded-full flex items-center justify-center">
                  <FileCode className="w-6 h-6" />
                </div>
                <div>
                  <p className="text-sm font-medium text-slate-500">Connected services</p>
                  <p className="text-2xl font-bold text-slate-900">{sdk?.detailed_selections?.length || 0}</p>
                </div>
              </div>

              <div className="bg-slate-50 p-6 rounded-xl border border-slate-200 shadow-sm flex items-center gap-4">
                <div className={`w-12 h-12 rounded-full flex items-center justify-center ${pendingDrift.length > 0 ? 'bg-red-100 text-red-600' : 'bg-slate-100 text-slate-500'}`}>
                  <AlertTriangle className="w-6 h-6" />
                </div>
                <div>
                  <p className="text-sm font-medium text-slate-500">Pending Drift</p>
                  <p className="text-2xl font-bold text-slate-900">{pendingDrift.length}</p>
                </div>
              </div>
            </div>

            <div>
              <h4 className="text-sm font-semibold text-slate-700 uppercase tracking-wider mb-3">
                Pending Drift
              </h4>
              <PendingDriftSection items={pendingDrift} />
            </div>
          </div>
        )}
      </div>
    </div>
  );
}
