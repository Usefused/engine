import { Link } from "@remix-run/react";
import { useEffect, useState, type ReactNode } from "react";
import {
  Bot,
  CheckCircle2,
  ChevronLeft,
  ChevronRight,
  Clock3,
  Code2,
  RotateCcw,
  Loader2,
  Server,
  XCircle,
} from "lucide-react";
import type {
  EngineExecutionAnalyticsSummary,
  EngineExecutionEventEntry,
  ServiceConsumerEntry,
  ServiceGenerationResult,
  PublicServiceInsights,
  WebhookAnalyticsSummary,
  WebhookEventEntry,
} from "~/lib/api";
import { api } from "~/lib/api";
import { WebhookLogsCard } from "~/components/webhooks/WebhookLogsCard";
import { NestedActivityTabs } from "~/components/activity/NestedActivityTabs";
import { ExecutionDetailsDrawer } from "~/components/activity/ExecutionDetailsDrawer";
import { activateReceiptRow } from "~/components/activity/receiptRow";

interface ActivityTabProps {
  res: ServiceGenerationResult;
  executionEvents: EngineExecutionEventEntry[];
  executionTotal: number;
  executionPage: number;
  setExecutionPage: (page: number) => void;
  executionLimit: number;
  executionTransport: string;
  setExecutionTransport: (transport: string) => void;
  executionStatus: string;
  setExecutionStatus: (status: string) => void;
  executionAnalytics: EngineExecutionAnalyticsSummary | null;
  webhookEvents: WebhookEventEntry[];
  webhookTotal: number;
  webhookPage: number;
  setWebhookPage: (page: number | ((prev: number) => number)) => void;
  webhookLimit: number;
  webhookFilterEvent: string;
  setWebhookFilterEvent: (event: string) => void;
  webhookStartDate: string;
  setWebhookStartDate: (date: string) => void;
  webhookEndDate: string;
  setWebhookEndDate: (date: string) => void;
  webhookAnalytics: WebhookAnalyticsSummary | null;
  loadWebhookData: () => void;
  dependentSDKs: ServiceConsumerEntry[];
  dependentMCPs: ServiceConsumerEntry[];
}

function formatLatency(value?: number | null) {
  if (value == null) return "Not recorded";
  if (value < 1000) return `${Math.round(value)} ms`;
  return `${(value / 1000).toFixed(2)} s`;
}

function formatTime(value: string) {
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    timeStyle: "medium",
  }).format(new Date(value));
}

function sourceName(event: EngineExecutionEventEntry, appNames: Map<string, string>) {
  return (event.app_id && appNames.get(event.app_id)) || "Source unavailable";
}

function environmentLabel(event: EngineExecutionEventEntry) {
  if (event.environment) return event.environment;
  if (event.environment_source === "default") return "Service default";
  return "Not recorded";
}

function environmentSourceLabel(event: EngineExecutionEventEntry) {
  if (event.environment_source === "provider") return "Named by the service specification";
  if (event.environment_source === "default") return "Default selected from the service specification";
  return "Not recorded";
}

function timingValue(event: EngineExecutionEventEntry, name: string) {
  return event.timings?.find((timing) => timing.name === name)?.duration_ms;
}

function ExecutionSource({
  event,
  appNames,
}: {
  event: EngineExecutionEventEntry;
  appNames: Map<string, string>;
}) {
  const SourceIcon = event.transport === "mcp" ? Bot : Code2;
  const name = sourceName(event, appNames);
  const kind = (event.app_kind || event.transport).toUpperCase();

  return (
    <div className="flex min-w-0 items-center gap-2 text-slate-700">
      <SourceIcon className="h-4 w-4 shrink-0 text-slate-400" />
      <span className="min-w-0 truncate" title={name}>{name}</span>
      <span className="shrink-0 text-[10px] font-semibold text-slate-400">{kind}</span>
    </div>
  );
}

// ActivityTab keeps service-scoped receipt selection local to the Activity surface.
export default function ActivityTab({
  res,
  executionEvents,
  executionTotal,
  executionPage,
  setExecutionPage,
  executionLimit,
  executionTransport,
  setExecutionTransport,
  executionStatus,
  setExecutionStatus,
  executionAnalytics,
  webhookEvents,
  webhookTotal,
  webhookPage,
  setWebhookPage,
  webhookLimit,
  webhookFilterEvent,
  setWebhookFilterEvent,
  webhookStartDate,
  setWebhookStartDate,
  webhookEndDate,
  setWebhookEndDate,
  webhookAnalytics,
  loadWebhookData,
  dependentSDKs,
  dependentMCPs,
}: ActivityTabProps) {
  const [view, setView] = useState<"outbound" | "webhooks">("outbound");
  const [source, setSource] = useState<"local" | "cross-engine">("local");
  const [selectedExecutionID, setSelectedExecutionID] = useState("");
  const consumers = [...dependentSDKs, ...dependentMCPs];
  const appNames = new Map<string, string>(consumers.map((consumer) => [consumer.id, consumer.name]));
  const selectedExecution = executionEvents.find((event) => event.id === selectedExecutionID) || null;

  return (
    <div className="min-w-0 space-y-8">
      <LocalActivity
        res={res} view={view} setView={setView} source={source} setSource={setSource}
        executionEvents={executionEvents} executionTotal={executionTotal}
        executionPage={executionPage} setExecutionPage={setExecutionPage} executionLimit={executionLimit}
        executionTransport={executionTransport} setExecutionTransport={setExecutionTransport}
        executionStatus={executionStatus} setExecutionStatus={setExecutionStatus} executionAnalytics={executionAnalytics}
        webhookEvents={webhookEvents} webhookTotal={webhookTotal} webhookPage={webhookPage} setWebhookPage={setWebhookPage}
        webhookLimit={webhookLimit} webhookFilterEvent={webhookFilterEvent} setWebhookFilterEvent={setWebhookFilterEvent}
        webhookStartDate={webhookStartDate} setWebhookStartDate={setWebhookStartDate} webhookEndDate={webhookEndDate}
        setWebhookEndDate={setWebhookEndDate} webhookAnalytics={webhookAnalytics} loadWebhookData={loadWebhookData}
        appNames={appNames} selectedExecutionID={selectedExecutionID} setSelectedExecutionID={setSelectedExecutionID}
      />

      <Consumers consumers={consumers} />
      {selectedExecution ? <ExecutionDetailsDrawer event={selectedExecution} onClose={() => setSelectedExecutionID("")}><ExecutionDetails event={selectedExecution} appNames={appNames} /></ExecutionDetailsDrawer> : null}
    </div>
  );
}

type LocalActivityProps = Omit<ActivityTabProps, "dependentSDKs" | "dependentMCPs"> & {
  view: "outbound" | "webhooks";
  setView: (view: "outbound" | "webhooks") => void;
  source: "local" | "cross-engine";
  setSource: (source: "local" | "cross-engine") => void;
  appNames: Map<string, string>;
  selectedExecutionID: string;
  setSelectedExecutionID: (id: string) => void;
};

function LocalActivity(props: LocalActivityProps) {
  return (
    <>
      <NestedActivityTabs
        active={props.view}
        ariaLabel="Service activity"
        onChange={props.setView}
        options={[
          { value: "outbound", label: "Outbound calls" },
          { value: "webhooks", label: "Incoming webhooks" },
        ]}
      />

      {props.view === "outbound" ? (
        <OutboundActivity {...props} />
      ) : (
        <WebhookLogsCard
          webhookNameOptions={Array.from(new Set((props.res.service.webhooks || []).map((webhook) => webhook.name)))}
          webhookEvents={props.webhookEvents} webhookTotal={props.webhookTotal} webhookPage={props.webhookPage}
          setWebhookPage={props.setWebhookPage} webhookLimit={props.webhookLimit} webhookFilterEvent={props.webhookFilterEvent}
          setWebhookFilterEvent={props.setWebhookFilterEvent} webhookStartDate={props.webhookStartDate}
          setWebhookStartDate={props.setWebhookStartDate} webhookEndDate={props.webhookEndDate}
          setWebhookEndDate={props.setWebhookEndDate} webhookAnalytics={props.webhookAnalytics} loadWebhookData={props.loadWebhookData}
        />
      )}
	</>
  );
}

// OutboundActivity lets owners switch data scope without adding another navigation tier.
function OutboundActivity(props: LocalActivityProps) {
  return (
    <section aria-label="Outbound calls" className="min-w-0 space-y-5">
      {props.res.service.is_owner ? (
        <div className="flex justify-end">
          <select
            value={props.source}
            onChange={(event) => props.setSource(event.target.value as "local" | "cross-engine")}
            aria-label="Outbound analytics source"
            className="h-9 w-full rounded-md border border-slate-200 bg-white px-3 text-sm text-slate-700 outline-none focus:border-blue-500 focus:ring-2 focus:ring-blue-100 sm:w-auto"
          >
            <option value="local">Workspace calls</option>
            <option value="cross-engine">Aggregate usage</option>
          </select>
        </div>
      ) : null}
      {props.source === "cross-engine" ? (
        <CrossEngineInsights serviceId={props.res.service.id} isPublic={props.res.service.is_public} />
      ) : (
        <LocalOutboundActivity {...props} />
      )}
    </section>
  );
}

function LocalOutboundActivity(props: LocalActivityProps) {
  return (
    <>
      <ExecutionMetrics analytics={props.executionAnalytics} />
      <div className="flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <h2 className="text-sm font-semibold text-slate-950">Execution receipts</h2>
          <p className="mt-1 text-xs text-slate-500">One receipt for every request Fused sends to this service.</p>
        </div>
        <div className="flex flex-col gap-2 min-[420px]:flex-row">
          <select
            value={props.executionTransport}
            onChange={(event) => props.setExecutionTransport(event.target.value)}
            aria-label="Filter by consumer type"
            className="h-9 rounded-md border border-slate-200 bg-white px-3 text-sm text-slate-700 outline-none focus:border-blue-500 focus:ring-2 focus:ring-blue-100"
          >
            <option value="">All consumers</option>
            <option value="sdk">SDKs</option>
            <option value="mcp">MCP servers</option>
          </select>
          <select
            value={props.executionStatus}
            onChange={(event) => props.setExecutionStatus(event.target.value)}
            aria-label="Filter by result"
            className="h-9 rounded-md border border-slate-200 bg-white px-3 text-sm text-slate-700 outline-none focus:border-blue-500 focus:ring-2 focus:ring-blue-100"
          >
            <option value="">All results</option>
            <option value="success">Successful</option>
            <option value="failed">Failed</option>
          </select>
        </div>
      </div>
      <ExecutionHistory
        events={props.executionEvents}
        appNames={props.appNames}
        selectedExecutionID={props.selectedExecutionID}
        setSelectedExecutionID={props.setSelectedExecutionID}
      />
      <ExecutionPagination total={props.executionTotal} limit={props.executionLimit} page={props.executionPage} setPage={props.setExecutionPage} />
    </>
  );
}

// ExecutionMetrics collapses to one column only on widths that cannot support readable metric pairs.
function ExecutionMetrics({ analytics }: { analytics: EngineExecutionAnalyticsSummary | null }) {
  return <div className="grid grid-cols-1 gap-2 min-[360px]:grid-cols-2 sm:gap-3 lg:grid-cols-4">
    <Metric label="Calls" value={analytics?.total_calls ?? 0} icon={Code2} />
    <Metric label="Failed" value={analytics?.failed_calls ?? 0} icon={XCircle} />
    <Metric label="P50 latency" value={formatLatency(analytics?.median_latency_ms ?? 0)} icon={Clock3} />
    <Metric label="P95 latency" value={formatLatency(analytics?.p95_latency_ms ?? 0)} icon={Clock3} />
  </div>;
}

function ExecutionPagination({ total, limit, page, setPage }: { total: number; limit: number; page: number; setPage: (page: number) => void }) {
  if (total <= limit) return null;
  const pages = Math.max(1, Math.ceil(total / limit));
  return <div className="flex items-center justify-between border-t border-slate-200 pt-3">
    <span className="text-xs text-slate-500">Page {page} of {pages}</span>
    <div className="flex gap-1">
      <PageButton label="Previous page" disabled={page <= 1} onClick={() => setPage(Math.max(1, page - 1))} icon={ChevronLeft} />
      <PageButton label="Next page" disabled={page >= pages} onClick={() => setPage(Math.min(pages, page + 1))} icon={ChevronRight} />
    </div>
  </div>;
}

// CrossEngineInsights keeps delayed public aggregates separate from local receipt history.
function CrossEngineInsights({ serviceId, isPublic }: { serviceId: string; isPublic: boolean }) {
  const [insights, setInsights] = useState<PublicServiceInsights | null>(null);
  const [loading, setLoading] = useState(isPublic);
  const [error, setError] = useState("");
  useEffect(() => {
    if (!isPublic) return;
    const endDate = new Date();
    const startDate = new Date(endDate.getTime() - 30 * 24 * 60 * 60 * 1000);
    api.workspace.getPublicServiceInsights({ serviceId, startDate: startDate.toISOString(), endDate: endDate.toISOString(), granularity: "day" })
      .then(setInsights).catch(() => setError("Cross-engine insights are temporarily unavailable. Activity from this Engine is unaffected."))
      .finally(() => setLoading(false));
  }, [isPublic, serviceId]);
  if (!isPublic) return <ActivityNotice>Cross-engine insights are available after this service is public.</ActivityNotice>;
  if (loading) return <div className="flex items-center gap-2 py-12 text-sm text-slate-500"><Loader2 className="h-4 w-4 animate-spin text-blue-600" />Loading cross-engine insights...</div>;
  if (error) return <ActivityNotice>{error}</ActivityNotice>;
  if (!insights || insights.total_calls === 0) return <ActivityNotice>No other Fused users have used this service in the last 30 days.</ActivityNotice>;
  return (
    <section className="space-y-6" aria-label="Cross-engine service activity">
      <div className="grid grid-cols-1 gap-2 min-[360px]:grid-cols-2 sm:gap-3 lg:grid-cols-4">
        <Metric label="Calls" value={insights.total_calls} icon={Code2} />
        <Metric label="Failed" value={insights.failed_calls} icon={XCircle} />
        <Metric label="P50 latency" value={formatLatency(insights.p50_latency_ms)} icon={Clock3} />
        <Metric label="P95 latency" value={formatLatency(insights.p95_latency_ms)} icon={Clock3} />
      </div>
      <div className="grid gap-8 lg:grid-cols-2">
        <InsightBreakdown title="Top operations" items={insights.top_operations} />
        <InsightBreakdown title="Consumers" items={insights.transport_breakdown} />
      </div>
      <p className="text-xs text-slate-500">{insights.partial_data ? "Showing cached data. " : ""}{insights.data_through ? `Data through ${formatTime(insights.data_through)}.` : "Aggregates are delayed."}</p>
    </section>
  );
}

function InsightBreakdown({ title, items }: { title: string; items: PublicServiceInsights["top_operations"] }) {
  return <div><h2 className="text-sm font-semibold text-slate-950">{title}</h2><div className="mt-3 divide-y divide-slate-100 border-y border-slate-200">{items.map((item) => <div key={item.key} className="flex items-center justify-between gap-4 py-3 text-sm"><span className="min-w-0 truncate text-slate-800">{item.label}</span><span className="shrink-0 text-xs tabular-nums text-slate-500">{item.total_calls} calls · {formatLatency(item.p95_latency_ms)} p95</span></div>)}</div></div>;
}

function ActivityNotice({ children }: { children: ReactNode }) {
  return <div className="rounded-lg border border-slate-200 bg-white px-6 py-12 text-center text-sm text-slate-500">{children}</div>;
}

// Metric remains shrinkable so long localized values cannot widen the mobile page.
function Metric({ label, value, icon: Icon }: { label: string; value: string | number; icon: typeof Code2 }) {
  return (
    <div className="min-w-0 rounded-lg border border-slate-200 bg-white p-3 sm:p-4">
      <div className="flex items-center gap-2 text-xs font-medium text-slate-500">
        <Icon className="h-4 w-4 text-slate-400" />
        {label}
      </div>
      <div className="mt-2 text-xl font-semibold text-slate-950">{value}</div>
    </div>
  );
}

// ExecutionHistory uses scan-friendly cards on phones and the denser table at desktop widths.
function ExecutionHistory({
  events,
  appNames,
  selectedExecutionID,
  setSelectedExecutionID,
}: {
  events: EngineExecutionEventEntry[];
  appNames: Map<string, string>;
  selectedExecutionID: string;
  setSelectedExecutionID: (id: string) => void;
}) {
  // The empty state stays in the same bounded card footprint as populated receipts.
  if (events.length === 0) {
    return (
      <div className="rounded-lg border border-slate-200 bg-white px-6 py-14 text-center">
        <Code2 className="mx-auto h-5 w-5 text-slate-400" />
        <h3 className="mt-3 text-sm font-medium text-slate-900">No outbound calls yet</h3>
        <p className="mx-auto mt-1 max-w-sm text-sm text-slate-500">SDK and MCP server requests will appear here.</p>
      </div>
    );
  }

  return (
    <div className="min-w-0 overflow-hidden rounded-lg border border-slate-200 bg-white">
      <div className="divide-y divide-slate-100 md:hidden">
        {events.map((event) => {
          const selected = event.id === selectedExecutionID;
          return (
            <button
              key={event.id}
              type="button"
              onClick={() => setSelectedExecutionID(event.id)}
              className={`group block w-full min-w-0 p-4 text-left transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-blue-500 ${selected ? "bg-slate-50" : "bg-white hover:bg-slate-50/70"}`}
              aria-haspopup="dialog"
              aria-label={`Inspect ${event.operation}`}
            >
              <div className="flex items-start justify-between gap-3">
                <RequestIdentity event={event} />
                <ChevronRight className="mt-0.5 h-4 w-4 shrink-0 text-slate-400 transition-transform group-hover:translate-x-0.5" />
              </div>
              <div className="mt-3 space-y-2.5">
                <div className="flex min-w-0 items-center justify-between gap-3">
                  <div className="min-w-0 flex-1"><ExecutionSource event={event} appNames={appNames} /></div>
                  <Result event={event} />
                </div>
                <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1 border-t border-slate-100 pt-2 text-[11px] text-slate-500">
                  <span><span className="text-slate-400">Total</span> <span className="font-medium tabular-nums text-slate-700">{formatLatency(event.latency_ms)}</span></span>
                  <time dateTime={event.started_at}>{formatTime(event.started_at)}</time>
                </div>
              </div>
            </button>
          );
        })}
      </div>

      <div className="hidden overflow-x-auto md:block">
        <table className="w-full min-w-[820px] table-fixed text-left text-sm">
          <thead className="border-b border-slate-200 bg-slate-50 text-xs font-medium text-slate-500">
            <tr>
              <th className="w-[30%] px-4 py-3">Request</th>
              <th className="w-[22%] px-4 py-3">Consumer</th>
              <th className="w-[15%] px-4 py-3">Outcome</th>
              <th className="w-[12%] px-4 py-3">Total time</th>
              <th className="w-[17%] px-4 py-3">Started</th>
              <th className="w-[4%] px-2 py-3"><span className="sr-only">Inspect</span></th>
            </tr>
          </thead>
          <tbody className="divide-y divide-slate-100">
            {events.map((event) => {
              const selected = event.id === selectedExecutionID;
              return (
                <tr key={event.id} role="button" tabIndex={0} aria-haspopup="dialog" aria-label={`Inspect ${event.operation}`} onClick={() => setSelectedExecutionID(event.id)} onKeyDown={(keyboardEvent) => activateReceiptRow(keyboardEvent, () => setSelectedExecutionID(event.id))} className={`cursor-pointer focus:outline-none ${selected ? "bg-slate-50" : "hover:bg-slate-50/70"}`}>
                  <td className="px-4 py-3"><RequestIdentity event={event} /></td>
                  <td className="px-4 py-3"><ExecutionSource event={event} appNames={appNames} /></td>
                  <td className="px-4 py-3"><Result event={event} /></td>
                  <td className="px-4 py-3 tabular-nums text-slate-700">{formatLatency(event.latency_ms)}</td>
                  <td className="whitespace-nowrap px-4 py-3 text-xs text-slate-500">{formatTime(event.started_at)}</td>
                  <td className="px-2 py-3 text-slate-400"><ChevronRight className="h-4 w-4" /></td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </div>
  );
}

// RequestIdentity permits two path lines on phones while preserving the compact desktop row.
function RequestIdentity({ event }: { event: EngineExecutionEventEntry }) {
  const request = [event.http_method, event.request_path].filter(Boolean).join(" ");
  return (
    <div className="min-w-0">
      <div className="break-words font-mono text-xs font-medium text-slate-950">{event.operation}</div>
      <div className="mt-1 line-clamp-2 break-all font-mono text-[11px] leading-4 text-slate-400 md:block md:truncate" title={request || undefined}>
        {request || "Provider request details not recorded"}
      </div>
    </div>
  );
}

function Result({ event }: { event: EngineExecutionEventEntry }) {
  const successful = event.status === "success";
  return (
    <span className={`inline-flex items-center gap-1.5 text-xs font-medium ${successful ? "text-emerald-700" : "text-red-700"}`}>
      {successful ? <CheckCircle2 className="h-3.5 w-3.5" /> : <XCircle className="h-3.5 w-3.5" />}
      {successful ? "Successful" : "Failed"}
      {event.provider_http_status ? <span className="text-slate-400">{event.provider_http_status}</span> : null}
    </span>
  );
}

export function ExecutionDetails({ event, appNames }: { event: EngineExecutionEventEntry; appNames: Map<string, string> }) {
  const timingRows = executionTimingRows(event);

  return (
    <section className="border-y border-slate-200 py-6" aria-label="Execution receipt details">
      <div className="flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between">
        <div>
          <div className="text-xs font-medium text-blue-700">Execution receipt</div>
          <h3 className="mt-1 break-words font-mono text-sm font-semibold text-slate-950">{event.operation}</h3>
        </div>
        <Result event={event} />
      </div>

      {event.failure_reason ? (
        <div className="mt-4 rounded-md border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-800">{event.failure_reason}</div>
      ) : null}

      <div className="mt-6 grid gap-6 lg:grid-cols-[1fr_1fr_1.15fr]">
        <DetailGroup title="Request">
          <Detail label="Operation" value={event.operation} mono />
          <Detail label="Service" value={serviceDisplayName(event)} />
          <Detail label="Service slug" value={recordedLabel(event.service_slug)} mono />
          <Detail label="Service version" value={recordedLabel(event.service_version)} />
          <Detail label="Provider request" value={providerRequestLabel(event)} mono />
          <Detail label="Provider" value={recordedLabel(event.provider_host)} mono />
        </DetailGroup>

        <DetailGroup title="Execution context">
          <Detail label="Consumer" value={sourceName(event, appNames)} />
          <Detail label="Access path" value={accessPathLabel(event)} />
          <Detail label="Environment" value={environmentLabel(event)} />
          <Detail label="Environment source" value={environmentSourceLabel(event)} />
          <Detail label="Cache replay" value={cacheReplayLabel(event)} />
          <Detail label="Attempts" value={attemptLabel(event)} />
          <Detail label="Failure type" value={failureTypeLabel(event)} />
        </DetailGroup>

        <DetailGroup title="Timing">
          <div className="space-y-3">
            {timingRows.map((timing) => (
              <TimingRow key={timing.label} label={timing.label} value={timing.value} total={event.latency_ms} />
            ))}
          </div>
        </DetailGroup>
      </div>

      <div className="mt-6 grid gap-4 border-t border-slate-200 pt-5 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-6">
        <Detail label="Started" value={formatTime(event.started_at)} />
        <Detail label="Provider status" value={providerStatusLabel(event)} />
        <Detail label="Service ID" value={recordedLabel(event.service_id)} mono />
        <Detail label="Service version ID" value={recordedLabel(event.service_version_id)} mono />
        <Detail label="Trace ID" value={recordedLabel(event.trace_id)} mono />
        <Detail label="Receipt ID" value={event.id} mono />
      </div>
    </section>
  );
}

function executionTimingRows(event: EngineExecutionEventEntry) {
  const providerLatency = event.provider_latency_ms;
  const engineLatency = providerLatency == null ? null : Math.max(0, event.latency_ms - providerLatency);
  return [
    { label: "Total", value: event.latency_ms }, { label: "Provider round trip", value: providerLatency },
    { label: "Engine work", value: engineLatency }, { label: "Time to provider headers", value: timingValue(event, "provider_time_to_headers") },
    { label: "Credentials", value: timingValue(event, "credentials_resolution") },
  ];
}

function recordedLabel(value?: string) { return value || "Not recorded"; }
/** Prefers current Engine-local service metadata without presenting a UUID as a human title. */
function serviceDisplayName(event: EngineExecutionEventEntry) { return event.service_name || event.service_slug || "Service metadata unavailable"; }
function providerRequestLabel(event: EngineExecutionEventEntry) { return recordedLabel([event.http_method, event.request_path].filter(Boolean).join(" ")); }
function accessPathLabel(event: EngineExecutionEventEntry) { return (event.app_kind || event.transport).toUpperCase(); }
function cacheReplayLabel(event: EngineExecutionEventEntry) { return event.idempotency_replayed ? "Yes, provider was not called" : "No"; }
function attemptLabel(event: EngineExecutionEventEntry) { return String(event.attempt_count || 1); }
function failureTypeLabel(event: EngineExecutionEventEntry) { return recordedLabel([event.failure_category, event.failure_code].filter(Boolean).join(" · ")).replace("Not recorded", "None"); }
function providerStatusLabel(event: EngineExecutionEventEntry) { return event.provider_http_status ? String(event.provider_http_status) : "Not recorded"; }

function DetailGroup({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div>
      <h4 className="mb-3 flex items-center gap-2 text-xs font-semibold text-slate-950">
        <Server className="h-3.5 w-3.5 text-slate-400" />
        {title}
      </h4>
      <dl className="space-y-3">{children}</dl>
    </div>
  );
}

function Detail({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="min-w-0">
      <dt className="text-[11px] text-slate-400">{label}</dt>
      <dd className={`mt-0.5 break-words text-xs text-slate-700 ${mono ? "font-mono" : ""}`} title={value}>{value}</dd>
    </div>
  );
}

function TimingRow({ label, value, total }: { label: string; value?: number | null; total: number }) {
  const width = value == null || total <= 0 ? 0 : Math.min(100, Math.max(2, (value / total) * 100));
  return (
    <div>
      <div className="flex items-center justify-between gap-3 text-[11px]">
        <span className="text-slate-500">{label}</span>
        <span className="font-medium tabular-nums text-slate-700">{formatLatency(value)}</span>
      </div>
      <div className="mt-1 h-1.5 overflow-hidden rounded bg-slate-100">
        <div className="h-full rounded bg-blue-500" style={{ width: `${width}%` }} />
      </div>
    </div>
  );
}

function Consumers({ consumers }: { consumers: ServiceConsumerEntry[] }) {
  return (
    <section className="border-t border-slate-200 pt-7">
      <div className="mb-4">
        <h2 className="text-sm font-semibold text-slate-950">Consumers</h2>
        <p className="mt-1 text-xs text-slate-500">SDKs and MCP servers currently configured to use this service.</p>
      </div>
      {consumers.length > 0 ? (
        <div className="overflow-hidden rounded-lg border border-slate-200 bg-white">
          <div className="divide-y divide-slate-100">
            {consumers.map((consumer) => {
              const ConsumerIcon = consumer.kind === "mcp" ? Bot : Code2;
              const selection = consumer.select_all
                ? "All operations"
                : `${consumer.operation_count} ${consumer.operation_count === 1 ? "operation" : "operations"}`;
              return (
                <div key={consumer.id} className="flex flex-col gap-3 px-4 py-3 sm:flex-row sm:items-center sm:justify-between">
                  <div className="flex min-w-0 items-center gap-3">
                    <ConsumerIcon className="h-4 w-4 shrink-0 text-slate-400" />
                    <div className="min-w-0">
                      <Link
                        to={consumer.kind === "mcp" ? `/integrations/mcp/${consumer.id}/analytics` : `/integrations/sdks/${consumer.id}`}
                        className="block truncate text-sm font-medium text-blue-700 hover:underline"
                      >
                        {consumer.name}
                      </Link>
                      <div className="mt-0.5 text-xs text-slate-500">{consumer.kind.toUpperCase()} · {selection}</div>
                    </div>
                  </div>
                  <div className="flex items-center gap-4 pl-7 text-xs sm:pl-0">
                    {consumer.version ? <span className="text-slate-500">v{consumer.version}</span> : null}
                    <span className={`inline-flex items-center gap-1.5 font-medium ${consumer.active ? "text-emerald-700" : "text-slate-400"}`}>
                      {consumer.active ? <CheckCircle2 className="h-3.5 w-3.5" /> : <RotateCcw className="h-3.5 w-3.5" />}
                      {consumer.active ? "Active" : "Inactive"}
                    </span>
                  </div>
                </div>
              );
            })}
          </div>
        </div>
      ) : (
        <div className="border-y border-slate-200 py-8 text-sm text-slate-500">No SDK or MCP server currently uses this service.</div>
      )}
    </section>
  );
}

function PageButton({ label, disabled, onClick, icon: Icon }: { label: string; disabled: boolean; onClick: () => void; icon: typeof ChevronLeft }) {
  return (
    <button
      type="button"
      title={label}
      aria-label={label}
      onClick={onClick}
      disabled={disabled}
      className="rounded-md border border-slate-200 p-1.5 text-slate-600 disabled:cursor-not-allowed disabled:opacity-40"
    >
      <Icon className="h-4 w-4" />
    </button>
  );
}
