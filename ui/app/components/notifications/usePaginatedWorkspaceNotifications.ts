import { useCallback, useEffect, useState } from "react";
import { api, NotificationServiceRef, WorkspaceNotification } from "~/lib/api";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";

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
  const { access } = useCurrentActorAccess();
  const canUpdate = hasWorkspacePermission(access, "notification.update");
  const [page, setPage] = useState(1);
  const [filter, setFilter] = useState<"unread" | "read" | "all">("unread");
  const [items, setItems] = useState<WorkspaceNotification[]>([]);
  const [serviceRefs, setServiceRefs] = useState<Record<string, NotificationServiceRef>>({});
  const [totalCount, setTotalCount] = useState(0);
  const [pendingCount, setPendingCount] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const loadServiceRefs = useCallback((loadedItems: WorkspaceNotification[]) => {
    const ids = Array.from(
      new Set(loadedItems.map((item) => item.service_id).filter((id): id is string => !!id))
    );
    if (ids.length === 0) return;
    api.workspace
      .listServiceRefsByIds(ids)
      .then((refs) => {
        setServiceRefs((prev) => {
          const next = { ...prev };
          for (const ref of refs) next[ref.id] = ref;
          return next;
        });
      })
      .catch((err) => {
        // Non-fatal: rows fall back to showing the bare service_id.
        console.error("Failed to resolve notification service names:", err);
      });
  }, []);

  const refresh = useCallback(() => {
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
  }, [page, filter, loadServiceRefs]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const totalPages = Math.max(1, Math.ceil(totalCount / PAGE_SIZE));

  // Marking-read/dismissing can shrink the current filter's total out from
  // under the page the user is on (e.g. clearing the last unread row on
  // page 3) -- step back onto the new last page instead of showing an
  // empty page with valid pages before it.
  useEffect(() => {
    if (!loading && page > totalPages) {
      setPage(totalPages);
    }
  }, [loading, page, totalPages]);

  const updateStatus = useCallback(
    (id: string, status: "acknowledged" | "dismissed") => {
      // Do not optimistically mutate rows when the actor lacks the mutation permission.
      if (!canUpdate) return Promise.reject(new Error("Notification update access is not available"));
      setItems((prev) => prev.map((item) => (item.id === id ? { ...item, status } : item)));
      return api.workspace
        .updateNotificationStatus(id, status)
        .then(() => refresh())
        .catch((err) => {
          console.error(`Failed to mark notification ${status}:`, err);
          refresh();
          throw err;
        });
    },
    [canUpdate, refresh]
  );

  const markRead = useCallback((id: string) => updateStatus(id, "acknowledged"), [updateStatus]);
  const dismiss = useCallback((id: string) => updateStatus(id, "dismissed"), [updateStatus]);

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
  };
}
