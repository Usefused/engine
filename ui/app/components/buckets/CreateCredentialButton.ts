import { createElement, type ReactNode } from "react";
import { Plus } from "lucide-react";

/** Matches the Create App action's color, spacing, icon, and focus treatment across credential entry points. */
export function CreateCredentialButton({ onClick, disabled, children = "Create credential set" }: {
  onClick: () => void; disabled?: boolean; children?: ReactNode;
}) {
  return createElement("button", {
    type: "button", onClick, disabled, "data-track": "create_builder_credential",
    className: "inline-flex items-center justify-center gap-2 rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white shadow-sm transition-colors hover:bg-slate-800 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-slate-400 focus-visible:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-50",
  }, createElement(Plus, { className: "h-4 w-4", "aria-hidden": true }), children);
}
