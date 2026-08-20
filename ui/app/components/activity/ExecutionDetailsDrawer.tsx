import { useEffect, type ReactNode } from "react";
import { X } from "lucide-react";
import type { EngineExecutionEventEntry } from "~/lib/api";

interface ExecutionDetailsDrawerProps {
  event: EngineExecutionEventEntry;
  onClose: () => void;
  children: ReactNode;
}

// ExecutionDetailsDrawer gives every scoped Activity surface one consistent,
// wide receipt inspector without adding another execution-detail projection.
export function ExecutionDetailsDrawer({ event, onClose, children }: ExecutionDetailsDrawerProps) {
  useEffect(() => {
    // Escape keeps keyboard dismissal aligned with the visible close controls.
    const closeOnEscape = (keyboardEvent: KeyboardEvent) => {
      if (keyboardEvent.key === "Escape") onClose();
    };
    window.addEventListener("keydown", closeOnEscape);
    return () => window.removeEventListener("keydown", closeOnEscape);
  }, [onClose]);

  const service = event.service_name || event.service_slug || "Service metadata unavailable";
  return <>
    <button type="button" aria-label="Close execution details" className="fixed inset-0 z-40 cursor-default bg-slate-900/20" onClick={onClose} />
    <aside role="dialog" aria-modal="true" aria-labelledby="execution-details-title" className="fixed inset-y-0 right-0 z-50 flex h-dvh max-h-dvh w-full max-w-full flex-col overflow-hidden border-l border-slate-200 bg-white shadow-2xl md:w-[calc(100vw-4rem)] md:max-w-[940px] xl:max-w-[1080px]">
      <div className="z-10 flex shrink-0 items-center justify-between gap-4 border-b border-slate-100 bg-white/90 px-4 py-4 backdrop-blur sm:px-6">
        <div className="min-w-0">
          <h2 id="execution-details-title" className="text-lg font-semibold text-slate-900">Execution details</h2>
          <p className="mt-0.5 truncate text-xs text-slate-500">{service} · {event.transport.toUpperCase()}</p>
        </div>
        <button type="button" onClick={onClose} aria-label="Close execution details" className="rounded-full p-2 text-slate-400 transition-colors hover:bg-slate-100 hover:text-slate-600"><X className="h-5 w-5" /></button>
      </div>
      <div className="min-w-0 flex-1 overflow-y-auto overflow-x-hidden overscroll-contain p-4 [overflow-wrap:anywhere] sm:p-6">{children}</div>
    </aside>
  </>;
}
