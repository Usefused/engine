import type { KeyboardEvent } from "react";

// activateReceiptRow gives table rows the Enter and Space behavior users
// expect from the native buttons used by the mobile receipt layout.
export function activateReceiptRow(event: KeyboardEvent<HTMLElement>, onActivate: () => void) {
  if (event.key !== "Enter" && event.key !== " ") return;
  event.preventDefault();
  onActivate();
}
