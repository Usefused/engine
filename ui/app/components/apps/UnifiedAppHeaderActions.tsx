import { Pencil, Upload } from "lucide-react";
import { templatePublishPath } from "~/lib/unified-template";
import { AppHeaderActionsMenu, type AppHeaderActionLink } from "./AppHeaderActionsMenu";

/** Composes Unified App edit, template publication, and permission-gated deletion in the shared overflow menu. */
export function UnifiedAppHeaderActions({
  appID,
  canPublishTemplate,
  canDelete = false,
  deleting = false,
  onDelete,
}: {
  appID: string;
  canPublishTemplate: boolean;
  canDelete?: boolean;
  deleting?: boolean;
  onDelete?: () => void;
}) {
  const links: AppHeaderActionLink[] = [
    { label: "Edit app", to: `/integrations/unified-apps/new?edit=${encodeURIComponent(appID)}`, icon: Pencil },
  ];
  // Template publication requires catalogue management in addition to family management.
  if (canPublishTemplate) {
    links.push({ label: "Publish as template", to: templatePublishPath(appID), icon: Upload });
  }
  return <AppHeaderActionsMenu links={links} canDelete={canDelete} deleting={deleting} onDelete={onDelete} />;
}
