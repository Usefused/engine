import { useMemo, useState } from "react";
import { type MetaFunction } from "@remix-run/react";
import { Bell, ChevronLeft, ChevronRight, Loader2, Search } from "lucide-react";
import { usePaginatedWorkspaceNotifications } from "~/components/notifications/usePaginatedWorkspaceNotifications";
import { NotificationList } from "~/components/notifications/NotificationList";

export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((m: any) => m.id === "root").flatMap((m: any) => m.meta ?? []);
  return [
    ...parentMeta.filter((m: any) => !("title" in m)),
    { title: "Notifications - Fused" },
  ];
};

type SeverityFilter = "all" | "breaking" | "non-breaking";

// Full notifications page -- the "expand" surface the bell panel's preview
// links out to. Offset-paginated with numbered pages (see
// plans/plan-service-changelog.md's Phase 4 pagination follow-up), defaults
// to unread-only so the page opens on what needs attention rather than a
// long scroll of already-read history; the "Show read" checkbox reveals
// acknowledged rows too. Severity filtering happens client-side on top of
// whatever page is loaded, since it's a display narrowing, not a separate
// fetch dimension.
export default function NotificationsPage() {
  const {
    items,
    serviceRefs,
    page,
    setPage,
    totalPages,
    totalCount,
    pendingCount,
    readFilter,
    setReadFilter,
    loading,
    error,
    markRead,
    dismiss,
  } = usePaginatedWorkspaceNotifications();
  const [filter, setFilter] = useState<SeverityFilter>("all");
  const [search, setSearch] = useState("");

  const filtered = useMemo(() => {
    let list = items;
    // The backend now filters based on readFilter ("unread", "read", "all").
    if (filter !== "all") {
      list = list.filter((item) => item.severity === filter);
    }
    if (search.trim()) {
      const q = search.toLowerCase();
      list = list.filter((item) => 
        item.message?.toLowerCase().includes(q) ||
        item.version?.toLowerCase().includes(q) ||
        item.config_key?.toLowerCase().includes(q) ||
        item.service_id?.toLowerCase().includes(q) ||
        serviceRefs[item.service_id || ""]?.name?.toLowerCase().includes(q)
      );
    }
    return list;
  }, [items, filter, readFilter, search, serviceRefs]);

  const breakingCount = items.filter((item) => item.severity === "breaking").length;
  const nonBreakingCount = items.length - breakingCount;

  return (
    <div className="space-y-6">
      <div className="flex items-center gap-3">
        <div className="p-2 rounded-lg bg-slate-100 text-slate-600">
          <Bell className="w-5 h-5" />
        </div>
        <div>
          <h1 className="text-xl font-semibold text-slate-900">Notifications</h1>
          <p className="text-sm text-slate-500">
            {pendingCount > 0 ? `${pendingCount} unread` : "You're all caught up"}
          </p>
        </div>
      </div>

      <div className="flex items-center justify-between flex-wrap gap-3">
        <div className="flex items-center gap-1 p-1 bg-slate-100/80 rounded-lg w-fit">
          {([
            ["all", `All (${items.length})`],
            ["breaking", `Breaking (${breakingCount})`],
            ["non-breaking", `Non-breaking (${nonBreakingCount})`],
          ] as [SeverityFilter, string][]).map(([value, label]) => (
            <button
              key={value}
              data-track={`filter_notifications_${value}`}
              type="button"
              onClick={() => setFilter(value)}
              className={`px-3 py-1.5 text-sm font-medium rounded-md transition-all cursor-pointer ${
                filter === value ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700"
              }`}
            >
              {label}
            </button>
          ))}
        </div>

        <div className="flex items-center gap-4">
          <div className="relative">
            <Search className="w-4 h-4 absolute left-2.5 top-2.5 text-slate-400" />
            <input
              type="text"
              placeholder="Filter notifications..."
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              className="pl-9 pr-3 py-1.5 text-sm border border-slate-200 rounded-lg w-full sm:w-64 focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-transparent"
            />
          </div>
          <div className="flex bg-slate-100 p-0.5 rounded-lg border border-slate-200">
            <button
              onClick={() => setReadFilter("unread")}
              className={`px-3 py-1 text-sm font-medium rounded-md transition-colors ${
                readFilter === "unread" ? "bg-white text-slate-900 shadow-sm" : "text-slate-600 hover:text-slate-900"
              }`}
            >
              Unread
            </button>
            <button
              onClick={() => setReadFilter("read")}
              className={`px-3 py-1 text-sm font-medium rounded-md transition-colors ${
                readFilter === "read" ? "bg-white text-slate-900 shadow-sm" : "text-slate-600 hover:text-slate-900"
              }`}
            >
              Read
            </button>
            <button
              onClick={() => setReadFilter("all")}
              className={`px-3 py-1 text-sm font-medium rounded-md transition-colors ${
                readFilter === "all" ? "bg-white text-slate-900 shadow-sm" : "text-slate-600 hover:text-slate-900"
              }`}
            >
              All
            </button>
          </div>
        </div>
      </div>

      <div className="bg-white border-y border-slate-200 overflow-hidden">
        {loading && items.length === 0 ? (
          <div className="flex flex-col items-center justify-center py-16 text-slate-400">
            <Loader2 className="w-6 h-6 animate-spin mb-2" />
            <p className="text-sm">Loading notifications...</p>
          </div>
        ) : error ? (
          <p className="px-4 py-8 text-sm text-red-600 text-center">{error}</p>
        ) : (
          <NotificationList
            items={filtered}
            serviceRefs={serviceRefs}
            onMarkRead={markRead}
            onDismiss={dismiss}
            emptyMessage={
              filter === "all"
                ? readFilter === "unread"
                  ? "No unread notifications"
                  : readFilter === "read"
                  ? "No read notifications"
                  : "No notifications"
                : `No ${filter} notifications`
            }
          />
        )}

        {totalPages > 1 && (
          <div className="flex items-center justify-between border-t border-slate-100 dark:border-slate-800 px-4 py-3 text-xs text-slate-500">
            <span>
              Page {page} of {totalPages} · {totalCount} total
            </span>
            <div className="flex items-center gap-1">
              <button
                type="button"
                data-track="notifications_prev_page"
                onClick={() => setPage(Math.max(1, page - 1))}
                disabled={page <= 1}
                className="rounded-md p-1.5 text-slate-500 hover:bg-slate-100 dark:hover:bg-slate-800 disabled:opacity-40"
                aria-label="Previous page"
              >
                <ChevronLeft className="h-4 w-4" />
              </button>
              {Array.from({ length: totalPages }, (_, i) => i + 1).map((p) => (
                <button
                  key={p}
                  type="button"
                  data-track={`notifications_page_${p}`}
                  onClick={() => setPage(p)}
                  className={`rounded-md px-2.5 py-1 text-xs font-medium transition-colors cursor-pointer ${
                    p === page
                      ? "bg-slate-100 text-slate-900 shadow-sm"
                      : "text-slate-500 hover:text-slate-700 hover:bg-slate-50"
                  }`}
                >
                  {p}
                </button>
              ))}
              <button
                type="button"
                data-track="notifications_next_page"
                onClick={() => setPage(Math.min(totalPages, page + 1))}
                disabled={page >= totalPages}
                className="rounded-md p-1.5 text-slate-500 hover:bg-slate-100 dark:hover:bg-slate-800 disabled:opacity-40"
                aria-label="Next page"
              >
                <ChevronRight className="h-4 w-4" />
              </button>
            </div>
          </div>
        )}
      </div>

      <p className="text-xs text-slate-400 text-center">
        {totalCount} {readFilter === "all" ? "total" : readFilter} notification{totalCount === 1 ? "" : "s"}. Dismissing is
        permanent -- there's no undo. Marking one read just de-emphasizes it here; either action also stops it
        from appearing on <code className="text-slate-500">fused-cli plan</code>/
        <code className="text-slate-500">apply</code> output going forward.
      </p>
    </div>
  );
}
