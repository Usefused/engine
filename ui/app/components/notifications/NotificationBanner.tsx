import { AlertTriangle, ChevronDown } from "lucide-react";
import { useId, useState } from "react";
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
  const [expanded, setExpanded] = useState(false);
  const contentID = useId();
  // Empty contextual panels disappear instead of leaving a disclosure with no content.
  if (items.length === 0) return null;

  const severity = worstSeverity(items);
  const isBreaking = severity === "breaking";
  // The summary keeps the strongest pending severity visible while details remain collapsed.
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
  const copy = notificationBannerCopy(items.length, isBreaking);

  return (
    <div className={`overflow-hidden rounded-lg border ${colors.border} ${colors.bg}`}>
      <button type="button" data-track="toggle_contextual_notifications" aria-expanded={expanded} aria-controls={contentID} onClick={() => setExpanded((value) => !value)} className="flex w-full cursor-pointer items-start gap-3 px-4 py-3 text-left focus:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-blue-500">
        <AlertTriangle className={`w-4 h-4 mt-0.5 shrink-0 ${colors.icon}`} />
        <span className="min-w-0 flex-1">
          <span className={`block text-sm font-medium ${colors.title}`}>{copy.title}</span>
          <span className={`mt-1 block text-sm ${colors.subtitle}`}>{copy.subtitle}</span>
        </span>
        <ChevronDown className={`mt-0.5 h-4 w-4 shrink-0 transition-transform ${expanded ? "rotate-180" : ""} ${colors.icon}`} aria-hidden="true" />
      </button>
      {/* Conditional mounting keeps collapsed row links and actions outside the keyboard focus order. */}
      {expanded ? (
        <div id={contentID} className={`mx-4 mb-3 divide-y border-t ${isBreaking ? "divide-red-200/70 border-red-200/70" : "divide-blue-200/70 border-blue-200/70"}`}>
          <NotificationList items={items} serviceRefs={serviceRefs} onMarkRead={onMarkRead} onDismiss={onDismiss} canUpdate={canUpdate} dense />
        </div>
      ) : null}
    </div>
  );
}

// notificationBannerCopy keeps count grammar and severity guidance identical on every contextual page.
function notificationBannerCopy(count: number, isBreaking: boolean): { title: string; subtitle: string } {
  const plural = count === 1 ? "" : "s";
  // Breaking changes remain explicit even before the user expands their details.
  if (isBreaking) {
    return {
      title: `${count} breaking change notification${plural}`,
      subtitle: "These changes may require action before your next deploy.",
    };
  }
  return {
    title: `${count} notification${plural}`,
    subtitle: "Recent changes relevant to this service.",
  };
}
