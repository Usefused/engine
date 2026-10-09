import { useEffect, useLayoutEffect, useRef, useState, type ReactNode, type RefObject } from "react";
import { createPortal } from "react-dom";

type AnchoredPopoverProps = {
  anchor: RefObject<HTMLElement>;
  onClose: () => void;
  children: ReactNode;
  busy?: boolean;
  width?: number;
};

/** Supports both direct button refs and wrappers used by existing action controls. */
export function focusPopoverAnchor(anchor: RefObject<HTMLElement>) {
  const trigger = anchor.current;
  // Wrapper anchors delegate focus to their actionable child.
  (trigger?.querySelector<HTMLElement>('button, a[href], [tabindex="0"]') ?? trigger)?.focus();
}

/** Gives small forms shared viewport placement and dismissal without dimming or trapping the page. */
export function AnchoredPopover({ anchor, onClose, children, busy = false, width = 384 }: AnchoredPopoverProps) {
  const panel = useRef<HTMLDivElement>(null);
  const [position, setPosition] = useState({ top: 0, left: 0, width });

  // Portalling escapes transformed panes; measurement keeps the form beside its action.
  useLayoutEffect(() => {
    const trigger = anchor.current;
    // Placement needs a mounted trigger; callers control the visible lifetime.
    if (!trigger) return;
    /** Reflows with the trigger and with validation content while keeping every field reachable. */
    function place() {
      const rect = trigger!.getBoundingClientRect();
      const fittedWidth = Math.min(width, Math.max(0, window.innerWidth - 32));
      const height = panel.current?.offsetHeight ?? 0;
      // Prefer below the action, flipping above when the full form cannot fit beneath it.
      const top = rect.bottom + 10 + height <= window.innerHeight - 16
        ? rect.bottom + 10 : Math.max(16, rect.top - height - 10);
      setPosition({ top, left: Math.max(16, Math.min(rect.right - fittedWidth, window.innerWidth - fittedWidth - 16)), width: fittedWidth });
    }
    place();
    const resize = new ResizeObserver(place);
    resize.observe(trigger);
    // Error messages and expanding text areas may change the panel height.
    if (panel.current) resize.observe(panel.current);
    window.addEventListener("resize", place);
    document.addEventListener("scroll", place, true);
    /** Release viewport tracking when the draft unmounts or changes anchor. */
    return () => { resize.disconnect(); window.removeEventListener("resize", place); document.removeEventListener("scroll", place, true); };
  }, [anchor, width]);

  // Pending mutations retain their form until the caller receives the result.
  useEffect(() => {
    // Dismissal during a save could hide failures or lose a successful creation receipt.
    if (busy) return;
    /** Leaves assistant interactions available for editing the same unsaved draft. */
    function outside(event: PointerEvent) {
      const target = event.target as Node;
      // The trigger, form, and assistant are all part of this interaction.
      if (panel.current?.contains(target) || anchor.current?.contains(target) || (target instanceof Element && target.closest("[data-fused-agent]"))) return;
      onClose();
    }
    /** Keyboard dismissal restores the invoking control without stealing pointer focus. */
    function escape(event: KeyboardEvent) {
      // Escape in the assistant belongs to its conversation rather than this form.
      if (event.key !== "Escape" || (event.target instanceof Element && event.target.closest("[data-fused-agent]"))) return;
      event.preventDefault(); event.stopPropagation(); onClose(); focusPopoverAnchor(anchor);
    }
    document.addEventListener("pointerdown", outside);
    document.addEventListener("keydown", escape, true);
    /** Keep dismissal listeners scoped to this visible, idle form. */
    return () => { document.removeEventListener("pointerdown", outside); document.removeEventListener("keydown", escape, true); };
  }, [anchor, busy, onClose]);

  // An explicit surface cursor avoids automatic text-edge switching; native fields and buttons retain their own cursors.
  return createPortal(
    <div ref={panel} data-fused-workspace-dialog style={position} className="fixed z-50 cursor-default max-h-[calc(100dvh-2rem)] overflow-y-auto rounded-xl border border-slate-200 bg-white shadow-lg">
      {children}
    </div>, document.body
  );
}
