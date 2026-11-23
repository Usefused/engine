import { useCallback, useEffect, useState } from "react";
import { api, WorkspaceNotification } from "~/lib/api";
import { isPending, isUnresolved } from "./notificationHelpers";
import { useNotificationServiceRefs, useNotificationStatusUpdater } from "./useNotificationShared";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";
import { canReadWorkspaceNotifications } from "~/lib/activity-access";

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
  const { access, loading: accessLoading, failed: accessFailed } = useCurrentActorAccess();
  const canRead = canReadWorkspaceNotifications(access);
  const canUpdate = hasWorkspacePermission(access, "notification.update");
  // Waiting for the complete snapshot avoids a speculative request that the
  // Engine would correctly deny and prevents notification UI from flashing.
  const shouldLoad = enabled && !accessLoading && !accessFailed && canRead;
  const [items, setItems] = useState<WorkspaceNotification[]>([]);
  const { serviceRefs, loadServiceRefs } = useNotificationServiceRefs();
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(() => {
    // Do not issue workspace notification reads before authorization completes.
    if (!shouldLoad) return Promise.resolve();
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
  }, [shouldLoad, loadServiceRefs]);

  useEffect(() => {
    // Load notifications only after read authorization succeeds.
    if (shouldLoad) refresh();
  }, [shouldLoad, refresh]);

  const { markRead, dismiss } = useNotificationStatusUpdater(canUpdate, setItems, refresh, false);

  const unresolved = items.filter(isUnresolved);
  const pendingCount = items.filter(isPending).length;

  return { items, unresolved, pendingCount, serviceRefs, loading, error, refresh, markRead, dismiss, canRead, canUpdate };
}
