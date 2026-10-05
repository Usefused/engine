import { createElement, type ReactNode } from "react";

interface FieldLabelProps {
  children?: ReactNode;
  required?: boolean;
}

/** Keeps required badges beside wrapping label text and reserves equal header height for optional fields. */
export function FieldLabel({ children, required = false }: FieldLabelProps) {
  // Only the label text may wrap; an orphaned badge would add a row above its input.
  return createElement("span", { className: "flex min-h-7 min-w-0 items-center justify-between gap-2" },
    createElement("span", { className: "min-w-0 break-words" }, children),
    // Native required or aria-required on the control supplies accessibility semantics without duplicating its name.
    required ? createElement("span", { "aria-hidden": true, className: "shrink-0 rounded-md border border-slate-200 bg-slate-50 px-2 py-0.5 text-[11px] font-medium leading-4 text-slate-500" }, "Required") : null,
  );
}
