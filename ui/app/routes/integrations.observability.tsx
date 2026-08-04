import { useEffect, useState } from "react";
import { useSearchParams, type MetaFunction } from "@remix-run/react";
import { Loader2 } from "lucide-react";
import { api, type ActivatedService, type WorkspaceExecutionAnalytics, type WebhookEventEntry, type WebhookAnalyticsSummary } from "~/lib/api";
import IntegrationsAnalyticsTab from "~/components/IntegrationsAnalyticsTab";
import { McpAnalyticsPanel, type McpAnalyticsData } from "~/components/mcp/McpAnalyticsPanel";
import { PendingDriftSection, type PendingDriftItem } from "~/components/sdk/PendingDrift";
import { WebhookLogsCard } from "~/components/webhooks/WebhookLogsCard";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m) => m.id === "root").flatMap((m) => m.meta ?? []);
  return [
    ...parentMeta.filter((m) => !('title' in m)),
    { title: "Activity - Fused" },
  ];
};

type ObservabilityTab = "sdk" | "mcp" | "webhooks" | "services";

const TABS: { id: ObservabilityTab; label: string }[] = [
  { id: "services", label: "Overview" },
  { id: "mcp", label: "MCP requests" },
  { id: "webhooks", label: "Webhooks" },
  { id: "sdk", label: "Changes" },
];

function isObservabilityTab(value: string | null): value is ObservabilityTab {
  return value === "sdk" || value === "mcp" || value === "webhooks" || value === "services";
}

export default function ObservabilityPage() {
  const [searchParams, setSearchParams] = useSearchParams();
  const urlTab = searchParams.get("tab");
  const tab: ObservabilityTab = isObservabilityTab(urlTab) ? urlTab : "services";

  const setTab = (next: ObservabilityTab) => {
    setSearchParams(prev => {
      prev.set("tab", next);
      return prev;
    }, { replace: true });
  };

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-xl font-semibold text-slate-900">Activity</h1>
        <p className="text-slate-500 text-sm mt-1">Requests, delivery health, and changes across your workspace.</p>
      </div>

      <div className="flex max-w-full w-fit overflow-x-auto whitespace-nowrap rounded-lg bg-slate-100 p-1 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
        {TABS.map(({ id, label }) => (
          <button
            key={id}
            data-track={`view_observability_${id}_tab`}
            type="button"
            onClick={() => setTab(id)}
            className={`px-4 py-1.5 text-sm font-medium rounded-md transition-all cursor-pointer ${
              tab === id ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700 hover:bg-slate-200/50"
            }`}
          >
            {label}
          </button>
        ))}
      </div>

      {tab === "services" && <ServicesObservability />}
      {tab === "sdk" && <SdkObservability />}
      {tab === "mcp" && <McpObservability />}
      {tab === "webhooks" && <WebhooksObservability />}
    </div>
  );
}

function PickerShell({ children }: { children: React.ReactNode }) {
  return <div className="space-y-4">{children}</div>;
}

function EmptyPickerState({ message }: { message: string }) {
  return (
    <div className="bg-white border border-slate-200 rounded-xl p-10 text-center text-sm text-slate-500">
      {message}
    </div>
  );
}

function LoadingState() {
  return (
    <div className="flex flex-col items-center justify-center py-16 text-slate-400">
      <Loader2 className="w-6 h-6 text-blue-500 animate-spin mb-3" />
      <p className="text-sm">Loading...</p>
    </div>
  );
}

// Service observability is scoped to this Engine workspace. Registry metadata
// is only used to resolve operation counts for services already activated here.
function ServicesObservability() {
  const [analytics, setAnalytics] = useState<WorkspaceExecutionAnalytics | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  useEffect(() => {
    setLoading(true);
    api.workspace.getWorkspaceExecutionAnalytics()
      .then(setAnalytics)
      .catch((e) => setError(e instanceof Error ? e.message : "Failed to load service analytics"))
      .finally(() => setLoading(false));
  }, []);

  if (loading) return <LoadingState />;
  if (error) return <EmptyPickerState message={error} />;
  if (!analytics || analytics.total_calls === 0) return <EmptyPickerState message="No calls in the last 7 days. SDK, MCP server, and webhook activity will appear here." />;
  return <IntegrationsAnalyticsTab analytics={analytics} />;
}

// -- SDKs: picker over the same `sdks(...)` list used on the Apps page,
// then the same `sdkAnalytics(name)` query used on an SDK's detail page. --
function SdkObservability() {
  const [items, setItems] = useState<Array<{ id: string; name: string; version: string }>>([]);
  const [selectedName, setSelectedName] = useState("");
  const [history, setHistory] = useState<Array<{ id: string; version: string; created_at: string }>>([]);
  const [pendingDrift, setPendingDrift] = useState<PendingDriftItem[]>([]);
  const [loadingList, setLoadingList] = useState(true);
  const [loadingAnalytics, setLoadingAnalytics] = useState(false);

  useEffect(() => {
    setLoadingList(true);
    const queryStr = `
      query {
        sdks(limit: 100, offset: 0, target_type: "sdk", latest_only: true) {
          items { id name version }
        }
      }
    `;
    api.graphql<{ sdks: { items: Array<{ id: string; name: string; version: string }> } }>(queryStr)
      .then(res => {
        const list = res.sdks.items ?? [];
        setItems(list);
        setSelectedName(current => current || list[0]?.name || "");
      })
      .catch(() => setItems([]))
      .finally(() => setLoadingList(false));
  }, []);

  useEffect(() => {
    if (!selectedName) return;
    setLoadingAnalytics(true);
    const queryStr = `
      query {
        sdkAnalytics(name: "${selectedName}") {
          history { id version created_at }
          pending_drift {
            id
            status
            integration_object_id
            webhook_object_id
            diff { field old_value new_value severity description }
          }
        }
      }
    `;
    api.graphql<{ sdkAnalytics: { history: Array<{ id: string; version: string; created_at: string }>; pending_drift: PendingDriftItem[] } }>(queryStr)
      .then(res => {
        const sorted = [...(res.sdkAnalytics?.history ?? [])].sort(
          (a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime()
        );
        setHistory(sorted);
        setPendingDrift(res.sdkAnalytics?.pending_drift ?? []);
      })
      .catch(() => {
        setHistory([]);
        setPendingDrift([]);
      })
      .finally(() => setLoadingAnalytics(false));
  }, [selectedName]);

  if (loadingList) return <LoadingState />;
  if (items.length === 0) return <EmptyPickerState message="No apps yet. Create an app to track versions and changes here." />;

  return (
    <PickerShell>
      <SelectPicker
        label="App"
        value={selectedName}
        onChange={setSelectedName}
        options={Array.from(new Set(items.map(i => i.name))).map(name => ({ value: name, label: name }))}
      />
      {loadingAnalytics ? (
        <LoadingState />
      ) : (
        <div className="space-y-6">
          <div className="bg-white rounded-xl border border-slate-200 overflow-hidden">
            <div className="px-5 py-3 border-b border-slate-100 bg-slate-50">
              <h2 className="text-sm font-semibold text-slate-800">Version history</h2>
            </div>
            <div className="divide-y divide-slate-100">
              {history.length > 0 ? history.map(v => (
                <div key={v.id} className="px-5 py-3 flex items-center justify-between text-sm">
                  <span className="font-medium text-slate-800">{v.version}</span>
                  <span className="text-slate-400 text-xs">{new Date(v.created_at).toLocaleDateString()}</span>
                </div>
              )) : (
                <div className="p-8 text-center text-slate-500 text-sm">No version history yet.</div>
              )}
            </div>
          </div>
          <div>
            <h2 className="text-sm font-semibold text-slate-800 mb-3">Pending drift</h2>
            <PendingDriftSection items={pendingDrift} />
          </div>
        </div>
      )}
    </PickerShell>
  );
}

// -- MCP: picker over the same `mcpServers(...)` list used on the MCP servers
// page, then the same `mcpAnalytics(artifactId)` query used on a server's
// own diagnostics route (see components/mcp/McpAnalyticsPanel.tsx). --
function McpObservability() {
  const [items, setItems] = useState<Array<{ id: string; name: string; active: boolean }>>([]);
  const [selectedId, setSelectedId] = useState("");
  const [data, setData] = useState<McpAnalyticsData | null>(null);
  const [loadingList, setLoadingList] = useState(true);
  const [loadingAnalytics, setLoadingAnalytics] = useState(false);

  useEffect(() => {
    setLoadingList(true);
    const queryStr = `
      query {
        mcpServers(limit: 100, offset: 0) {
          items { id name active }
        }
      }
    `;
    api.mcpGraphql<{ mcpServers: { items: Array<{ id: string; name: string; active: boolean }> } }>(queryStr)
      .then(res => {
        const list = res.mcpServers.items ?? [];
        setItems(list);
        setSelectedId(current => current || list[0]?.id || "");
      })
      .catch(() => setItems([]))
      .finally(() => setLoadingList(false));
  }, []);

  useEffect(() => {
    if (!selectedId) return;
    setLoadingAnalytics(true);
    const queryStr = `
      query($id: String!) {
        mcpAnalytics(artifactId: $id) {
          total_requests
          failed_requests
          average_latency
          active_agents
          tool_usage { tool_name count failed average_latency }
          service_usage { service_name count failed average_latency }
        }
      }
    `;
    api.mcpGraphql<{ mcpAnalytics: McpAnalyticsData }>(queryStr, { id: selectedId })
      .then(res => setData(res.mcpAnalytics))
      .catch(() => setData(null))
      .finally(() => setLoadingAnalytics(false));
  }, [selectedId]);

  if (loadingList) return <LoadingState />;
  if (items.length === 0) return <EmptyPickerState message="No MCP servers yet. Create one to see request activity here." />;

  return (
    <PickerShell>
      <SelectPicker
        label="MCP server"
        value={selectedId}
        onChange={setSelectedId}
        options={items.map(i => ({ value: i.id, label: i.active ? i.name : `${i.name} (killed)` }))}
      />
      {loadingAnalytics || !data ? <LoadingState /> : <McpAnalyticsPanel data={data} />}
    </PickerShell>
  );
}

// -- Webhooks: picker over the workspace's activated services (what the
// current actor has permission to view), then the same
// api.workspace.listWebhookEvents / getWebhookAnalytics calls the per-service
// Analytics tab uses. --
function WebhooksObservability() {
  const [services, setServices] = useState<ActivatedService[]>([]);
  const [selectedServiceId, setSelectedServiceId] = useState("");
  const [loadingList, setLoadingList] = useState(true);
  const [loadingEvents, setLoadingEvents] = useState(false);
  const [events, setEvents] = useState<WebhookEventEntry[]>([]);
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(1);
  const [filterEvent, setFilterEvent] = useState("");
  const [startDate, setStartDate] = useState("");
  const [endDate, setEndDate] = useState("");
  const [analytics, setAnalytics] = useState<WebhookAnalyticsSummary | null>(null);
  const limit = 10;

  useEffect(() => {
    setLoadingList(true);
    api.workspace.getServices()
      .then(list => {
        setServices(list);
        setSelectedServiceId(current => current || list[0]?.service_id || "");
      })
      .catch(() => setServices([]))
      .finally(() => setLoadingList(false));
  }, []);

  const loadWebhookData = () => {
    if (!selectedServiceId) return;
    setLoadingEvents(true);
    const params: Parameters<typeof api.workspace.listWebhookEvents>[0] = {
      serviceId: selectedServiceId,
      limit,
      offset: (page - 1) * limit,
      eventName: filterEvent || undefined,
    };
    if (startDate) {
      params.startDate = new Date(startDate).toISOString();
    }
    if (endDate) {
      const end = new Date(endDate);
      end.setHours(23, 59, 59, 999);
      params.endDate = end.toISOString();
    }
    Promise.all([
      api.workspace.listWebhookEvents(params),
      api.workspace.getWebhookAnalytics(params),
    ])
      .then(([eventsRes, analyticsRes]) => {
        setEvents(eventsRes.items || []);
        setTotal(eventsRes.total || 0);
        setAnalytics(analyticsRes);
      })
      .catch(() => {
        setEvents([]);
        setTotal(0);
        setAnalytics(null);
      })
      .finally(() => setLoadingEvents(false));
  };

  useEffect(() => {
    loadWebhookData();
  }, [selectedServiceId, page, filterEvent, startDate, endDate]);

  if (loadingList) return <LoadingState />;
  if (services.length === 0) return <EmptyPickerState message="No services added yet. Add one to see webhook activity here." />;

  return (
    <PickerShell>
      <SelectPicker
        label="Service"
        value={selectedServiceId}
        onChange={(value) => { setSelectedServiceId(value); setPage(1); }}
        options={services.map(s => ({ value: s.service_id, label: s.service_name }))}
      />
      {loadingEvents ? (
        <LoadingState />
      ) : (
        <WebhookLogsCard
          webhookNameOptions={[]}
          webhookEvents={events}
          webhookTotal={total}
          webhookPage={page}
          setWebhookPage={setPage}
          webhookLimit={limit}
          webhookFilterEvent={filterEvent}
          setWebhookFilterEvent={(value) => { setFilterEvent(value); setPage(1); }}
          webhookStartDate={startDate}
          setWebhookStartDate={(value) => { setStartDate(value); setPage(1); }}
          webhookEndDate={endDate}
          setWebhookEndDate={(value) => { setEndDate(value); setPage(1); }}
          webhookAnalytics={analytics}
          loadWebhookData={loadWebhookData}
        />
      )}
    </PickerShell>
  );
}

function SelectPicker({
  label,
  value,
  onChange,
  options,
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  options: Array<{ value: string; label: string }>;
}) {
  return (
    <div className="flex items-center gap-3">
      <label className="text-sm font-medium text-slate-700">{label}</label>
      <select
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="text-sm border border-slate-300 rounded-lg px-3 py-2 bg-white focus:outline-none focus:ring-2 focus:ring-blue-500/50 focus:border-blue-500"
      >
        {options.map(opt => (
          <option key={opt.value} value={opt.value}>{opt.label}</option>
        ))}
      </select>
    </div>
  );
}
