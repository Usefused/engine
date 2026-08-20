import { useEffect, useRef, useState } from "react";
import { Bell } from "lucide-react";
import { Link } from "@remix-run/react";
import { useWorkspaceNotifications } from "./useWorkspaceNotifications";
import { NotificationList } from "./NotificationList";

// General notification surface mounted once in the shared layout. Badge = pending
// count; panel lists pending+acknowledged (dismissed items are hidden and
// gone for good, per the two-tier read/dismiss model). The panel itself is
// a preview -- capped to PANEL_LIMIT rows so it doesn't grow into an
// unusably tall dropdown; "View all" links to the full /integrations/
// Activity's notifications tab for anything past that.
const PANEL_LIMIT = 6;

export function NotificationBell() {
  const [open, setOpen] = useState(false);
  const panelRef = useRef<HTMLDivElement>(null);
  const { unresolved, pendingCount, serviceRefs, markRead, dismiss, canRead, canUpdate } = useWorkspaceNotifications();

  useEffect(() => {
    if (!open || !canRead) return;
    const handleClickOutside = (e: MouseEvent) => {
      if (panelRef.current && !panelRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    document.addEventListener("mousedown", handleClickOutside);
    return () => document.removeEventListener("mousedown", handleClickOutside);
  }, [canRead, open]);

  const previewItems = unresolved.slice(0, PANEL_LIMIT);
  const hasMore = unresolved.length > PANEL_LIMIT;

  // The bell is a notification read surface, so hiding it also prevents an
  // unauthorized actor from discovering a tab they cannot query.
  if (!canRead) return null;

  return (
    <div ref={panelRef} className="relative z-20">
      <button
        data-track="toggle_notification_bell"
        type="button"
        onClick={() => setOpen((prev) => !prev)}
        className="relative flex items-center justify-center w-8 h-8 rounded-lg select-none
          border border-slate-200/50 backdrop-blur-md bg-white/75 shadow-sm hover:shadow hover:bg-white/85
          transition-shadow duration-200 cursor-pointer"
        aria-label="Notifications"
      >
        <Bell className="w-3.5 h-3.5 text-slate-600" />
        {pendingCount > 0 && (
          <span className="absolute -top-1 -right-1 flex items-center justify-center min-w-[16px] h-4 px-1 rounded-full bg-red-500 text-white text-[10px] font-bold leading-none">
            {pendingCount > 9 ? "9+" : pendingCount}
          </span>
        )}
      </button>

      {open && (
        <div className="absolute right-0 mt-2 w-[min(420px,calc(100vw-2rem))] max-h-[75vh] flex flex-col rounded-xl border border-slate-200 bg-white shadow-2xl overflow-hidden">
          <div className="px-4 py-3 border-b border-slate-100 flex items-center justify-between shrink-0">
            <span className="text-base font-semibold text-slate-900">Notifications</span>
            {pendingCount > 0 && (
              <span className="text-xs font-medium text-slate-500">{pendingCount} unread</span>
            )}
          </div>
          {/* Long config keys/messages must wrap inside the fixed-width panel. */}
          <div className="min-w-0 overflow-x-hidden overflow-y-auto">
            <NotificationList
              items={previewItems}
              serviceRefs={serviceRefs}
              onMarkRead={markRead}
              onDismiss={dismiss}
              canUpdate={canUpdate}
            />
          </div>
          <Link
            to="/integrations/activity?tab=notifications"
            onClick={() => setOpen(false)}
            className="shrink-0 block text-center px-4 py-3 text-sm font-medium text-blue-600 hover:bg-slate-50 border-t border-slate-100 transition-colors"
          >
            {hasMore ? `View all ${unresolved.length} notifications` : "View all notifications"}
          </Link>
        </div>
      )}
    </div>
  );
}
