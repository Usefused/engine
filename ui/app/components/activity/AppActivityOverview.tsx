import { useEffect, useMemo, useState, type ComponentType } from "react";
import { Activity, AlertTriangle, Clock3, Download, FileCode, ServerCrash } from "lucide-react";
import { api, type AppExecutionAnalytics, type EngineExecutionBreakdown } from "~/lib/api";
import { appActivityIssue, type AppActivityIssue } from "~/lib/app-activity-error";
import { formatAppDownloadCount } from "~/lib/app-downloads";

export interface AppActivityService {
  service_id: string;
  service_name?: string;
}

interface AppActivityOverviewProps {
  appId: string;
  downloads: string | null;
  pendingDriftCount: number;
  services: AppActivityService[];
}

interface ServiceUsageRow extends EngineExecutionBreakdown {
  name: string;
}

// formatLatency keeps millisecond values compact while preserving readable
// seconds for slower providers.
function formatLatency(value: number): string {
  if (value < 1000) return `${Math.round(value)} ms`;
  return `${(value / 1000).toFixed(2)} s`;
}

// failureRate presents an empty app as a stable zero-rate state.
function failureRate(analytics: AppExecutionAnalytics | null): string {
  if (!analytics?.total_calls) return "0%";
  return `${((analytics.failed_calls / analytics.total_calls) * 100).toFixed(1)}%`;
}

// usageRows joins bundled-service labels to the already aggregated receipt
// response without introducing per-service requests.
function usageRows(services: AppActivityService[], analytics: AppExecutionAnalytics | null): ServiceUsageRow[] {
  const usageByService = new Map((analytics?.by_service ?? []).map((item) => [item.key, item]));
  return services.map((service) => serviceUsageRow(service, usageByService.get(service.service_id)));
}

// serviceUsageRow fills absent receipt aggregates with stable empty values for
// one bundled service.
function serviceUsageRow(service: AppActivityService, usage?: EngineExecutionBreakdown): ServiceUsageRow {
  // A missing aggregate remains a zero row so bundled services stay visible
  // before their first execution.
  const values = usage || {
    key: service.service_id,
    label: service.service_id,
    total_calls: 0,
    failed_calls: 0,
    inbound_calls: 0,
    p95_latency_ms: 0,
  };
  return {
    key: service.service_id,
    label: values.label,
    name: service.service_name || "Unnamed service",
    total_calls: values.total_calls,
    failed_calls: values.failed_calls,
    inbound_calls: values.inbound_calls,
    p95_latency_ms: values.p95_latency_ms,
  };
}

// serviceSuccessRate provides one calculation for both card and table layouts.
function serviceSuccessRate(row: ServiceUsageRow): number {
  if (row.total_calls === 0) return 0;
  return ((row.total_calls - row.failed_calls) / row.total_calls) * 100;
}

// Metric renders one bounded app statistic in the responsive summary grid.
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

// ServiceUsageCard keeps every service measure visible without horizontal
// scrolling on narrow screens.
function ServiceUsageCard({ row }: { row: ServiceUsageRow }) {
  return (
    <div className="p-4">
      <div className="min-w-0 break-words font-medium text-slate-800">{row.name}</div>
      <dl className="mt-3 grid grid-cols-2 gap-x-4 gap-y-3 text-sm">
        <div><dt className="text-xs text-slate-500">Requests</dt><dd className="mt-0.5 tabular-nums text-slate-800">{row.total_calls.toLocaleString()}</dd></div>
        <div><dt className="text-xs text-slate-500">Failed</dt><dd className={`mt-0.5 tabular-nums ${row.failed_calls > 0 ? "font-medium text-red-600" : "text-slate-400"}`}>{row.failed_calls.toLocaleString()}</dd></div>
        <div><dt className="text-xs text-slate-500">P95 latency</dt><dd className="mt-0.5 tabular-nums text-slate-700">{formatLatency(row.p95_latency_ms)}</dd></div>
        <div><dt className="text-xs text-slate-500">Success rate</dt><dd className="mt-0.5 tabular-nums text-slate-700">{serviceSuccessRate(row).toFixed(1)}%</dd></div>
      </dl>
    </div>
  );
}

// ServiceUsageTable switches presentation at the responsive breakpoint while
// retaining the same aggregated rows and labels.
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
        <>
        <div className="divide-y divide-slate-100 md:hidden">
          {rows.map((row) => <ServiceUsageCard key={row.key} row={row} />)}
        </div>
        <div className="hidden overflow-x-auto md:block">
          <table className="w-full min-w-[640px] text-left text-sm">
            <thead className="bg-slate-50 text-xs uppercase tracking-wide text-slate-500">
              <tr><th className="px-5 py-3">Service</th><th className="px-5 py-3 text-right">Requests</th><th className="px-5 py-3 text-right">Failed</th><th className="px-5 py-3 text-right">P95 latency</th><th className="px-5 py-3 text-right">Success rate</th></tr>
            </thead>
            <tbody className="divide-y divide-slate-100">
              {rows.map((row) => {
                const successRate = serviceSuccessRate(row);
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
        </>
      )}
    </div>
  );
}

// AppActivityOverview loads one app-scoped aggregate and renders its responsive
// metrics and service usage views.
export function AppActivityOverview({ appId, downloads, pendingDriftCount, services }: AppActivityOverviewProps) {
	const [analytics, setAnalytics] = useState<AppExecutionAnalytics | null>(null);
	const [issue, setIssue] = useState<AppActivityIssue | null>(null);
	const [includeAllVersions, setIncludeAllVersions] = useState(false);

  useEffect(() => {
    setIssue(null);
	// SDK activity includes generated-client and direct REST ingress because
	// both receipts carry the same immutable app-family identity.
	api.workspace.getAppExecutionAnalytics({ appId, includeAllVersions })
      .then(setAnalytics)
      .catch((cause) => setIssue(appActivityIssue(cause, "sdk")));
	}, [appId, includeAllVersions]);

  const rows = useMemo(() => usageRows(services, analytics), [analytics, services]);
  const requestValue = analytics ? analytics.total_calls.toLocaleString() : "—";
  const failedValue = analytics ? analytics.failed_calls.toLocaleString() : "—";
  const latencyValue = analytics ? formatLatency(analytics.average_latency_ms) : "—";

	return (
		<div className="space-y-6">
			<div className="flex justify-end">
				<label className="flex items-center gap-2 text-sm text-slate-600">
					<span>Version</span>
					<select
						value={includeAllVersions ? "all" : "current"}
						onChange={(event) => setIncludeAllVersions(event.target.value === "all")}
						className="rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm text-slate-700 shadow-sm"
					>
						<option value="current">This version</option>
						<option value="all">All versions</option>
					</select>
				</label>
			</div>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
        <Metric label="Requests" value={requestValue} detail="Executed with this app" icon={Activity} />
        <Metric label="Failed requests" value={failedValue} detail={`${failureRate(analytics)} failure rate`} icon={ServerCrash} />
        <Metric label="Average latency" value={latencyValue} icon={Clock3} />
        <Metric label="Total downloads" value={formatAppDownloadCount(downloads)} icon={Download} />
        <Metric label="Connected services" value={services.length.toLocaleString()} icon={FileCode} />
        <Metric label="Pending drift" value={pendingDriftCount.toLocaleString()} icon={AlertTriangle} />
      </div>
      {issue ? (
        <div className={`rounded-lg border px-4 py-3 text-sm ${issue.tone === "neutral" ? "border-slate-200 bg-slate-50 text-slate-600" : "border-red-200 bg-red-50 text-red-700"}`}>
          {issue.message}
        </div>
      ) : null}
      <ServiceUsageTable rows={rows} />
    </div>
  );
}
