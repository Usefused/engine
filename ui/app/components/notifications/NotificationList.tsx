import { AlertTriangle, Check, Info, X, ChevronDown } from "lucide-react";
import { useState } from "react";
import { useToast } from "~/components/Toast";
import { Link } from "@remix-run/react";
import type { NotificationServiceRef, WorkspaceNotification } from "~/lib/api";
import { notificationLink, notificationTarget, notificationTitle, notificationTypeLabel } from "./notificationHelpers";

// Shared presentational row used by the bell panel, the full notifications
// page, and the contextual banners, so read/dismiss/title/link logic can't
// drift apart between surfaces. Dismiss is terminal (no "undismiss") -- see
// plans/plan-service-changelog.md's Phase 4 two-tier read/dismiss model.
//
// serviceRefs resolves service_id -> {name, slug} (see
// useWorkspaceNotifications' loadServiceRefs) so the title is a real name,
// not a bare UUID, and the whole row links to the affected service's page.
// Pass {} if a caller genuinely has no resolved refs yet -- the title/link
// helpers degrade gracefully to the service_id itself.
export function NotificationRow({
  item,
  serviceRefs,
  onMarkRead,
  onDismiss,
  dense = false,
}: {
  item: WorkspaceNotification;
  serviceRefs: Record<string, NotificationServiceRef>;
  onMarkRead: (id: string) => void;
  onDismiss: (id: string) => void;
  dense?: boolean;
}) {
  const isBreaking = item.severity === "breaking";
  const isAcknowledged = item.status === "acknowledged";
  const link = notificationLink(item, serviceRefs);
  const [dropdownOpen, setDropdownOpen] = useState(false);
  const toast = useToast();

  const copyTag = (e: React.MouseEvent, text: string) => {
    e.preventDefault();
    e.stopPropagation();
    navigator.clipboard.writeText(text);
    toast.success("Copied to clipboard");
  };

  const accentBorder = isBreaking ? "border-l-red-400" : "border-l-blue-400";
  const iconWrap = isBreaking
    ? "bg-red-50 text-red-600"
    : "bg-blue-50 text-blue-600";

  return (
    <div
      className={`flex items-start gap-3 border-l-4 ${accentBorder} border-b border-slate-100 last:border-b-0 ${
        dense ? "px-3 py-3" : "px-4 py-4"
      } ${isAcknowledged ? "opacity-80" : ""}`}
    >
      <div className={`shrink-0 rounded-lg p-1.5 mt-0.5 ${iconWrap}`}>
        {isBreaking ? <AlertTriangle className="w-4 h-4" /> : <Info className="w-4 h-4" />}
      </div>
      <div className="flex-1 min-w-0">
        <div className="flex items-center gap-2 flex-wrap">
          <span
            className={`text-[10px] font-semibold uppercase tracking-wide px-1.5 py-0.5 rounded ${
              isBreaking ? "bg-red-100 text-red-700" : "bg-blue-100 text-blue-700"
            }`}
          >
            {notificationTypeLabel(item)}
          </span>
        </div>
        {link ? (
          <Link
            to={link}
            className={`block mt-1 font-medium text-slate-700 hover:underline ${
              dense ? "text-xs" : "text-sm"
            }`}
            title={item.message}
          >
            {item.message}
          </Link>
        ) : (
          <p
            className={`mt-1 font-medium text-slate-700 ${
              dense ? "text-xs" : "text-sm"
            }`}
            title={item.message}
          >
            {item.message}
          </p>
        )}
        
        {(item.service_id || item.version || item.config_key) && (
          <div className="mt-2 flex items-center gap-2 flex-wrap">
            {item.service_id && (
              <button
                type="button"
                onClick={(e) => copyTag(e, serviceRefs[item.service_id!]?.name || item.service_id!)}
                className="text-[10px] font-medium bg-slate-50 text-slate-600 px-1.5 py-0.5 rounded border border-slate-200 truncate max-w-[150px] hover:bg-slate-100 transition-colors cursor-pointer"
                title="Click to copy"
              >
                {serviceRefs[item.service_id!]?.name || item.service_id}
              </button>
            )}
            {item.version && (
              <button
                type="button"
                onClick={(e) => copyTag(e, item.version!)}
                className="text-[10px] font-medium bg-slate-50 text-slate-600 px-1.5 py-0.5 rounded border border-slate-200 hover:bg-slate-100 transition-colors cursor-pointer"
                title="Click to copy"
              >
                {item.version}
              </button>
            )}
            {item.config_key && (
              <button
                type="button"
                onClick={(e) => copyTag(e, item.config_key!)}
                className="text-[10px] font-medium bg-slate-50 text-slate-600 px-1.5 py-0.5 rounded border border-slate-200 hover:bg-slate-100 transition-colors cursor-pointer"
                title="Click to copy"
              >
                {item.config_key}
              </button>
            )}
          </div>
        )}
      </div>
      <div className="flex items-center gap-2 shrink-0">
        <div 
          className="relative"
          onBlur={(e) => {
            if (!e.currentTarget.contains(e.relatedTarget as Node)) {
              setDropdownOpen(false);
            }
          }}
        >
          <button
            type="button"
            onClick={(e) => {
              e.preventDefault();
              setDropdownOpen(!dropdownOpen);
            }}
            className="px-2 py-1 rounded border border-slate-200 bg-white text-slate-500 hover:text-slate-700 hover:bg-slate-50 transition-all text-xs flex items-center gap-1 font-medium shadow-sm cursor-pointer"
            aria-haspopup="true"
            aria-expanded={dropdownOpen}
          >
            Options <ChevronDown className="w-3 h-3" />
          </button>
          
          {dropdownOpen && (
             <div className="absolute right-0 top-full mt-1 w-32 bg-white rounded-lg shadow-lg border border-slate-200 py-1 z-10 flex flex-col">
                {!isAcknowledged && (
                  <button
                    className="w-full text-left px-3 py-1.5 text-xs text-slate-700 hover:bg-slate-50 flex items-center gap-2 cursor-pointer"
                    onClick={(e) => {
                       e.preventDefault();
                       onMarkRead(item.id);
                       setDropdownOpen(false);
                    }}
                  >
                    <Check className="w-3.5 h-3.5 text-slate-400" /> Read
                  </button>
                )}
                <button
                  className="w-full text-left px-3 py-1.5 text-xs text-red-600 hover:bg-red-50 flex items-center gap-2 cursor-pointer"
                  onClick={async (e) => {
                     e.preventDefault();
                     setDropdownOpen(false);
                     const confirmed = await toast.confirm("Are you sure you want to dismiss this notification?", {
                       confirmLabel: "Dismiss",
                       cancelLabel: "Cancel",
                     });
                     if (confirmed) {
                       onDismiss(item.id);
                       toast.success("Notification dismissed");
                     }
                  }}
                >
                  <X className="w-3.5 h-3.5 text-red-400" /> Dismiss
                </button>
             </div>
          )}
        </div>
      </div>
    </div>
  );
}

export function NotificationList({
  items,
  serviceRefs,
  onMarkRead,
  onDismiss,
  emptyMessage = "No notifications",
  dense = false,
}: {
  items: WorkspaceNotification[];
  serviceRefs: Record<string, NotificationServiceRef>;
  onMarkRead: (id: string) => void;
  onDismiss: (id: string) => void;
  emptyMessage?: string;
  dense?: boolean;
}) {
  if (items.length === 0) {
    return <p className="px-3 py-8 text-sm text-slate-400 text-center">{emptyMessage}</p>;
  }

  return (
    <div className="flex flex-col">
      {items.map((item) => (
        <NotificationRow
          key={item.id}
          item={item}
          serviceRefs={serviceRefs}
          onMarkRead={onMarkRead}
          onDismiss={onDismiss}
          dense={dense}
        />
      ))}
    </div>
  );
}
