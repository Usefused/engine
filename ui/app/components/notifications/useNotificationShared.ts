import { useCallback, useState, type Dispatch, type SetStateAction } from "react";
import { api, type NotificationServiceRef, type WorkspaceNotification } from "~/lib/api";

/** Resolves distinct service_id values from loaded notifications into human-readable names and slugs. */
export function useNotificationServiceRefs() {
  const [serviceRefs, setServiceRefs] = useState<Record<string, NotificationServiceRef>>({});

  const loadServiceRefs = useCallback((loadedItems: WorkspaceNotification[]) => {
    const ids = Array.from(
      new Set(loadedItems.map((item) => item.service_id).filter((id): id is string => !!id))
    );
    // Avoid issuing a batched lookup when no loaded notification references a service.
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

  return { serviceRefs, loadServiceRefs };
}

/** Provides optimistic notification acknowledge/dismiss handlers with permission gating and error resync. */
export function useNotificationStatusUpdater(
  canUpdate: boolean,
  setItems: Dispatch<SetStateAction<WorkspaceNotification[]>>,
  refresh: () => Promise<void>,
  refreshOnSuccess = false
) {
  const updateStatus = useCallback(
    (id: string, status: "acknowledged" | "dismissed") => {
      // Fail closed when the actor lacks notification.update; Engine remains authoritative.
      if (!canUpdate) return Promise.reject(new Error("Notification update access is not available"));
      setItems((prev) => prev.map((item) => (item.id === id ? { ...item, status } : item)));
      const request = api.workspace.updateNotificationStatus(id, status);
      // Paginated views refresh after success so total counts and page boundaries stay accurate.
      const settled = refreshOnSuccess ? request.then(() => refresh()) : request;
      return settled.catch((err) => {
        console.error(`Failed to mark notification ${status}:`, err);
        refresh();
        throw err;
      });
    },
    [canUpdate, setItems, refresh, refreshOnSuccess]
  );

  const markRead = useCallback((id: string) => updateStatus(id, "acknowledged"), [updateStatus]);
  const dismiss = useCallback((id: string) => updateStatus(id, "dismissed"), [updateStatus]);

  return { markRead, dismiss };
}
