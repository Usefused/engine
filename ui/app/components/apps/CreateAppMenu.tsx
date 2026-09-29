import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";
import { useEffect, useId, useRef, useState, type ComponentType } from "react";
import { Link } from "@remix-run/react";
import { ChevronDown, Code2, Globe2, Layers3, Plus, TerminalSquare, type LucideProps } from "lucide-react";
import type { AppCreationMode } from "~/lib/app-builder-contract";

export type CreateAppOption = {
  mode: Exclude<AppCreationMode, "app"> | "unified_app";
  label: string;
  description: string;
  to: string;
  icon: ComponentType<LucideProps>;
};

type CreateAppMenuProps = {
  className?: string;
  align?: "end" | "center";
};

export const CREATE_APP_OPTIONS: CreateAppOption[] = [
  { mode: "mcp", label: "MCP", description: "Connect agents to selected operations", to: "/integrations/builder?tab=mcp", icon: TerminalSquare },
  { mode: "unified_app", label: "Unified App", description: "Build one app that coordinates multiple services", to: "/integrations/unified-apps/new", icon: Layers3 },
  {
    mode: "sdk",
    label: "SDK",
    description: "Generate a typed package",
    to: "/integrations/builder?tab=sdk",
    icon: Code2,
  },
  {
    mode: "api",
    label: "REST",
    description: "Call operations through the Engine",
    to: "/integrations/builder?tab=api",
    icon: Globe2,
  },
];

/** Presents the four app types in catalogue order through one creation menu. */
export function CreateAppMenu({ className = "", align = "end" }: CreateAppMenuProps) {
  const { access } = useCurrentActorAccess();
  // Each choice appears only when its own creation permission is available.
  const options = CREATE_APP_OPTIONS.filter((option) => hasWorkspacePermission(access, `app.${option.mode}.create`));
  const [open, setOpen] = useState(false);
  const menuId = useId();
  const rootRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const firstItemRef = useRef<HTMLAnchorElement>(null);

  // Dismissal listeners exist only while the menu is open so unrelated page input remains untouched.
  useEffect(() => {
    // A closed disclosure has no global pointer or keyboard behavior to manage.
    if (!open) return;
    firstItemRef.current?.focus();

    /** Closes the delivery menu when the pointer moves to another UI surface. */
    function dismissOutside(event: PointerEvent) {
      // Pointer activity inside the disclosure belongs to its trigger or options.
      if (!rootRef.current?.contains(event.target as Node)) setOpen(false);
    }

    /** Restores focus to the trigger when the user abandons the menu with Escape. */
    function dismissOnEscape(event: KeyboardEvent) {
      // Other keys retain their native link and focus behavior.
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

  /** Opens the type menu without silently choosing a default app type. */
  function toggleMenu() {
    setOpen((current) => !current);
  }

  /** Closes the transient disclosure before navigation starts. */
  function closeMenu() {
    setOpen(false);
  }

  // Empty-state triggers are centered; header triggers anchor the popup to their trailing edge.
  const alignmentClass = align === "center" ? "left-1/2 -translate-x-1/2" : "right-0";

  return (
    <div ref={rootRef} className={`relative inline-flex min-w-0 ${className}`}>
      <button
        ref={triggerRef}
        type="button"
        onClick={toggleMenu}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={menuId}
        className="inline-flex w-full items-center justify-center gap-2 rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white shadow-sm transition-colors hover:bg-slate-800 sm:w-auto"
      >
        <Plus className="h-4 w-4" />
        Create App
        <ChevronDown className={`h-4 w-4 transition-transform ${open ? "rotate-180" : ""}`} />
      </button>

      {/* The menu offers only authorized app types in their catalogue order. */}
      {open && (
        <div
          id={menuId}
          role="menu"
          aria-label="Choose app type"
          className={`absolute ${alignmentClass} top-full z-50 mt-2 w-72 max-w-[calc(100vw-2rem)] max-h-[min(28rem,calc(100dvh-6rem))] overflow-y-auto rounded-xl border border-slate-200 bg-white p-1.5 text-left shadow-xl`}
        >
          {options.map((option, index) => {
            const Icon = option.icon;
            return (
              <Link
                key={option.to}
                ref={index === 0 ? firstItemRef : undefined}
                role="menuitem"
                to={option.to}
                onClick={closeMenu}
                className="flex items-start gap-3 rounded-lg px-3 py-2.5 text-slate-700 outline-none hover:bg-slate-50 focus:bg-slate-50"
              >
                <span className="mt-0.5 flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-slate-100 text-slate-600">
                  <Icon className="h-4 w-4" />
                </span>
                <span className="min-w-0">
                  <span className="block text-sm font-semibold text-slate-900">{option.label}</span>
                  <span className="mt-0.5 block text-xs text-slate-500">{option.description}</span>
                </span>
              </Link>
            );
          })}
        </div>
      )}
    </div>
  );
}
