import { createElement, type ReactNode } from "react";

interface FieldLabelProps {
  children?: ReactNode;
  required?: boolean;
}

/** Shares the neutral required badge across JSX and createElement forms without replacing their semantic labels. */
export function FieldLabel({ children, required = false }: FieldLabelProps) {
  return createElement("span", { className: "flex min-w-0 flex-wrap items-center justify-between gap-2" },
    createElement("span", { className: "min-w-0 break-words" }, children),
    // Native required or aria-required on the control supplies accessibility semantics without duplicating its name.
    required ? createElement("span", { "aria-hidden": true, className: "shrink-0 rounded-md border border-slate-200 bg-slate-50 px-2 py-0.5 text-[11px] font-medium leading-4 text-slate-500" }, "Required") : null,
  );
}
