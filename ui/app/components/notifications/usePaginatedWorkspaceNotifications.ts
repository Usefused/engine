import { useCallback, useEffect, useState } from "react";
import { api, WorkspaceNotification } from "~/lib/api";
import { useNotificationServiceRefs, useNotificationStatusUpdater } from "./useNotificationShared";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";
import { canReadWorkspaceNotifications } from "~/lib/activity-access";

const PAGE_SIZE = 20;

// usePaginatedWorkspaceNotifications backs Activity's Notifications tab
// page only (offset pagination, numbered pages -- see
// plans/plan-service-changelog.md's Phase 4 pagination follow-up). Defaults
// to "unread only" (pending rows) with a toggle to reveal acknowledged rows
// too, so the page opens showing what actually needs attention rather than
// burying it under a long history.
//
// useWorkspaceNotifications (the unbounded hook) is deliberately untouched
// and unrelated to this one: the bell panel and contextual banners both
// depend on its exact current unbounded behavior, and neither needs
// numbered pages.
export function usePaginatedWorkspaceNotifications() {
  const { access, loading: accessLoading, failed: accessFailed } = useCurrentActorAccess();
  const canRead = canReadWorkspaceNotifications(access);
  const canUpdate = hasWorkspacePermission(access, "notification.update");
  // The paginated surface shares the bell's fail-closed read preflight.
  const shouldLoad = !accessLoading && !accessFailed && canRead;
  const [page, setPage] = useState(1);
  const [filter, setFilter] = useState<"unread" | "read" | "all">("unread");
  const [items, setItems] = useState<WorkspaceNotification[]>([]);
  const { serviceRefs, loadServiceRefs } = useNotificationServiceRefs();
  const [totalCount, setTotalCount] = useState(0);
  const [pendingCount, setPendingCount] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(() => {
    // Do not issue paginated notification reads before authorization completes.
    if (!shouldLoad) return Promise.resolve();
    setLoading(true);
    setError(null);
    return api.workspace
      .listNotificationsPage(page, PAGE_SIZE, filter === "unread", filter === "read")
      .then((inbox) => {
        const loaded = inbox.items || [];
        setItems(loaded);
        setTotalCount(inbox.total_count || 0);
        setPendingCount(inbox.pending_count || 0);
        loadServiceRefs(loaded);
      })
      .catch((err) => {
        console.error("Failed to load workspace notifications:", err);
        setError("Failed to load notifications");
      })
      .finally(() => setLoading(false));
  }, [page, filter, loadServiceRefs, shouldLoad]);

  useEffect(() => {
    // Load the active page only after read authorization succeeds.
    if (shouldLoad) refresh();
  }, [refresh, shouldLoad]);

  const totalPages = Math.max(1, Math.ceil(totalCount / PAGE_SIZE));

  // Marking-read/dismissing can shrink the current filter's total out from
  // under the page the user is on (e.g. clearing the last unread row on
  // page 3) -- step back onto the new last page instead of showing an
  // empty page with valid pages before it.
  useEffect(() => {
    // Clamp the current page when filtering or dismissal shrinks the total page count.
    if (!loading && page > totalPages) {
      setPage(totalPages);
    }
  }, [loading, page, totalPages]);

  const { markRead, dismiss } = useNotificationStatusUpdater(canUpdate, setItems, refresh, true);

  const updateFilter = useCallback((f: "unread" | "read" | "all") => {
    setFilter(f);
    setPage(1);
  }, []);

  return {
    items,
    serviceRefs,
    page,
    setPage,
    totalPages,
    totalCount,
    pendingCount,
    readFilter: filter,
    setReadFilter: updateFilter,
    loading,
    error,
    refresh,
    markRead,
    dismiss,
    canUpdate,
    canRead,
  };
}
