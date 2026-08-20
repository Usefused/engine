import { useMemo, useState } from "react";
import { ChevronLeft, ChevronRight, Loader2, Search } from "lucide-react";
import { usePaginatedWorkspaceNotifications } from "~/components/notifications/usePaginatedWorkspaceNotifications";
import { NotificationList } from "~/components/notifications/NotificationList";

type SeverityFilter = "all" | "breaking" | "non-breaking";

function matchesNotificationSearch(
  item: { message?: string; version?: string; config_key?: string; service_id?: string },
  serviceRefs: Record<string, { name?: string } | undefined>,
  query: string
): boolean {
  const serviceName = serviceRefs[item.service_id || ""]?.name || "";
  const haystacks = [item.message, item.version, item.config_key, item.service_id, serviceName];
  return haystacks.some((value) => (value || "").toLowerCase().includes(query));
}

function notificationsEmptyMessage(filter: SeverityFilter, readFilter: string): string {
  if (filter !== "all") return `No ${filter} notifications`;
  if (readFilter === "unread") return "No unread notifications";
  if (readFilter === "read") return "No read notifications";
  return "No notifications";
}

interface NotificationsBodyProps {
  loading: boolean;
  error: string | null;
  itemsLoaded: boolean;
  filtered: ReturnType<typeof usePaginatedWorkspaceNotifications>["items"];
  serviceRefs: ReturnType<typeof usePaginatedWorkspaceNotifications>["serviceRefs"];
  markRead: ReturnType<typeof usePaginatedWorkspaceNotifications>["markRead"];
  dismiss: ReturnType<typeof usePaginatedWorkspaceNotifications>["dismiss"];
  canUpdate: boolean;
  filter: SeverityFilter;
  readFilter: string;
}

/** Renders the paginated notification body and forwards update authorization. */
function NotificationsBody({ loading, error, itemsLoaded, filtered, serviceRefs, markRead, dismiss, canUpdate, filter, readFilter }: NotificationsBodyProps) {
  if (loading && !itemsLoaded) {
    return (
      <div className="flex flex-col items-center justify-center py-16 text-slate-400">
        <Loader2 className="w-6 h-6 animate-spin mb-2" />
        <p className="text-sm">Loading notifications...</p>
      </div>
    );
  }
  if (error) {
    return <p className="px-4 py-8 text-sm text-red-600 text-center">{error}</p>;
  }
  return (
    <NotificationList
      items={filtered}
      serviceRefs={serviceRefs}
      onMarkRead={markRead}
      onDismiss={dismiss}
      emptyMessage={notificationsEmptyMessage(filter, readFilter)}
      canUpdate={canUpdate}
    />
  );
}

interface NotificationsPaginationProps {
  page: number;
  totalPages: number;
  totalCount: number;
  setPage: (page: number) => void;
}

function NotificationsPagination({ page, totalPages, totalCount, setPage }: NotificationsPaginationProps) {
  if (totalPages <= 1) return null;
  return (
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
  );
}

function notificationsFooterLabel(readFilter: string, totalCount: number): string {
  const kind = readFilter === "all" ? "total" : readFilter;
  const plural = totalCount === 1 ? "" : "s";
  return `${totalCount} ${kind} notification${plural}`;
}

// Activity owns the full notification surface behind its Notifications tab.
// It is offset-paginated with numbered pages (see
// plans/plan-service-changelog.md's Phase 4 pagination follow-up), defaults
// to unread-only so the page opens on what needs attention rather than a
// long scroll of already-read history; the "Show read" checkbox reveals
// acknowledged rows too. Severity filtering happens client-side on top of
// whatever page is loaded, since it's a display narrowing, not a separate
// fetch dimension.
export function NotificationsContent() {
  const {
    items,
    serviceRefs,
    page,
    setPage,
    totalPages,
    totalCount,
    readFilter,
    setReadFilter,
    loading,
    error,
    markRead,
    dismiss,
    canUpdate,
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
      list = list.filter((item) => matchesNotificationSearch(item, serviceRefs, q));
    }
    return list;
  }, [items, filter, readFilter, search, serviceRefs]);

  const breakingCount = items.filter((item) => item.severity === "breaking").length;
  const nonBreakingCount = items.length - breakingCount;

  return (
    <div className="space-y-6">
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
        <NotificationsBody
          loading={loading}
          error={error}
          itemsLoaded={items.length > 0}
          filtered={filtered}
          serviceRefs={serviceRefs}
          markRead={markRead}
          dismiss={dismiss}
          canUpdate={canUpdate}
          filter={filter}
          readFilter={readFilter}
        />

        <NotificationsPagination page={page} totalPages={totalPages} totalCount={totalCount} setPage={setPage} />
      </div>

      <p className="text-xs text-slate-400 text-center">
        {notificationsFooterLabel(readFilter, totalCount)}. Dismissing is
        permanent -- there's no undo. Marking one read just de-emphasizes it here; either action also stops it
        from appearing on <code className="text-slate-500">fused-cli plan</code>/
        <code className="text-slate-500">apply</code> output going forward.
      </p>
    </div>
  );
}
