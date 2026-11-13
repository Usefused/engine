import { useEffect, useId, useRef, useState } from "react";
import { Link } from "@remix-run/react";
import { MoreHorizontal, Pencil, Upload } from "lucide-react";
import { templatePublishPath } from "~/lib/unified-template";

export function UnifiedAppHeaderActions({ appID, canPublishTemplate }: { appID: string; canPublishTemplate: boolean }) {
  const [open, setOpen] = useState(false);
  const disclosureID = useId();
  const rootRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const editRef = useRef<HTMLAnchorElement>(null);

  useEffect(() => {
    if (!open) return;
    editRef.current?.focus();
    function dismissOutside(event: PointerEvent) {
      if (!rootRef.current?.contains(event.target as Node)) setOpen(false);
    }
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

  return (
    <div ref={rootRef} className="relative shrink-0">
      <button
        ref={triggerRef}
        type="button"
        aria-label="More app actions"
        title="More app actions"
        aria-expanded={open}
        aria-controls={disclosureID}
        onClick={() => setOpen(!open)}
        className="inline-flex h-11 w-11 items-center justify-center rounded-lg text-slate-500 transition-colors hover:bg-slate-100 hover:text-slate-900 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-slate-950 sm:h-9 sm:w-9"
      >
        <MoreHorizontal className="h-5 w-5" aria-hidden="true" />
      </button>
      {open && (
        <div id={disclosureID} className="absolute left-0 top-full z-50 mt-2 w-56 rounded-xl border border-slate-200 bg-white p-1.5 shadow-lg md:left-auto md:right-0">
          <Link
            ref={editRef}
            to={`/integrations/unified-apps/new?edit=${encodeURIComponent(appID)}`}
            onClick={() => setOpen(false)}
            className="flex items-center gap-2 rounded-lg px-3 py-2.5 text-sm text-slate-700 outline-none hover:bg-slate-50 focus:bg-slate-50"
          >
            <Pencil className="h-4 w-4" aria-hidden="true" />
            Edit app
          </Link>
          {canPublishTemplate && (
            <Link
              to={templatePublishPath(appID)}
              onClick={() => setOpen(false)}
              className="flex items-center gap-2 rounded-lg px-3 py-2.5 text-sm text-slate-700 outline-none hover:bg-slate-50 focus:bg-slate-50"
            >
              <Upload className="h-4 w-4" aria-hidden="true" />
              Publish as template
            </Link>
          )}
        </div>
      )}
    </div>
  );
}
