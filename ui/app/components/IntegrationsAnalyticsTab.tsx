import { Link } from "@remix-run/react";
import { Activity, ArrowDownToLine, Clock3, Code2, Database, Server, ServerCrash } from "lucide-react";
import type { ComponentType, ReactNode } from "react";
import type { EngineExecutionBreakdown, WorkspaceExecutionAnalytics } from "~/lib/api";

export type WorkspaceActivityRange = "24h" | "7d" | "30d" | "90d";

interface IntegrationsAnalyticsTabProps {
  analytics: WorkspaceExecutionAnalytics;
  range: WorkspaceActivityRange;
  onRangeChange: (range: WorkspaceActivityRange) => void;
}

// IntegrationsAnalyticsTab keeps workspace Activity aggregate-only while the
// scoped App, MCP, and Service pages retain individual execution receipts.
export default function IntegrationsAnalyticsTab({ analytics, range, onRangeChange }: IntegrationsAnalyticsTabProps) {
  return <div className="min-w-0 space-y-5 sm:space-y-7">
    <ActivityRangeSelector value={range} onChange={onRangeChange} />
    <div className="grid grid-cols-2 gap-2.5 sm:gap-3 lg:grid-cols-4">
      <Metric label="Calls" value={analytics.total_calls.toLocaleString()} icon={Activity} />
      <Metric label="Inbound traffic" value={analytics.inbound_calls.toLocaleString()} icon={ArrowDownToLine} />
      <Metric label="Success rate" value={successRate(analytics)} icon={Code2} />
      <Metric label="P95 latency" value={formatLatency(analytics.p95_latency_ms)} icon={Clock3} />
    </div>
    <InboundOverview analytics={analytics} />
    <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
      <AggregateCard label="Most-used SDK" item={analytics.most_used_sdk} icon={Code2} detail={usageDetail(analytics.most_used_sdk)} />
      <AggregateCard label="Most-used service" item={analytics.most_used_service} icon={Server} href={serviceHref(analytics.most_used_service)} detail={usageDetail(analytics.most_used_service)} />
      <AggregateCard label="Most-failed service" item={analytics.most_failed_service} icon={ServerCrash} href={serviceHref(analytics.most_failed_service)} detail={failureDetail(analytics.most_failed_service)} tone="danger" />
      <AggregateCard label="Most-used bucket" item={analytics.most_used_bucket} icon={Database} href={bucketHref(analytics.most_used_bucket)} detail={usageDetail(analytics.most_used_bucket)} />
    </div>
    <ServiceTraffic items={analytics.by_service} />
  </div>;
}

// ActivityRangeSelector maps the fixed reporting windows to one explicit user choice.
function ActivityRangeSelector({ value, onChange }: { value: WorkspaceActivityRange; onChange: (range: WorkspaceActivityRange) => void }) {
  return <div className="flex w-full sm:justify-end">
    <label className="grid w-full gap-1.5 text-sm text-slate-600 sm:flex sm:w-auto sm:items-center sm:gap-2">
      <span>Date range</span>
      <select value={value} onChange={(event) => onChange(event.target.value as WorkspaceActivityRange)} aria-label="Workspace activity date range" className="w-full rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm font-medium text-slate-700 shadow-sm outline-none focus:border-blue-500 focus:ring-2 focus:ring-blue-100 sm:w-auto">
        <option value="24h">Last 24 hours</option>
        <option value="7d">Last 7 days</option>
        <option value="30d">Last 30 days</option>
        <option value="90d">Last 90 days</option>
      </select>
    </label>
  </div>;
}

// InboundOverview distinguishes traffic entering Engine from provider calls leaving it.
function InboundOverview({ analytics }: { analytics: WorkspaceExecutionAnalytics }) {
  const share = analytics.total_calls === 0 ? 0 : (analytics.inbound_calls / analytics.total_calls) * 100;
  return <section className="overflow-hidden rounded-xl border border-slate-200 bg-white shadow-sm" aria-label="Inbound traffic overview">
    <div className="grid gap-4 p-4 sm:grid-cols-[minmax(0,1fr)_auto] sm:items-center sm:gap-6 sm:p-5">
      <div>
        <div className="flex items-center gap-2 text-sm font-semibold text-slate-900"><ArrowDownToLine className="h-4 w-4 text-blue-600" />Inbound traffic overview</div>
        <p className="mt-1 max-w-2xl text-sm leading-5 text-slate-500">Requests received by Engine, including incoming webhook traffic, within the selected reporting window.</p>
        <div className="mt-4 h-2 overflow-hidden rounded-full bg-slate-100"><div className="h-full rounded-full bg-blue-500" style={{ width: `${Math.min(100, share)}%` }} /></div>
        <p className="mt-2 text-xs text-slate-500">{formatPercent(share)} of workspace traffic was inbound.</p>
      </div>
      <div className="flex items-end justify-between gap-4 sm:block sm:text-right"><div className="text-2xl font-semibold tabular-nums text-slate-950 sm:text-3xl">{analytics.inbound_calls.toLocaleString()}</div><div className="pb-1 text-xs font-medium uppercase tracking-wide text-slate-500 sm:mt-1 sm:pb-0">Inbound calls</div></div>
    </div>
  </section>;
}

// AggregateCard links a computed leader to its existing scoped management page.
function AggregateCard({ label, item, icon: Icon, href, detail, tone = "default" }: { label: string; item?: EngineExecutionBreakdown | null; icon: ComponentType<{ className?: string }>; href?: string; detail: string; tone?: "default" | "danger" }) {
  const body = <CardBody label={label} item={item} icon={Icon} detail={detail} tone={tone} />;
  if (!item || !href) return <div className="overflow-hidden rounded-xl border border-slate-200 bg-white shadow-sm">{body}</div>;
  return <Link to={href} className="group overflow-hidden rounded-xl border border-slate-200 bg-white shadow-sm transition hover:border-blue-200 hover:shadow-md">{body}</Link>;
}

// CardBody keeps leader-card layout identical for linked and empty states.
function CardBody({ label, item, icon: Icon, detail, tone }: { label: string; item?: EngineExecutionBreakdown | null; icon: ComponentType<{ className?: string }>; detail: string; tone: "default" | "danger" }) {
  return <div className="relative p-4 sm:min-h-36 sm:p-5">
    <Icon className={`absolute right-4 top-4 h-8 w-8 opacity-10 sm:h-10 sm:w-10 ${tone === "danger" ? "text-red-600" : "text-blue-600"}`} />
    <p className="text-xs font-semibold uppercase tracking-wide text-slate-500">{label}</p>
    <p className="mt-3 truncate pr-8 text-base font-semibold text-slate-900 group-hover:text-blue-700 sm:mt-5 sm:pr-0 sm:text-lg" title={item?.label}>{item?.label || "No activity"}</p>
    <p className={`mt-1 text-xs ${tone === "danger" && item ? "text-red-600" : "text-slate-500"}`}>{detail}</p>
  </div>;
}

// ServiceTraffic keeps the complete aggregate service view available beneath the leader cards.
function ServiceTraffic({ items }: { items: EngineExecutionBreakdown[] }) {
  return <section>
    <div><h2 className="text-sm font-semibold text-slate-950">Service traffic</h2><p className="mt-1 text-xs text-slate-500">Aggregate outbound and inbound activity grouped by service.</p></div>
    <div className="mt-3 overflow-hidden rounded-xl border border-slate-200 bg-white shadow-sm">
      {items.length === 0 ? <EmptyLine>No service traffic in this period.</EmptyLine> : <div className="divide-y divide-slate-100">
        {items.map((item) => <Link key={item.key} to={`/integrations/${item.key}`} className="grid grid-cols-2 gap-x-4 gap-y-3 px-4 py-3 transition-colors hover:bg-slate-50 sm:grid-cols-[minmax(0,1fr)_repeat(4,auto)] sm:items-center sm:gap-6">
          <span className="col-span-2 truncate text-sm font-medium text-slate-800 sm:col-span-1">{item.label}</span>
          <TrafficValue label="Calls" value={item.total_calls} />
          <TrafficValue label="Inbound" value={item.inbound_calls} />
          <TrafficValue label="Failed" value={item.failed_calls} danger={item.failed_calls > 0} />
          <TrafficValue label="P95" value={formatLatency(item.p95_latency_ms)} />
        </Link>)}
      </div>}
    </div>
  </section>;
}

// TrafficValue gives compact labels to service-row aggregate values on every breakpoint.
function TrafficValue({ label, value, danger = false }: { label: string; value: ReactNode; danger?: boolean }) {
  return <span className={`flex min-w-0 flex-col gap-0.5 text-xs tabular-nums sm:block sm:min-w-16 sm:text-right ${danger ? "text-red-600" : "text-slate-500"}`}><span className="text-slate-400 sm:block">{label}</span><span className="truncate">{typeof value === "number" ? value.toLocaleString() : value}</span></span>;
}

// Metric renders one workspace-wide aggregate without implying receipt-level drill-down.
function Metric({ label, value, icon: Icon }: { label: string; value: string; icon: ComponentType<{ className?: string }> }) {
  return <div className="min-w-0 rounded-lg border border-slate-200 bg-white p-3 sm:p-4"><div className="flex min-w-0 items-start gap-1.5 text-xs font-medium leading-4 text-slate-500 sm:items-center sm:gap-2"><Icon className="mt-px h-4 w-4 shrink-0 text-blue-600 sm:mt-0" /><span>{label}</span></div><div className="mt-2 truncate text-lg font-semibold tabular-nums text-slate-950 sm:text-xl" title={value}>{value}</div></div>;
}

// EmptyLine provides a consistent bounded empty state for aggregate lists.
function EmptyLine({ children }: { children: ReactNode }) {
  return <div className="py-10 text-center text-sm text-slate-500">{children}</div>;
}

// formatLatency keeps aggregate durations readable across millisecond and second scales.
function formatLatency(value: number) {
  if (value < 1000) return `${Math.round(value)} ms`;
  return `${(value / 1000).toFixed(2)} s`;
}

// successRate handles empty windows without producing a non-finite percentage.
function successRate(analytics: WorkspaceExecutionAnalytics) {
  if (analytics.total_calls === 0) return "0%";
  return formatPercent((analytics.successful_calls / analytics.total_calls) * 100);
}

// formatPercent uses one decimal only when the aggregate is not a whole number.
function formatPercent(value: number) {
  return `${Number.isInteger(value) ? value.toFixed(0) : value.toFixed(1)}%`;
}

// usageDetail describes a leader using the shared aggregate count.
function usageDetail(item?: EngineExecutionBreakdown | null) {
  return item ? `${item.total_calls.toLocaleString()} calls` : "No calls in this period";
}

// failureDetail distinguishes failure ranking from total-use ranking.
function failureDetail(item?: EngineExecutionBreakdown | null) {
  return item ? `${item.failed_calls.toLocaleString()} failed calls` : "No failures in this period";
}

// serviceHref routes service leaders to the service-scoped Activity surface.
function serviceHref(item?: EngineExecutionBreakdown | null) {
  return item ? `/integrations/${item.key}` : undefined;
}

// bucketHref preserves the existing bucket-list route and selects the winning bucket.
function bucketHref(item?: EngineExecutionBreakdown | null) {
  return item ? `/integrations/buckets?bucket=${encodeURIComponent(item.key)}` : undefined;
}
