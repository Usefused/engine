import { useEffect, useState } from "react";
import { useSearchParams, type MetaFunction } from "@remix-run/react";
import { Loader2 } from "lucide-react";
import IntegrationsAnalyticsTab from "~/components/IntegrationsAnalyticsTab";
import { api, type WorkspaceExecutionAnalytics } from "~/lib/api";
import { NotificationsContent } from "~/components/notifications/NotificationsContent";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((match) => match.id === "root").flatMap((match) => match.meta ?? []);
  return [...parentMeta.filter((item) => !("title" in item)), { title: "Activity - Fused" }];
};

type ActivityTab = "overview" | "notifications";

function activityTab(value: string | null): ActivityTab {
  return value === "notifications" ? "notifications" : "overview";
}

function WorkspaceOverview() {
  const [analytics, setAnalytics] = useState<WorkspaceExecutionAnalytics | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  useEffect(() => {
    api.workspace.getWorkspaceExecutionAnalytics()
      .then(setAnalytics)
      .catch((cause) => setError(cause instanceof Error ? cause.message : "Failed to load workspace activity"))
      .finally(() => setLoading(false));
  }, []);

  if (loading) {
    return <div className="flex items-center justify-center gap-2 py-16 text-sm text-slate-500"><Loader2 className="h-5 w-5 animate-spin" /> Loading activity...</div>;
  }
  if (error) {
    return <div className="rounded-xl border border-slate-200 bg-white p-10 text-center text-sm text-red-600">{error}</div>;
  }
  if (!analytics || analytics.total_calls === 0) {
    return <div className="rounded-xl border border-slate-200 bg-white p-10 text-center text-sm text-slate-500">No calls in the last 7 days. SDK, MCP server, and webhook activity will appear here.</div>;
  }
  return <IntegrationsAnalyticsTab analytics={analytics} />;
}

export default function ActivityPage() {
  const [searchParams, setSearchParams] = useSearchParams();
  const tab = activityTab(searchParams.get("tab"));

  const setTab = (next: ActivityTab) => {
    const params = new URLSearchParams(searchParams);
    if (next === "overview") params.delete("tab");
    else params.set("tab", next);
    setSearchParams(params, { replace: true });
  };

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-xl font-semibold text-slate-900">Activity</h1>
        <p className="mt-1 text-sm text-slate-500">Workspace execution health and changes that need attention.</p>
      </div>
      <div className="flex w-fit rounded-lg bg-slate-100 p-1">
        {(["overview", "notifications"] as ActivityTab[]).map((value) => (
          <button
            key={value}
            type="button"
            data-track={`view_activity_${value}_tab`}
            onClick={() => setTab(value)}
            className={`rounded-md px-4 py-1.5 text-sm font-medium capitalize ${tab === value ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700"}`}
          >
            {value}
          </button>
        ))}
      </div>
      {tab === "overview" ? <WorkspaceOverview /> : <NotificationsContent />}
    </div>
  );
}
