import { useEffect, useState } from "react";
import { useSearchParams, type MetaFunction } from "@remix-run/react";
import { Loader2 } from "lucide-react";
import IntegrationsAnalyticsTab, { type WorkspaceActivityRange } from "~/components/IntegrationsAnalyticsTab";
import { api, type WorkspaceExecutionAnalytics } from "~/lib/api";
import { NotificationsContent } from "~/components/notifications/NotificationsContent";
import { AccessDenied, useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { selectedWorkspaceActivityTab, workspaceActivityTabs, type WorkspaceActivityTab } from "~/lib/activity-access";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((match) => match.id === "root").flatMap((match) => match.meta ?? []);
  return [...parentMeta.filter((item) => !("title" in item)), { title: "Activity - Fused" }];
};

// WorkspaceOverview requests one SQL-backed aggregate projection for the selected window.
function WorkspaceOverview() {
  const [analytics, setAnalytics] = useState<WorkspaceExecutionAnalytics | null>(null);
  const [range, setRange] = useState<WorkspaceActivityRange>("7d");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  useEffect(() => {
    let active = true;
    setLoading(true);
    setError("");
    api.workspace.getWorkspaceExecutionAnalytics(workspaceActivityRange(range))
      .then((summary) => { if (active) setAnalytics(summary); })
      .catch((cause) => { if (active) setError(cause instanceof Error ? cause.message : "Failed to load workspace activity"); })
      .finally(() => { if (active) setLoading(false); });
    // A stale preset response must not overwrite a newer user selection.
    return () => { active = false; };
  }, [range]);

  if (loading) {
    return <div className="flex items-center justify-center gap-2 py-16 text-sm text-slate-500"><Loader2 className="h-5 w-5 animate-spin" /> Loading activity...</div>;
  }
  if (error) {
    return <div className="rounded-xl border border-slate-200 bg-white p-10 text-center text-sm text-red-600">{error}</div>;
  }
  if (!analytics) return null;
  return <IntegrationsAnalyticsTab analytics={analytics} range={range} onRangeChange={setRange} />;
}

// workspaceActivityRange converts each fixed reporting preset into exact ISO
// bounds so the Engine can aggregate in PostgreSQL without browser filtering.
function workspaceActivityRange(range: WorkspaceActivityRange) {
  const endDate = new Date();
  const durationHours = range === "24h" ? 24 : Number.parseInt(range, 10) * 24;
  const startDate = new Date(endDate.getTime() - durationHours * 60 * 60 * 1000);
  return { startDate: startDate.toISOString(), endDate: endDate.toISOString() };
}

// ActivityPage keeps workspace health and action items on one canonical route.
export default function ActivityPage() {
  const { access, loading, failed } = useCurrentActorAccess();
  const [searchParams, setSearchParams] = useSearchParams();
  const tabs = workspaceActivityTabs(access);
  const tab = selectedWorkspaceActivityTab(searchParams.get("tab"), tabs);

  const setTab = (next: WorkspaceActivityTab) => {
    const params = new URLSearchParams(searchParams);
    if (next === "overview") params.delete("tab");
    else params.set("tab", next);
    setSearchParams(params, { replace: true });
  };

  if (loading) {
    return <p role="status" className="rounded-xl border border-slate-200 bg-white p-6 text-sm text-slate-600">Checking your workspace access…</p>;
  }
  if (failed) {
    return <p role="alert" className="rounded-xl border border-slate-200 bg-white p-6 text-sm text-slate-600">We couldn't check your workspace access. Refresh the page and try again.</p>;
  }
  if (!tab) return <AccessDenied area="workspace activity or notifications" />;

  return (
    <div className="min-w-0 space-y-5 sm:space-y-6">
      <div>
        <h1 className="text-xl font-semibold text-slate-900">Activity</h1>
        <p className="mt-1 text-sm text-slate-500">Workspace execution health and changes that need attention.</p>
      </div>
      <div className="grid w-full grid-flow-col auto-cols-fr rounded-lg bg-slate-100 p-1 sm:inline-grid sm:w-auto">
        {tabs.map((value) => (
          <button
            key={value}
            type="button"
            data-track={`view_activity_${value}_tab`}
            onClick={() => setTab(value)}
            className={`min-w-0 rounded-md px-3 py-2 text-sm font-medium capitalize sm:px-4 sm:py-1.5 ${tab === value ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700"}`}
          >
            {value}
          </button>
        ))}
      </div>
      {tab === "overview" ? <WorkspaceOverview /> : <NotificationsContent />}
    </div>
  );
}
