import { useEffect, useId, useRef, useState, type ComponentType } from "react";
import { Link } from "@remix-run/react";
import { MoreHorizontal, Trash2 } from "lucide-react";

export interface AppHeaderActionLink {
  label: string;
  to: string;
  icon: ComponentType<{ className?: string }>;
}

interface AppHeaderActionsMenuProps {
  links?: AppHeaderActionLink[];
  canDelete: boolean;
  deleting?: boolean;
  onDelete?: () => void;
}

/** Renders the shared app-detail overflow menu with permission-gated links and app deletion. */
export function AppHeaderActionsMenu({ links = [], canDelete, deleting = false, onDelete }: AppHeaderActionsMenuProps) {
  const [open, setOpen] = useState(false);
  const disclosureID = useId();
  const rootRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const showDelete = canDelete && Boolean(onDelete);

  // Dismiss the menu on outside clicks or Escape and move keyboard focus into the open panel.
  useEffect(() => {
    if (!open) return;
    panelRef.current?.querySelector<HTMLElement>("a, button:not(:disabled)")?.focus();
    /** Closes the disclosure when the user interacts outside the menu bounds. */
    function dismissOutside(event: PointerEvent) {
      if (!rootRef.current?.contains(event.target as Node)) setOpen(false);
    }
    /** Closes on Escape and returns keyboard focus to the invoking overflow button. */
    function dismissOnEscape(event: KeyboardEvent) {
      if (event.key !== "Escape") return;
      event.preventDefault();
      setOpen(false);
      triggerRef.current?.focus();
    }
    document.addEventListener("pointerdown", dismissOutside);
    document.addEventListener("keydown", dismissOnEscape);
    return () => {
      document.removeEventListener("pointerdown", dismissOutside);
      document.removeEventListener("keydown", dismissOnEscape);
    };
  }, [open]);

  // Hide the overflow trigger completely when the current actor has no authorized actions to show.
  if (links.length === 0 && !showDelete) return null;

  return (
    <div ref={rootRef} className="relative shrink-0">
      <button
        ref={triggerRef}
        type="button"
        aria-label="More app actions"
        title="More app actions"
        aria-expanded={open}
        aria-controls={disclosureID}
        onClick={() => setOpen((previous) => !previous)}
        className="inline-flex h-11 w-11 items-center justify-center rounded-lg text-slate-500 transition-colors hover:bg-slate-100 hover:text-slate-900 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-slate-950 sm:h-9 sm:w-9"
      >
        <MoreHorizontal className="h-5 w-5" aria-hidden="true" />
      </button>
      {/* Keep secondary and destructive app actions grouped in one anchored popover. */}
      {open && (
        <div
          ref={panelRef}
          id={disclosureID}
          className="absolute left-0 top-full z-50 mt-2 w-56 rounded-xl border border-slate-200 bg-white p-1.5 shadow-lg md:left-auto md:right-0"
        >
          {links.map(({ label, to, icon: Icon }) => (
            <Link
              key={label}
              to={to}
              onClick={() => setOpen(false)}
              className="flex items-center gap-2 rounded-lg px-3 py-2.5 text-sm text-slate-700 outline-none hover:bg-slate-50 focus:bg-slate-50"
            >
              <Icon className="h-4 w-4" aria-hidden="true" />
              {label}
            </Link>
          ))}
          {/* Separate destructive family deletion from navigation links when both are present. */}
          {showDelete && (
            <div className={links.length > 0 ? "mt-1 border-t border-slate-100 pt-1" : ""}>
              <button
                type="button"
                disabled={deleting}
                onClick={() => {
                  setOpen(false);
                  onDelete?.();
                }}
                className="flex w-full items-center gap-2 rounded-lg px-3 py-2.5 text-left text-sm text-red-600 outline-none hover:bg-red-50 focus:bg-red-50 disabled:opacity-40"
              >
                <Trash2 className="h-4 w-4" aria-hidden="true" />
                {deleting ? "Deleting…" : "Delete app"}
              </button>
            </div>
          )}
        </div>
      )}
    </div>
  );
}
