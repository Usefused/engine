import { useEffect, useMemo, useState, type ComponentType } from "react";
import { Activity, AlertTriangle, Clock3, Download, FileCode, ServerCrash } from "lucide-react";
import { api, type ArtifactExecutionAnalytics, type EngineExecutionBreakdown } from "~/lib/api";

export interface AppActivityService {
  service_id: string;
  service_name?: string;
}

interface AppActivityOverviewProps {
  artifactId: string;
  downloads: number;
  pendingDriftCount: number;
  services: AppActivityService[];
}

interface ServiceUsageRow extends EngineExecutionBreakdown {
  name: string;
}

function formatLatency(value: number): string {
  if (value < 1000) return `${Math.round(value)} ms`;
  return `${(value / 1000).toFixed(2)} s`;
}

function failureRate(analytics: ArtifactExecutionAnalytics | null): string {
  if (!analytics?.total_calls) return "0%";
  return `${((analytics.failed_calls / analytics.total_calls) * 100).toFixed(1)}%`;
}

function usageRows(services: AppActivityService[], analytics: ArtifactExecutionAnalytics | null): ServiceUsageRow[] {
  const usageByService = new Map((analytics?.by_service ?? []).map((item) => [item.key, item]));
  return services.map((service) => {
    const usage = usageByService.get(service.service_id);
    return {
      key: service.service_id,
      label: usage?.label ?? service.service_id,
      name: service.service_name || "Unnamed service",
      total_calls: usage?.total_calls ?? 0,
      failed_calls: usage?.failed_calls ?? 0,
      p95_latency_ms: usage?.p95_latency_ms ?? 0,
    };
  });
}

function Metric({ label, value, detail, icon: Icon }: { label: string; value: string | number; detail?: string; icon: ComponentType<{ className?: string }> }) {
  return (
    <div className="relative overflow-hidden rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
      <Icon className="absolute right-4 top-4 h-12 w-12 text-slate-900 opacity-5" />
      <p className="text-sm font-medium text-slate-500">{label}</p>
      <p className="mt-1 text-2xl font-bold text-slate-900">{value}</p>
      {detail ? <p className="mt-1 text-xs text-slate-500">{detail}</p> : null}
    </div>
  );
}

function ServiceUsageTable({ rows }: { rows: ServiceUsageRow[] }) {
  return (
    <div className="overflow-hidden rounded-xl border border-slate-200 bg-white shadow-sm">
      <div className="border-b border-slate-100 px-5 py-4">
        <h3 className="font-semibold text-slate-900">Service usage</h3>
        <p className="mt-0.5 text-xs text-slate-500">Requests executed with this app, grouped by bundled service.</p>
      </div>
      {rows.length === 0 ? (
        <div className="py-12 text-center text-sm text-slate-500">No services are bundled in this app.</div>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[640px] text-left text-sm">
            <thead className="bg-slate-50 text-xs uppercase tracking-wide text-slate-500">
              <tr><th className="px-5 py-3">Service</th><th className="px-5 py-3 text-right">Requests</th><th className="px-5 py-3 text-right">Failed</th><th className="px-5 py-3 text-right">P95 latency</th><th className="px-5 py-3 text-right">Success rate</th></tr>
            </thead>
            <tbody className="divide-y divide-slate-100">
              {rows.map((row) => {
                const successRate = row.total_calls === 0 ? 0 : ((row.total_calls - row.failed_calls) / row.total_calls) * 100;
                return (
                  <tr key={row.key}>
                    <td className="px-5 py-4 font-medium text-slate-800">{row.name}</td>
                    <td className="px-5 py-4 text-right tabular-nums text-slate-700">{row.total_calls.toLocaleString()}</td>
                    <td className={`px-5 py-4 text-right tabular-nums ${row.failed_calls > 0 ? "font-medium text-red-600" : "text-slate-400"}`}>{row.failed_calls.toLocaleString()}</td>
                    <td className="px-5 py-4 text-right tabular-nums text-slate-600">{formatLatency(row.p95_latency_ms)}</td>
                    <td className="px-5 py-4 text-right tabular-nums text-slate-600">{successRate.toFixed(1)}%</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

export function AppActivityOverview({ artifactId, downloads, pendingDriftCount, services }: AppActivityOverviewProps) {
  const [analytics, setAnalytics] = useState<ArtifactExecutionAnalytics | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    setError("");
    api.workspace.getArtifactExecutionAnalytics({ artifactId, transport: "sdk" })
      .then(setAnalytics)
      .catch((cause) => setError(cause instanceof Error ? cause.message : "Failed to load app analytics"));
  }, [artifactId]);

  const rows = useMemo(() => usageRows(services, analytics), [analytics, services]);
  const requestValue = analytics ? analytics.total_calls.toLocaleString() : "—";
  const failedValue = analytics ? analytics.failed_calls.toLocaleString() : "—";
  const latencyValue = analytics ? formatLatency(analytics.average_latency_ms) : "—";

  return (
    <div className="space-y-6">
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
        <Metric label="Requests" value={requestValue} detail="Executed with this app" icon={Activity} />
        <Metric label="Failed requests" value={failedValue} detail={`${failureRate(analytics)} failure rate`} icon={ServerCrash} />
        <Metric label="Average latency" value={latencyValue} icon={Clock3} />
        <Metric label="Total downloads" value={downloads.toLocaleString()} icon={Download} />
        <Metric label="Connected services" value={services.length.toLocaleString()} icon={FileCode} />
        <Metric label="Pending drift" value={pendingDriftCount.toLocaleString()} icon={AlertTriangle} />
      </div>
      {error ? <div className="rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">Execution analytics could not be loaded: {error}</div> : null}
      <ServiceUsageTable rows={rows} />
    </div>
  );
}
