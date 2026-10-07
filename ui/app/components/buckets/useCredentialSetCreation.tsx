import { useState } from "react";
import { BucketCreateModal } from "./BucketCreateModal";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";
import { listAppBuildSelectors } from "~/lib/app-builder";
import { readAllBoundedPages } from "~/lib/bounded-pages";
import type { Bucket } from "~/lib/api";
import type { AppBuildSelector } from "~/lib/app-builder-contract";

/** Creates in place, then selects only a bucket authorized for the app's current owner. */
export function useCredentialSetCreation(ownerTeamId: string, onSelected: (item: AppBuildSelector) => void) {
  const { access } = useCurrentActorAccess();
  const [open, setOpen] = useState(false);
  const allowed = hasWorkspacePermission(access, "bucket.manage");
  /** Creation alone does not establish the team's access; recheck the authoritative selector. */
  async function selectCreated(_name: string, bucket: Bucket) {
    const items = await readAllBoundedPages((limit, offset) => listAppBuildSelectors(ownerTeamId, "BUCKET", bucket.name, limit, offset), 100, 100);
    const selected = items.find((item) => item.resource_id === bucket.id);
    // Never select an inaccessible bucket or silently fall back to a different one.
    if (!selected) throw new Error("Bucket created, but it is not available to this app owner. Share it with the owning team on the Bucket page, then retry selection.");
    onSelected(selected);
  }
  return {
    // Creation is a workspace action even when an actor can use existing scoped credentials.
    create: allowed ? () => setOpen(true) : undefined,
    dialog: <BucketCreateModal open={open && allowed} onClose={() => setOpen(false)} onCreated={selectCreated} />,
  };
}
