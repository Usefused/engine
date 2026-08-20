import { AlertTriangle, Check, Info, X, ChevronDown } from "lucide-react";
import { useState } from "react";
import { useToast } from "~/components/Toast";
import { Link } from "@remix-run/react";
import type { NotificationServiceRef, WorkspaceNotification } from "~/lib/api";
import { notificationLink, notificationTypeLabel } from "./notificationHelpers";
import { canMutateNotification } from "./notificationActions";

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
  canUpdate = false,
}: {
  item: WorkspaceNotification;
  serviceRefs: Record<string, NotificationServiceRef>;
  onMarkRead: (id: string) => void;
  onDismiss: (id: string) => void;
  dense?: boolean;
  canUpdate?: boolean;
}) {
  const isBreaking = item.severity === "breaking";
  const isAcknowledged = item.status === "acknowledged";
  const link = notificationLink(item, serviceRefs);

  return (
    <div className={notificationRowClass(isBreaking, isAcknowledged, dense)}>
      <NotificationIcon isBreaking={isBreaking} />
      <div className="flex-1 min-w-0">
        <div className="flex items-center gap-2 flex-wrap">
          <span className={notificationTypeClass(isBreaking)}>
            {notificationTypeLabel(item)}
          </span>
        </div>
        <NotificationMessage item={item} link={link} dense={dense} />
        
        <NotificationTags item={item} serviceRefs={serviceRefs} />
      </div>
      {canMutateNotification(item.source, canUpdate) && <NotificationActions item={item} isAcknowledged={isAcknowledged} onMarkRead={onMarkRead} onDismiss={onDismiss} />}
    </div>
  );
}

/** Builds the row class without spreading presentation branches through the component. */
function notificationRowClass(isBreaking: boolean, isAcknowledged: boolean, dense: boolean): string {
  const border = isBreaking ? "border-l-red-400" : "border-l-blue-400";
  const spacing = dense ? "px-3 py-3" : "px-4 py-4";
  const opacity = isAcknowledged ? "opacity-80" : "";
  return `flex min-w-0 items-start gap-3 border-l-4 ${border} border-b border-slate-100 last:border-b-0 ${spacing} ${opacity}`;
}

/** Renders the severity icon with its matching tone. */
function NotificationIcon({ isBreaking }: { isBreaking: boolean }) {
  if (isBreaking) return <div className="shrink-0 rounded-lg p-1.5 mt-0.5 bg-red-50 text-red-600"><AlertTriangle className="w-4 h-4" /></div>;
  return <div className="shrink-0 rounded-lg p-1.5 mt-0.5 bg-blue-50 text-blue-600"><Info className="w-4 h-4" /></div>;
}

/** Selects the severity badge class. */
function notificationTypeClass(isBreaking: boolean): string {
  const tone = isBreaking ? "bg-red-100 text-red-700" : "bg-blue-100 text-blue-700";
  return `text-[10px] font-semibold uppercase tracking-wide px-1.5 py-0.5 rounded ${tone}`;
}

/** Renders the notification message as a link only when a safe route exists. */
function NotificationMessage({ item, link, dense }: { item: WorkspaceNotification; link: string | null; dense: boolean }) {
  const className = `mt-1 break-words font-medium text-slate-700 ${dense ? "text-xs" : "text-sm"}`;
  if (link) return <Link to={link} className={`block hover:underline ${className}`} title={item.message}>{item.message}</Link>;
  return <p className={className} title={item.message}>{item.message}</p>;
}

/** Renders copyable service/version/config metadata outside the main row logic. */
function NotificationTags({ item, serviceRefs }: { item: WorkspaceNotification; serviceRefs: Record<string, NotificationServiceRef> }) {
  const toast = useToast();
  const tags = [
    item.service_id ? { value: serviceRefs[item.service_id]?.name || item.service_id, className: "max-w-[150px]" } : null,
    item.version ? { value: item.version, className: "max-w-[120px]" } : null,
    item.config_key ? { value: item.config_key, className: "max-w-[180px]" } : null,
  ].filter((tag): tag is { value: string; className: string } => Boolean(tag));
  if (tags.length === 0) return null;
  return <div className="mt-2 flex min-w-0 max-w-full items-center gap-2 flex-wrap">
    {tags.map((tag) => <button key={tag.value} type="button" onClick={(event) => copyNotificationTag(event, tag.value, toast.success)} className={`min-w-0 truncate text-[10px] font-medium bg-slate-50 text-slate-600 px-1.5 py-0.5 rounded border border-slate-200 hover:bg-slate-100 transition-colors cursor-pointer ${tag.className}`} title="Click to copy">{tag.value}</button>)}
  </div>;
}

/** Copies one notification tag without activating its surrounding link. */
function copyNotificationTag(event: React.MouseEvent, value: string, notify: (message: string) => void) {
  event.preventDefault();
  event.stopPropagation();
  navigator.clipboard.writeText(value);
  notify("Copied to clipboard");
}

/** Renders read and dismiss commands for actors with notification.update. */
function NotificationActions({ item, isAcknowledged, onMarkRead, onDismiss }: { item: WorkspaceNotification; isAcknowledged: boolean; onMarkRead: (id: string) => void; onDismiss: (id: string) => void }) {
  const [open, setOpen] = useState(false);
  const toast = useToast();
  // Confirms terminal dismissal before invoking the authorized mutation.
  const dismiss = async (event: React.MouseEvent) => {
    event.preventDefault();
    setOpen(false);
    const confirmed = await toast.confirm("Are you sure you want to dismiss this notification?", { confirmLabel: "Dismiss", cancelLabel: "Cancel" });
    if (!confirmed) return;
    onDismiss(item.id);
    toast.success("Notification dismissed");
  };
  return <div className="relative shrink-0" onBlur={(event) => { if (!event.currentTarget.contains(event.relatedTarget as Node)) setOpen(false); }}>
    <button type="button" onClick={(event) => { event.preventDefault(); setOpen((value) => !value); }} className="px-2 py-1 rounded border border-slate-200 bg-white text-slate-500 hover:text-slate-700 hover:bg-slate-50 transition-all text-xs flex items-center gap-1 font-medium shadow-sm cursor-pointer" aria-haspopup="true" aria-expanded={open}>Options <ChevronDown className="w-3 h-3" /></button>
    {open && <div className="absolute right-0 top-full mt-1 w-32 bg-white rounded-lg shadow-lg border border-slate-200 py-1 z-10 flex flex-col">
      {!isAcknowledged && <button className="w-full text-left px-3 py-1.5 text-xs text-slate-700 hover:bg-slate-50 flex items-center gap-2 cursor-pointer" onClick={(event) => { event.preventDefault(); onMarkRead(item.id); setOpen(false); }}><Check className="w-3.5 h-3.5 text-slate-400" /> Read</button>}
      <button className="w-full text-left px-3 py-1.5 text-xs text-red-600 hover:bg-red-50 flex items-center gap-2 cursor-pointer" onClick={dismiss}><X className="w-3.5 h-3.5 text-red-400" /> Dismiss</button>
    </div>}
  </div>;
}

/** Renders notification rows with mutation controls disabled by default. */
export function NotificationList({
  items,
  serviceRefs,
  onMarkRead,
  onDismiss,
  emptyMessage = "No notifications",
  dense = false,
  canUpdate = false,
}: {
  items: WorkspaceNotification[];
  serviceRefs: Record<string, NotificationServiceRef>;
  onMarkRead: (id: string) => void;
  onDismiss: (id: string) => void;
  emptyMessage?: string;
  dense?: boolean;
  canUpdate?: boolean;
}) {
  if (items.length === 0) {
    return <p className="px-3 py-8 text-sm text-slate-400 text-center">{emptyMessage}</p>;
  }

  return (
    <div className="flex min-w-0 flex-col">
      {items.map((item) => (
        <NotificationRow
          key={item.id}
          item={item}
          serviceRefs={serviceRefs}
          onMarkRead={onMarkRead}
          onDismiss={onDismiss}
          dense={dense}
          canUpdate={canUpdate}
        />
      ))}
    </div>
  );
}
