import { useCallback, useEffect, useState } from "react";
import { api, NotificationServiceRef, WorkspaceNotification } from "~/lib/api";
import { isPending, isUnresolved } from "./notificationHelpers";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";

// useWorkspaceNotifications is the single data-fetching hook backing both the
// bell panel and the contextual per-page banners, so they never drift out of
// sync (same list, same optimistic-update behavior). Read-side is a plain
// GraphQL query (workspaceNotifications) that already existed before Phase 4;
// mark-read/dismiss call the new updateWorkspaceNotificationStatus mutation.
//
// serviceRefs additionally resolves every distinct service_id in the loaded
// items into a real {name, slug} via one batched servicesByIds call, so rows
// can show a readable title + a working link instead of a bare UUID. Fetched
// as a second pass after items load (not joined server-side) -- the simplest
// fix for a real bug: the bell panel was rendering raw service_id UUIDs as
// the row's title with no way to tell what was actually affected.
export function useWorkspaceNotifications(enabled = true) {
  const { access } = useCurrentActorAccess();
  const canUpdate = hasWorkspacePermission(access, "notification.update");
  const [items, setItems] = useState<WorkspaceNotification[]>([]);
  const [serviceRefs, setServiceRefs] = useState<Record<string, NotificationServiceRef>>({});
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
		if (!enabled) return Promise.resolve();
		setLoading(true);
    setError(null);
    return api.workspace
      .listNotifications()
      .then((inbox) => {
        const loaded = inbox.items || [];
        setItems(loaded);
        loadServiceRefs(loaded);
      })
      .catch((err) => {
        console.error("Failed to load workspace notifications:", err);
        setError("Failed to load notifications");
      })
      .finally(() => setLoading(false));
  }, [enabled, loadServiceRefs]);

  useEffect(() => {
    if (enabled) refresh();
  }, [enabled, refresh]);

  // Optimistic update: apply the new status locally immediately, then
  // reconcile with the server. If the server rejects it (e.g. someone else
  // already dismissed it), refresh() re-syncs to the true state.
  const updateStatus = useCallback(
    (id: string, status: "acknowledged" | "dismissed") => {
      // UI actions fail closed while access is unavailable; the Engine remains
      // authoritative if a caller bypasses this presentation layer.
      if (!canUpdate) return Promise.reject(new Error("Notification update access is not available"));
      setItems((prev) =>
        prev.map((item) => (item.id === id ? { ...item, status } : item))
      );
      return api.workspace.updateNotificationStatus(id, status).catch((err) => {
        console.error(`Failed to mark notification ${status}:`, err);
        refresh();
        throw err;
      });
    },
    [canUpdate, refresh]
  );

  const markRead = useCallback((id: string) => updateStatus(id, "acknowledged"), [updateStatus]);
  const dismiss = useCallback((id: string) => updateStatus(id, "dismissed"), [updateStatus]);

  const unresolved = items.filter(isUnresolved);
  const pendingCount = items.filter(isPending).length;

  return { items, unresolved, pendingCount, serviceRefs, loading, error, refresh, markRead, dismiss, canUpdate };
}
