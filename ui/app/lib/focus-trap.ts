export interface DrawerKeyboardOptions {
  selector: string;
  trapFocus?: boolean;
  includePanelOnShiftTab?: boolean;
  preventEscapeDefault?: boolean;
}

/** Handles Escape dismissal and modal Tab focus wrapping for slide-over drawers and dialogs. */
export function handleDrawerKeyboard(
  event: Pick<KeyboardEvent, "key" | "shiftKey" | "preventDefault" | "stopPropagation">,
  panel: HTMLElement | null,
  close: () => void,
  options: DrawerKeyboardOptions
) {
  // Route Escape through the drawer's close handler while preventing outer surfaces from handling it.
  if (event.key === "Escape") {
    // Callers that keep outer focus intact use preventDefault instead of stopPropagation.
    if (options.preventEscapeDefault) event.preventDefault();
    else event.stopPropagation();
    close();
    return;
  }
  // Skip focus trapping for non-Tab keys, unmounted panels, or non-modal shared-assistant sessions.
  if (options.trapFocus === false || event.key !== "Tab" || !panel) return;
  const controls = Array.from(panel.querySelectorAll<HTMLElement>(options.selector)).filter(
    // Hidden or collapsed controls cannot receive keyboard focus.
    (node) => node.getClientRects().length > 0
  );
  const first = controls[0];
  const last = controls[controls.length - 1];
  const atStart =
    options.includePanelOnShiftTab
      ? [first, panel].includes(document.activeElement as HTMLElement)
      : document.activeElement === first;
  // Wrap backward navigation from the first focusable control to the end of the drawer.
  if (event.shiftKey && atStart) {
    event.preventDefault();
    last?.focus();
  } else if (!event.shiftKey && document.activeElement === last) {
    // Wrap forward navigation from the last focusable control back to the start of the drawer.
    event.preventDefault();
    first?.focus();
  }
}
