import { useEffect, useState } from "react";
import { Activity, CheckCircle2, Clock3, RefreshCw, ServerCrash, ShieldAlert } from "lucide-react";
import { ActivityMetric } from "~/components/activity/AppActivityOverview";
import { WebhookLogsCard } from "./WebhookLogsCard";
import { webhookRegistrationAnalytics, webhookRegistrationEvents, type RegistrationAnalytics, type RegistrationEventPage, type WebhookActivityInput } from "~/lib/webhook-detail-api";
import { webhookEventNameOptions, type WebhookEventStatus } from "~/lib/webhook-detail";

/** Cancels obsolete reports and clears stale values so failed refreshes never look like verified zero activity. */
function useRegistrationReport<T>(load: (input: WebhookActivityInput, signal: AbortSignal) => Promise<T>, input: WebhookActivityInput, refresh: number) {
  const key = JSON.stringify([input, refresh]);
  const [state, setState] = useState<{ key: string; data: T | null; error: string }>({ key: "", data: null, error: "" });
  useEffect(() => {
    let active = true;
    const controller = new AbortController();
    setState({ key, data: null, error: "" });
    load(input, controller.signal).then((data) => {
      // A cancelled registration or filter no longer owns this component's displayed report.
      if (active) setState({ key, data, error: "" });
    }).catch(() => {
      // Keep transport details out of the UI and offer a bounded, explicit retry instead of fabricated empty data.
      if (active) setState({ key, data: null, error: "Webhook activity could not be loaded. Refresh to try again." });
    });
    // Navigation and filter changes stop the underlying network request as well as stale state updates.
    return () => { active = false; controller.abort(); };
    // The serialized key includes every reporting filter; the imported loader is stable across renders.
  }, [key, load]);
  // New filters hide the previous result immediately, even before their effect begins.
  const current = state.key === key ? state : { data: null, error: "" };
  return { ...current, loading: !current.data && !current.error };
}

/** Uses the app analytics latency convention for the same canonical receipt measurements. */
function latency(value: number): string {
  // Subsecond deliveries remain readable without rounding slow requests down to milliseconds.
  return value < 1000 ? `${Math.round(value)} ms` : `${(value / 1000).toFixed(2)} s`;
}

/** Shows the same period controls and explicit refresh action on registration analytics and received events. */
function ReportControls({ start, end, onStart, onEnd, onRefresh }: { start: string; end: string; onStart: (value: string) => void; onEnd: (value: string) => void; onRefresh: () => void }) {
  return <div className="flex min-w-0 flex-wrap items-end gap-3">
    <label className="min-w-0 flex-1 text-xs text-slate-500 sm:flex-none">From (UTC)<input aria-label="Webhook start date" type="date" value={start} onChange={(event) => onStart(event.target.value)} className="mt-1 block h-9 w-full min-w-0 rounded-md border border-slate-200 bg-white px-2 text-sm text-slate-700" /></label>
    <label className="min-w-0 flex-1 text-xs text-slate-500 sm:flex-none">To (UTC)<input aria-label="Webhook end date" type="date" value={end} onChange={(event) => onEnd(event.target.value)} className="mt-1 block h-9 w-full min-w-0 rounded-md border border-slate-200 bg-white px-2 text-sm text-slate-700" /></label>
    <button type="button" aria-label="Refresh webhook activity" onClick={onRefresh} className="inline-flex h-9 items-center gap-2 rounded-md border border-slate-200 bg-white px-3 text-sm text-slate-600 hover:bg-slate-50"><RefreshCw className="h-3.5 w-3.5" />Refresh</button>
  </div>;
}

/** Matches app analytics cards while distinguishing verification rejections from delivery failures. */
function RegistrationMetrics({ analytics }: { analytics: RegistrationAnalytics | null }) {
  // Loading and unavailable summaries remain dashes; an actual empty report legitimately displays zeros.
  const number = (value: number | undefined) => value?.toLocaleString() ?? "—";
  // A verified empty report has a zero failure rate; pending/unavailable reports make no outcome claim.
  const failureRate = analytics ? `${(analytics.total_ingested ? analytics.total_failed / analytics.total_ingested * 100 : 0).toFixed(1)}% delivery failure rate` : undefined;
  return <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
    <ActivityMetric label="Received events" value={number(analytics?.total_ingested)} detail="This registration only" icon={Activity} />
    <ActivityMetric label="Delivered events" value={number(analytics?.total_delivered)} icon={CheckCircle2} />
    <ActivityMetric label="Failed events" value={number(analytics?.total_failed)} detail={failureRate} icon={ServerCrash} />
    <ActivityMetric label="Rejected events" value={number(analytics?.total_rejected)} detail="Rejected during verification" icon={ShieldAlert} />
    {/* Missing latency values must not appear as measured zero-duration deliveries. */}
    <ActivityMetric label="Average latency" value={analytics ? latency(analytics.average_latency_ms) : "—"} icon={Clock3} />
    <ActivityMetric label="P95 latency" value={analytics ? latency(analytics.p95_latency_ms) : "—"} icon={Clock3} />
  </div>;
}

/** Provides an app-style outcome breakdown with direct navigation into the matching receipt set. */
function DeliveryOutcomes({ analytics, onEvents }: { analytics: RegistrationAnalytics; onEvents: (status: WebhookEventStatus) => void }) {
  const rows: Array<{ label: string; count: number; status: WebhookEventStatus }> = [
    { label: "All received events", count: analytics.total_ingested, status: "" },
    { label: "Rejected events", count: analytics.total_rejected, status: "rejected" },
    { label: "Failed or rejected events", count: analytics.total_failed + analytics.total_rejected, status: "failed" },
  ];
  return <section className="overflow-hidden rounded-xl border border-slate-200 bg-white shadow-sm">
    <div className="border-b border-slate-100 px-5 py-4"><h2 className="font-semibold text-slate-900">Delivery outcomes</h2><p className="mt-0.5 text-xs text-slate-500">Receipts for this receiving URL in the selected period.</p></div>
    <div className="divide-y divide-slate-100">{rows.map((row) => <button key={row.label} type="button" onClick={() => onEvents(row.status)} className="flex w-full flex-wrap items-center justify-between gap-3 px-5 py-4 text-left text-sm hover:bg-slate-50"><span className="font-medium text-slate-800">{row.label}</span><span className="flex items-center gap-4"><span className="tabular-nums text-slate-700">{row.count.toLocaleString()}</span><span className="text-xs text-[var(--brand-violet)]">View events →</span></span></button>)}</div>
  </section>;
}

/** Loads metrics and outcome drill-downs from this registration's existing canonical execution history. */
export function WebhookRegistrationAnalytics({ serviceId, registrationId, start, end, onStart, onEnd, onEvents }: { serviceId: string; registrationId: string; start: string; end: string; onStart: (value: string) => void; onEnd: (value: string) => void; onEvents: (status: WebhookEventStatus) => void }) {
  const [refresh, setRefresh] = useState(0);
  const report = useRegistrationReport(webhookRegistrationAnalytics, { serviceId, registrationId, startDate: start, endDate: end }, refresh);
  return <div className="min-w-0 max-w-full space-y-6 overflow-x-hidden">
    <ReportControls start={start} end={end} onStart={onStart} onEnd={onEnd} onRefresh={() => setRefresh((value) => value + 1)} />
    <RegistrationMetrics analytics={report.data} />
    {/* Failures, pending loads, and confirmed empty data are distinct states across every Activity surface. */}
    {report.error ? <p role="alert" className="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700">{report.error}</p> : report.loading ? <p role="status" className="py-8 text-center text-sm text-slate-500">Loading webhook analytics…</p> : report.data && <DeliveryOutcomes analytics={report.data} onEvents={onEvents} />}
  </div>;
}

/** Retains server-side outcome filtering and pagination while reusing the shared mobile/table receipt inspector. */
export function WebhookRegistrationEvents({ serviceId, registrationId, initialStatus = "", start, end, onStart, onEnd }: { serviceId: string; registrationId: string; initialStatus?: WebhookEventStatus; start: string; end: string; onStart: (value: string) => void; onEnd: (value: string) => void }) {
  const [status, setStatus] = useState<WebhookEventStatus>(initialStatus);
  const [eventName, setEventName] = useState("");
  const [page, setPage] = useState(1);
  const [refresh, setRefresh] = useState(0);
  const pageSize = 20;
  const report = useRegistrationReport(webhookRegistrationEvents, { serviceId, registrationId, status, eventName, startDate: start, endDate: end, limit: pageSize, offset: (page - 1) * pageSize }, refresh);
  // Keep the active selection across pages without duplicating the shared UNKNOWN rejection option.
  const names = webhookEventNameOptions(report.data?.items.map((event) => event.event_name) ?? [], eventName);
  /** Period changes restart SQL paging even while the previous report is loading or unavailable. */
  function changeStart(value: string) { onStart(value); setPage(1); }
  /** End bounds use the same pagination reset as start bounds and outcome filters. */
  function changeEnd(value: string) { onEnd(value); setPage(1); }
  return <div className="min-w-0 max-w-full space-y-5 overflow-x-hidden">
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div><h2 className="text-sm font-semibold text-slate-900">Received events</h2><p className="mt-1 text-xs text-slate-500">Only deliveries received by this webhook registration.</p></div>
      <div aria-label="Filter webhook outcomes" className="flex max-w-full flex-wrap gap-1 rounded-lg bg-slate-100 p-1">{([{ value: "", label: "All" }, { value: "success", label: "Successful" }, { value: "failed", label: "Failures" }, { value: "rejected", label: "Rejected" }] as const).map((option) => <button type="button" key={option.value} aria-pressed={status === option.value} onClick={() => { setStatus(option.value); setPage(1); }} className={`rounded-md px-3 py-2 text-xs font-medium sm:py-1.5 ${status === option.value ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700"}`}>{option.label}</button>)}</div>
    </div>
    {/* The shared receipt card owns period controls once data is loaded; these preserve retry access during loading and failure. */}
    {(report.loading || report.error) && <ReportControls start={start} end={end} onStart={changeStart} onEnd={changeEnd} onRefresh={() => setRefresh((value) => value + 1)} />}
    {report.error ? <p role="alert" className="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700">{report.error}</p> : report.loading ? <p role="status" className="py-12 text-center text-sm text-slate-500">Loading received events…</p> : report.data && <WebhookLogsCard
      webhookNameOptions={names} webhookEvents={report.data.items} webhookTotal={report.data.total} webhookPage={page} setWebhookPage={setPage} webhookLimit={pageSize}
      webhookFilterEvent={eventName} setWebhookFilterEvent={(value) => { setEventName(value); setPage(1); }}
      webhookStartDate={start} setWebhookStartDate={changeStart} webhookEndDate={end} setWebhookEndDate={changeEnd}
      webhookAnalytics={null} showSummary={false} loadWebhookData={() => setRefresh((value) => value + 1)} emptyMessage="No received events match these filters."
    />}
  </div>;
}
