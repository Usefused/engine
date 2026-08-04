import { Activity, AlertCircle, Bot, Clock3, Code2, Webhook } from "lucide-react";
import type { WorkspaceExecutionAnalytics } from "~/lib/api";

interface IntegrationsAnalyticsTabProps {
  analytics: WorkspaceExecutionAnalytics;
}

function formatLatency(value: number) {
  if (value < 1000) return `${Math.round(value)} ms`;
  return `${(value / 1000).toFixed(2)} s`;
}

function formatTime(value: string) {
  return new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" }).format(new Date(value));
}

function successRate(analytics: WorkspaceExecutionAnalytics) {
  if (analytics.total_calls === 0) return "0%";
  return `${Math.round((analytics.successful_calls / analytics.total_calls) * 1000) / 10}%`;
}

function transportIcon(key: string) {
  if (key === "mcp") return Bot;
  if (key === "webhook") return Webhook;
  return Code2;
}

export default function IntegrationsAnalyticsTab({ analytics }: IntegrationsAnalyticsTabProps) {
  return (
    <div className="space-y-7">
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Metric label="Calls" value={analytics.total_calls} icon={Activity} />
        <Metric label="Success rate" value={successRate(analytics)} icon={Code2} />
        <Metric label="P50 latency" value={formatLatency(analytics.median_latency_ms)} icon={Clock3} />
        <Metric label="P95 latency" value={formatLatency(analytics.p95_latency_ms)} icon={Clock3} />
      </div>

      <div className="grid gap-8 lg:grid-cols-2">
        <Breakdown title="Calls by service" items={analytics.by_service} />
        <div>
          <h2 className="text-sm font-semibold text-slate-950">Consumers</h2>
          <div className="mt-3 divide-y divide-slate-100 border-y border-slate-200">
            {analytics.by_transport.map((item) => {
              const Icon = transportIcon(item.key);
              return (
                <div key={item.key} className="flex items-center justify-between gap-4 py-3">
                  <div className="flex min-w-0 items-center gap-2 text-sm text-slate-700">
                    <Icon className="h-4 w-4 shrink-0 text-slate-400" />
                    <span>{item.label}</span>
                  </div>
                  <BreakdownNumbers total={item.total_calls} failed={item.failed_calls} latency={item.p95_latency_ms} />
                </div>
              );
            })}
            {analytics.by_transport.length === 0 ? <EmptyLine>Calls from SDKs, MCP servers, and webhooks will appear here.</EmptyLine> : null}
          </div>
        </div>
      </div>

      <div>
        <h2 className="text-sm font-semibold text-slate-950">Recent failures</h2>
        <div className="mt-3 overflow-hidden rounded-lg border border-slate-200 bg-white">
          {analytics.recent_failures.map((failure) => (
            <div key={failure.id} className="flex flex-col gap-2 border-b border-slate-100 px-4 py-3 last:border-b-0 sm:flex-row sm:items-center sm:justify-between">
              <div className="min-w-0">
                <div className="flex items-center gap-2 text-sm font-medium text-slate-900">
                  <AlertCircle className="h-4 w-4 shrink-0 text-red-500" />
                  <span className="truncate">{failure.service_name}</span>
                  <span className="truncate font-mono text-xs text-slate-500">{failure.operation}</span>
                </div>
                <p className="mt-1 line-clamp-2 text-xs text-slate-500">
                  {[failure.failure_category, failure.failure_code, failure.failure_reason].filter(Boolean).join(" · ") || "Execution failed"}
                </p>
              </div>
              <div className="flex shrink-0 items-center gap-3 text-xs text-slate-500">
                <span>{failure.transport.toUpperCase()}</span>
                <span>{formatLatency(failure.latency_ms)}</span>
                <span>{formatTime(failure.started_at)}</span>
              </div>
            </div>
          ))}
          {analytics.recent_failures.length === 0 ? <EmptyLine>No failed calls in this period.</EmptyLine> : null}
        </div>
      </div>
    </div>
  );
}

function Metric({ label, value, icon: Icon }: { label: string; value: string | number; icon: typeof Activity }) {
  return (
    <div className="rounded-lg border border-slate-200 bg-white p-4">
      <div className="flex items-center gap-2 text-xs font-medium text-slate-500">
        <Icon className="h-4 w-4 text-blue-600" />
        {label}
      </div>
      <div className="mt-2 text-xl font-semibold text-slate-950">{value}</div>
    </div>
  );
}

function Breakdown({ title, items }: { title: string; items: WorkspaceExecutionAnalytics["by_service"] }) {
  return (
    <div>
      <h2 className="text-sm font-semibold text-slate-950">{title}</h2>
      <div className="mt-3 divide-y divide-slate-100 border-y border-slate-200">
        {items.map((item) => (
          <div key={item.key} className="flex items-center justify-between gap-4 py-3">
            <span className="min-w-0 truncate text-sm font-medium text-slate-800">{item.label}</span>
            <BreakdownNumbers total={item.total_calls} failed={item.failed_calls} latency={item.p95_latency_ms} />
          </div>
        ))}
        {items.length === 0 ? <EmptyLine>Service calls will appear here.</EmptyLine> : null}
      </div>
    </div>
  );
}

function BreakdownNumbers({ total, failed, latency }: { total: number; failed: number; latency: number }) {
  return (
    <div className="flex shrink-0 items-center gap-4 text-xs tabular-nums text-slate-500">
      <span>{total} calls</span>
      <span className={failed > 0 ? "text-red-600" : "text-slate-400"}>{failed} failed</span>
      <span>{formatLatency(latency)} p95</span>
    </div>
  );
}

function EmptyLine({ children }: { children: React.ReactNode }) {
  return <div className="py-8 text-center text-sm text-slate-500">{children}</div>;
}
