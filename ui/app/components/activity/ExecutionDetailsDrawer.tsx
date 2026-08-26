import { useEffect, useLayoutEffect, useRef, type ReactNode, type RefObject } from "react";
import { ArrowLeft, X } from "lucide-react";
import type { EngineExecutionEventEntry } from "~/lib/api";

interface ExecutionDetailsDrawerProps {
  event: EngineExecutionEventEntry;
  onClose: () => void;
  children: ReactNode;
  onBack?: () => void;
  scrollRef?: RefObject<HTMLDivElement>;
  restoreScrollTop?: number;
  navigationDirection?: "forward" | "backward";
}

// ExecutionDetailsDrawer gives every scoped Activity surface one consistent,
// wide receipt inspector without adding another execution-detail projection.
export function ExecutionDetailsDrawer({ event, onClose, children, onBack, scrollRef, restoreScrollTop = 0, navigationDirection = "forward" }: ExecutionDetailsDrawerProps) {
  const titleRef = useRef<HTMLHeadingElement>(null);
  // Restore keyboard focus to the invoking row when the entire inspector closes.
  useLayoutEffect(() => {
    const trigger = document.activeElement;
    return () => {
      // A removed receipt row cannot receive focus after a filter change.
      if (trigger instanceof HTMLElement && trigger.isConnected) trigger.focus({ preventScroll: true });
    };
  }, []);

  // Returning from a child restores position before paint instead of flashing the parent's first row.
  useLayoutEffect(() => {
    if (scrollRef?.current) scrollRef.current.scrollTop = restoreScrollTop;
    titleRef.current?.focus({ preventScroll: true });
  }, [event.id, restoreScrollTop, scrollRef]);

  // The shared dismissal lifecycle remains unchanged while moving between receipt views.
  useEffect(() => {
    // Escape keeps keyboard dismissal aligned with the visible close controls.
    const closeOnEscape = (keyboardEvent: KeyboardEvent) => {
      if (keyboardEvent.key === "Escape") onClose();
    };
    window.addEventListener("keydown", closeOnEscape);
    return () => window.removeEventListener("keydown", closeOnEscape);
  }, [onClose]);

  // Logical parents have no provider; retain the authored operation as their navigation context.
  const service = event.execution_kind === "unified" ? "Unified operation" : event.service_name || event.service_slug || "Service metadata unavailable";
  return <>
    <button type="button" aria-label="Close execution details" className="fixed inset-0 z-40 cursor-default bg-slate-900/20" onClick={onClose} />
    <aside role="dialog" aria-modal="true" aria-labelledby="execution-details-title" className="receipt-drawer-enter fixed inset-y-0 right-0 z-50 flex h-dvh max-h-dvh w-full max-w-full flex-col overflow-hidden border-l border-slate-200 bg-white shadow-2xl md:w-[calc(100vw-4rem)] md:max-w-[940px] xl:max-w-[1080px]">
      <div className="z-10 flex shrink-0 items-center justify-between gap-4 border-b border-slate-100 bg-white/90 px-4 py-4 backdrop-blur sm:px-6">
        <div className="min-w-0">
          {/* Back replaces content in this drawer; it never stacks a second dialog over the first. */}
          {onBack ? <button type="button" onClick={onBack} className="mb-2 inline-flex items-center gap-1.5 text-xs font-medium text-blue-700 hover:underline"><ArrowLeft className="h-3.5 w-3.5" />Back to Unified</button> : null}
          <h2 ref={titleRef} tabIndex={-1} id="execution-details-title" className="text-lg font-semibold text-slate-900 outline-none">Execution details</h2>
          <p className="mt-0.5 truncate text-xs text-slate-500">{service} · {event.transport.toUpperCase()}</p>
        </div>
        <button type="button" onClick={onClose} aria-label="Close execution details" className="rounded-full p-2 text-slate-400 transition-colors hover:bg-slate-100 hover:text-slate-600"><X className="h-5 w-5" /></button>
      </div>
      <div ref={scrollRef} className="min-w-0 flex-1 overflow-y-auto overflow-x-hidden overscroll-contain p-4 [overflow-wrap:anywhere] sm:p-6"><div key={event.id} className={`receipt-view-${navigationDirection}`}>{children}</div></div>
    </aside>
  </>;
}
