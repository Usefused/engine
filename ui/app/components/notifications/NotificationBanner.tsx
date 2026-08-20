import { AlertTriangle } from "lucide-react";
import type { NotificationServiceRef, WorkspaceNotification } from "~/lib/api";
import { NotificationList } from "./NotificationList";
import { worstSeverity } from "./notificationHelpers";

// Contextual, page-scoped notification banner. Shown at the top of a
// service-details or SDK-details page, filtered to only what's relevant to
// that page (service_id, or config_key/service_id/version -- see
// notificationHelpers.matchesService / matchesConfig). Complements, not
// replaces, the general bell at the top -- see plans/plan-service-changelog.md
// Phase 4. Color follows the worst severity present: red if anything
// breaking, blue if only non-breaking (non-breaking is informational, not a
// warning -- amber read as "something's wrong" for what's often just "a new
// version exists").
export function NotificationBanner({
  items,
  serviceRefs,
  onMarkRead,
  onDismiss,
  canUpdate = false,
}: {
  items: WorkspaceNotification[];
  serviceRefs: Record<string, NotificationServiceRef>;
  onMarkRead: (id: string) => void;
  onDismiss: (id: string) => void;
  canUpdate?: boolean;
}) {
  if (items.length === 0) return null;

  const severity = worstSeverity(items);
  const isBreaking = severity === "breaking";

  const colors = isBreaking
    ? {
        border: "border-red-200",
        bg: "bg-red-50",
        icon: "text-red-600",
        title: "text-red-900",
        subtitle: "text-red-800",
      }
    : {
        border: "border-blue-200",
        bg: "bg-blue-50",
        icon: "text-blue-600",
        title: "text-blue-900",
        subtitle: "text-blue-800",
      };

  return (
    <div className={`border ${colors.border} ${colors.bg} rounded-lg px-4 py-3`}>
      <div className="flex items-start gap-3">
        <AlertTriangle className={`w-4 h-4 mt-0.5 shrink-0 ${colors.icon}`} />
        <div className="flex-1 min-w-0">
          <p className={`text-sm font-medium ${colors.title}`}>
            {isBreaking
              ? `${items.length} breaking change notification${items.length === 1 ? "" : "s"}`
              : `${items.length} notification${items.length === 1 ? "" : "s"}`}
          </p>
          <p className={`text-sm mt-1 ${colors.subtitle}`}>
            {isBreaking
              ? "These changes may require action before your next deploy."
              : "Recent changes relevant to this service."}
          </p>
        </div>
      </div>
      <div className={`mt-3 divide-y ${isBreaking ? "divide-red-200/70 border-red-200/70" : "divide-blue-200/70 border-blue-200/70"} border-t`}>
        <NotificationList items={items} serviceRefs={serviceRefs} onMarkRead={onMarkRead} onDismiss={onDismiss} canUpdate={canUpdate} dense />
      </div>
    </div>
  );
}
