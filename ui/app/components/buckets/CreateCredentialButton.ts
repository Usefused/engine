import { createElement, type ReactNode } from "react";

/** Keeps creation quiet while exposing an attached popover's state to assistive technology. */
export function CreateCredentialButton({ onClick, disabled, expanded, controls, children = "Create bucket" }: {
  onClick: () => void; disabled?: boolean; expanded?: boolean; controls?: string; children?: ReactNode;
}) {
  return createElement("button", {
    type: "button", onClick, disabled, "data-track": "create_builder_credential",
    "aria-expanded": expanded, "aria-controls": controls, "aria-haspopup": controls ? "dialog" : undefined,
    className: "inline-flex items-center py-1 text-sm font-normal text-slate-950 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-slate-950 focus-visible:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-50",
  }, children);
}
