import { Link } from "@remix-run/react";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { ChevronDown } from "lucide-react";

/** Gives all service catalogs the same introduction and local action placement. */
export function ServiceCatalogHeader({ title, description, children }: { title: string; description: string; children?: ReactNode }) {
  return <div className="flex flex-wrap items-center justify-between gap-3 border-b border-slate-100 p-5">
    <div><h2 className="font-semibold text-slate-900">{title}</h2><p className="mt-1 text-sm text-slate-500">{description}</p></div>
    {children}
  </div>;
}

type CatalogAction = { label: string; href?: string; onSelect?: () => void };

/** Shares dismissal and keyboard behavior across tab-local navigation and import actions. */
export function ServiceCatalogOptions({ actions, disabled = false }: { actions: CatalogAction[]; disabled?: boolean }) {
  const [open, setOpen] = useState(false);
  const container = useRef<HTMLDivElement>(null);
  // Outside interactions should dismiss without intercepting any page action.
  useEffect(() => {
    /** Keeps interactions inside the disclosure available to its trigger and commands. */
    function dismiss(event: PointerEvent) {
      // A pointer outside this disclosure means the user has moved on to another control.
      if (!container.current?.contains(event.target as Node)) setOpen(false);
    }
    document.addEventListener("pointerdown", dismiss);
    return () => document.removeEventListener("pointerdown", dismiss);
  }, []);
  // No empty menu is exposed when the actor has no available catalog actions.
  if (actions.length === 0) return null;
  return <div ref={container} className="relative" onKeyDown={(event) => {
    // Escape returns keyboard users to the control that opened the disclosure.
    if (event.key === "Escape") { setOpen(false); container.current?.querySelector("button")?.focus(); }
  }} onBlur={(event) => {
    // Tabbing out dismisses without trapping focus inside a navigation menu.
    if (!event.currentTarget.contains(event.relatedTarget)) setOpen(false);
  }}>
    <button type="button" aria-expanded={open} disabled={disabled} onClick={() => setOpen(!open)} className="inline-flex items-center gap-2 rounded-md border border-slate-300 bg-white px-3 py-1.5 text-sm font-medium text-slate-700 hover:bg-slate-50 disabled:opacity-50">Options<ChevronDown size={14} aria-hidden="true" /></button>
    {/* Pending mutations must not leave active commands behind the disabled trigger. */}
    {open && !disabled && <div className="absolute right-0 z-20 mt-2 w-52 rounded-lg border border-slate-200 bg-white p-1 shadow-lg">
      {actions.map((action) => <CatalogActionItem key={action.label} action={action} onClose={() => setOpen(false)} />)}
    </div>}
  </div>;
}

/** Preserves native link navigation while closing commands before their panel opens. */
function CatalogActionItem({ action, onClose }: { action: CatalogAction; onClose: () => void }) {
  const className = "block w-full rounded-md px-3 py-2 text-left text-sm text-slate-700 hover:bg-slate-50";
  // Links retain browser navigation semantics; import commands open the existing local editor.
  if (action.href) return <Link to={action.href} onClick={onClose} className={className}>{action.label}</Link>;
  return <button type="button" onClick={() => { onClose(); action.onSelect?.(); }} className={className}>{action.label}</button>;
}
