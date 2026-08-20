import { useMemo, useState } from "react";
import { ChevronLeft, ChevronRight, Loader2, Search } from "lucide-react";
import { usePaginatedWorkspaceNotifications } from "~/components/notifications/usePaginatedWorkspaceNotifications";
import { NotificationList } from "~/components/notifications/NotificationList";

type SeverityFilter = "all" | "breaking" | "non-breaking";

// matchesNotificationSearch searches only fields already present on the current page.
function matchesNotificationSearch(
  item: { message?: string; version?: string; config_key?: string; service_id?: string },
  serviceRefs: Record<string, { name?: string } | undefined>,
  query: string
): boolean {
  const serviceName = serviceRefs[item.service_id || ""]?.name || "";
  const haystacks = [item.message, item.version, item.config_key, item.service_id, serviceName];
  return haystacks.some((value) => (value || "").toLowerCase().includes(query));
}

// notificationsEmptyMessage keeps each filter state explicit when its current page is empty.
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

// NotificationsPagination keeps mobile controls bounded while desktop retains direct page access.
function NotificationsPagination({ page, totalPages, totalCount, setPage }: NotificationsPaginationProps) {
  if (totalPages <= 1) return null;
  return (
    <div className="flex flex-col gap-3 border-t border-slate-100 px-4 py-3 text-xs text-slate-500 min-[380px]:flex-row min-[380px]:items-center min-[380px]:justify-between dark:border-slate-800">
      <span>
        Page {page} of {totalPages} · {totalCount} total
      </span>
      <div className="flex w-full items-center justify-between gap-1 min-[380px]:w-auto min-[380px]:justify-start">
        <button
          type="button"
          data-track="notifications_prev_page"
          onClick={() => setPage(Math.max(1, page - 1))}
          disabled={page <= 1}
          className="flex h-9 w-9 items-center justify-center rounded-md text-slate-500 hover:bg-slate-100 dark:hover:bg-slate-800 disabled:opacity-40"
          aria-label="Previous page"
        >
          <ChevronLeft className="h-4 w-4" />
        </button>
        <div className="hidden items-center gap-1 sm:flex">
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
        </div>
        <button
          type="button"
          data-track="notifications_next_page"
          onClick={() => setPage(Math.min(totalPages, page + 1))}
          disabled={page >= totalPages}
          className="flex h-9 w-9 items-center justify-center rounded-md text-slate-500 hover:bg-slate-100 dark:hover:bg-slate-800 disabled:opacity-40"
          aria-label="Next page"
        >
          <ChevronRight className="h-4 w-4" />
        </button>
      </div>
    </div>
  );
}

// notificationsFooterLabel summarizes the backend-filtered notification count.
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
    <div className="min-w-0 space-y-5 sm:space-y-6">
      <div className="flex min-w-0 flex-col gap-3 md:flex-row md:items-center md:justify-between">
        <div className="grid w-full grid-cols-3 gap-1 rounded-lg bg-slate-100/80 p-1 md:flex md:w-fit">
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
              className={`min-w-0 truncate rounded-md px-1.5 py-2 text-[11px] font-medium transition-all cursor-pointer sm:px-3 sm:py-1.5 sm:text-sm ${
                filter === value ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700"
              }`}
            >
              {label}
            </button>
          ))}
        </div>

        <div className="flex w-full min-w-0 flex-col gap-3 md:w-auto md:flex-row md:items-center md:gap-4">
          <div className="relative w-full md:w-64">
            <Search className="w-4 h-4 absolute left-2.5 top-2.5 text-slate-400" />
            <input
              type="text"
              placeholder="Filter notifications..."
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              className="w-full rounded-lg border border-slate-200 py-2 pl-9 pr-3 text-sm focus:border-transparent focus:outline-none focus:ring-2 focus:ring-blue-500 md:py-1.5"
            />
          </div>
          <div className="grid w-full grid-cols-3 rounded-lg border border-slate-200 bg-slate-100 p-0.5 md:flex md:w-auto">
            <button
              onClick={() => setReadFilter("unread")}
              className={`rounded-md px-3 py-2 text-sm font-medium transition-colors md:py-1 ${
                readFilter === "unread" ? "bg-white text-slate-900 shadow-sm" : "text-slate-600 hover:text-slate-900"
              }`}
            >
              Unread
            </button>
            <button
              onClick={() => setReadFilter("read")}
              className={`rounded-md px-3 py-2 text-sm font-medium transition-colors md:py-1 ${
                readFilter === "read" ? "bg-white text-slate-900 shadow-sm" : "text-slate-600 hover:text-slate-900"
              }`}
            >
              Read
            </button>
            <button
              onClick={() => setReadFilter("all")}
              className={`rounded-md px-3 py-2 text-sm font-medium transition-colors md:py-1 ${
                readFilter === "all" ? "bg-white text-slate-900 shadow-sm" : "text-slate-600 hover:text-slate-900"
              }`}
            >
              All
            </button>
          </div>
        </div>
      </div>

      <div className="overflow-hidden rounded-xl border border-slate-200 bg-white">
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

      <p className="px-2 text-center text-xs leading-5 text-slate-400">
        {notificationsFooterLabel(readFilter, totalCount)}. Dismissing is
        permanent -- there's no undo. Marking one read just de-emphasizes it here; either action also stops it
        from appearing on <code className="text-slate-500">fused-cli plan</code>/
        <code className="text-slate-500">apply</code> output going forward.
      </p>
    </div>
  );
}
