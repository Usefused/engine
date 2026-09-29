import { createElement, forwardRef, type SelectHTMLAttributes } from "react";

export interface SelectProps extends SelectHTMLAttributes<HTMLSelectElement> {
  /** Preserve page instrumentation for both JSX and createElement callers. */
  [attribute: `data-${string}`]: string | number | boolean | undefined;
  /** Compact controls suit table filters and pagination; regular controls suit forms. */
  density?: "regular" | "compact";
  /** Pages can choose a quiet background without rebuilding the control. */
  tone?: "surface" | "subtle";
}

/** Shares dropdown styling while preserving native options, form semantics, events, and DOM refs. */
export const Select = forwardRef<HTMLSelectElement, SelectProps>(function Select(
  { density = "regular", tone = "surface", className = "", ...props }, ref,
) {
  return createElement("select", {
    ...props,
    ref,
    className: `fused-select ${className}`,
    "data-density": density,
    "data-tone": tone,
  });
});
