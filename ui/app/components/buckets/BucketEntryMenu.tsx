import { useEffect, useId, useRef, useState } from "react";
import { MoreVertical, Trash2 } from "lucide-react";

/** Separates opening row options from requesting a confirmed removal. */
export function BucketEntryMenu({ name, onRemove }: { name: string; onRemove: () => void }) {
  const [open, setOpen] = useState(false);
  const menuId = useId();
  const rootRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const removeRef = useRef<HTMLButtonElement>(null);

  // Scope dismissal listeners to an open menu; neither dismissal path requests removal.
  useEffect(() => {
    // Closed menus must not capture keyboard or pointer events elsewhere in the drawer.
    if (!open) return;
    removeRef.current?.focus();
    // Clicking outside abandons the menu without changing the entry.
    function dismissOutside(event: PointerEvent) {
      // Pointer activity within the menu belongs to its selected control.
      if (!rootRef.current?.contains(event.target as Node)) setOpen(false);
    }
    // Escape restores focus to the options button without selecting its action.
    function dismissOnEscape(event: KeyboardEvent) {
      // Only Escape dismisses this menu; ordinary activation stays with the focused button.
      if (event.key !== "Escape") return;
      event.preventDefault();
      event.stopPropagation();
      setOpen(false);
      triggerRef.current?.focus();
    }
    document.addEventListener("pointerdown", dismissOutside);
    document.addEventListener("keydown", dismissOnEscape);
    // Unmounting or closing must not leave global listeners behind.
    return () => {
      document.removeEventListener("pointerdown", dismissOutside);
      document.removeEventListener("keydown", dismissOnEscape);
    };
  }, [open]);

  // The options trigger changes disclosure state only and cannot request deletion.
  function toggleMenu() {
    setOpen(!open);
  }

  // Requesting removal closes the menu first; the route owns explicit confirmation.
  function requestRemoval() {
    setOpen(false);
    triggerRef.current?.focus();
    onRemove();
  }

  return (
    <div ref={rootRef} className="relative">
      <button
        ref={triggerRef}
        type="button"
        onClick={toggleMenu}
        aria-label={`Options for ${name}`}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={menuId}
        title="Options"
        className="rounded-md p-1.5 text-slate-400 hover:bg-slate-50 hover:text-slate-700"
      >
        <MoreVertical className="h-4 w-4" />
      </button>
      {/* The destructive command is available only after an explicit menu opening. */}
      {open && (
        <div id={menuId} role="menu" aria-label={`Options for ${name}`} className="absolute right-0 top-full z-30 mt-1 w-40 rounded-lg border border-slate-200 bg-white p-1 shadow-lg">
          <button ref={removeRef} type="button" role="menuitem" onClick={requestRemoval} className="flex w-full items-center gap-2 rounded-md px-3 py-2 text-left text-sm text-red-600 hover:bg-red-50 focus:bg-red-50">
            <Trash2 className="h-4 w-4" />
            Remove…
          </button>
        </div>
      )}
    </div>
  );
}
