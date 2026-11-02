import { createElement, forwardRef, useEffect, useImperativeHandle, useRef, type ReactElement, type SelectHTMLAttributes } from "react";
import { enhanceSearchableSelect } from "../../../branding/searchable_select.js";

export interface SelectProps extends SelectHTMLAttributes<HTMLSelectElement> {
  /** Preserve page instrumentation for both JSX and createElement callers. */
  [attribute: `data-${string}`]: string | number | boolean | undefined;
  /** Compact controls suit table filters and pagination; regular controls suit forms. */
  density?: "regular" | "compact";
  /** Pages can choose a quiet background without rebuilding the control. */
  tone?: "surface" | "subtle";
  /** Filter native single-select options with the same control used on standalone consent. */
  searchable?: boolean;
  searchPlaceholder?: string;
}

/** Keep React ownership of the native element while sharing search, keyboard handling and form behavior. */
const SearchableSelect = forwardRef<HTMLSelectElement, Omit<SelectProps, "searchable">>(function SearchableSelect(props, ref): ReactElement {
  const native = useRef<HTMLSelectElement>(null);
  const control = useRef<ReturnType<typeof enhanceSearchableSelect> | null>(null);
  useImperativeHandle(ref, () => native.current!, []);
  // Cleanup restores labels and native form behavior, including React StrictMode's repeated setup.
  useEffect(() => {
    control.current = enhanceSearchableSelect(native.current!);
    return () => { control.current?.destroy(); control.current = null; };
  }, []);
  // React commits controlled values and option updates before the shared control reads them.
  useEffect(() => { control.current?.sync(); });
  return createElement("span", { className: "fused-select-host" }, createElement(Select, { ...props, ref: native }));
});

/** Share native styling and opt-in search while preserving names, option values, events and native refs. */
export const Select = forwardRef<HTMLSelectElement, SelectProps>(function Select(
  { density = "regular", tone = "surface", className = "", searchable = false, searchPlaceholder, ...props }, ref,
): ReactElement {
  // Multiple/size listboxes retain native semantics; search enhances only ordinary single-choice dropdowns.
  if (searchable && !props.multiple && !(props.size && props.size > 1)) {
    return createElement(SearchableSelect, { ...props, ref, density, tone, className, searchPlaceholder });
  }
  return createElement("select", {
    ...props,
    ref,
    className: `fused-select ${className}`,
    "data-density": density,
    "data-tone": tone,
    "data-search-placeholder": searchPlaceholder,
  });
});
