import { createElement, type ReactNode } from "react";

/** Keeps credential creation a quiet text action beside selectors, with a visible keyboard focus indicator. */
export function CreateCredentialButton({ onClick, disabled, children = "Create bucket" }: {
  onClick: () => void; disabled?: boolean; children?: ReactNode;
}) {
  return createElement("button", {
    type: "button", onClick, disabled, "data-track": "create_builder_credential",
    className: "inline-flex items-center py-1 text-sm font-normal text-[var(--brand-violet)] hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--brand-violet)] focus-visible:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-50",
  }, children);
}
