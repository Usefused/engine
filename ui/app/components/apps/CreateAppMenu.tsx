import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";
import { useEffect, useId, useRef, useState, type ComponentType } from "react";
import { Link } from "@remix-run/react";
import { ChevronDown, Code2, Globe2, Layers3, Plus, TerminalSquare, type LucideProps } from "lucide-react";
import type { AppCreationMode } from "~/lib/app-builder-contract";

export type CreateAppOption = {
  mode: AppCreationMode;
  label: string;
  description: string;
  to: string;
  icon: ComponentType<LucideProps>;
};

type CreateAppMenuProps = {
  className?: string;
};

export const CREATE_APP_OPTIONS: CreateAppOption[] = [
  {
    mode: "app",
    label: "App",
    description: "One App with SDK, MCP, and REST delivery",
    to: "/integrations/builder?tab=app",
    icon: Layers3,
  },
  {
    mode: "sdk",
    label: "SDK",
    description: "Generate a typed package",
    to: "/integrations/builder?tab=sdk",
    icon: Code2,
  },
  {
    mode: "api",
    label: "REST API",
    description: "Call operations through the Engine",
    to: "/integrations/builder?tab=api",
    icon: Globe2,
  },
  {
    mode: "mcp",
    label: "MCP server",
    description: "Connect agents over MCP",
    to: "/integrations/builder?tab=mcp",
    icon: TerminalSquare,
  },
];

/** Presents delivery choices without splitting the shared app catalogue. */
export function CreateAppMenu({ className = "" }: CreateAppMenuProps) {
  const { access } = useCurrentActorAccess();
  // A combined App requires both delivery grants; individual choices retain their own grants.
  const options = CREATE_APP_OPTIONS.filter((option) => option.mode === "app"
    ? hasWorkspacePermission(access, "app.sdk.create") && hasWorkspacePermission(access, "app.mcp.create")
    : hasWorkspacePermission(access, `app.${option.mode}.create`));
  // The primary action creates all three methods when authorized; narrower grants use their first available choice.
  const primaryOption = options.find((option) => option.mode === "app") ?? options[0];
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

  /** Toggles optional single-method choices beside the default App action. */
  function toggleMenu() {
    setOpen((current) => !current);
  }

  /** Closes the transient disclosure before navigation starts. */
  function closeMenu() {
    setOpen(false);
  }

  return (
    <div ref={rootRef} className={`relative ${className}`}>
      <div className="inline-flex w-full items-stretch rounded-lg bg-slate-950 text-white shadow-sm sm:w-auto">
      <Link
        to={primaryOption?.mode === "app" ? "/integrations/builder" : primaryOption?.to ?? "/integrations/builder"}
        className="inline-flex flex-1 items-center justify-center gap-2 rounded-l-lg px-4 py-2 text-sm font-medium transition-colors hover:bg-slate-800"
      >
        <Plus className="h-4 w-4" />
        Create app
      </Link>
      <button
        ref={triggerRef}
        type="button"
        onClick={toggleMenu}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={menuId}
        aria-label="Choose a delivery method"
        className="inline-flex items-center justify-center rounded-r-lg border-l border-slate-700 px-3 transition-colors hover:bg-slate-800"
      >
        <ChevronDown className={`h-4 w-4 transition-transform ${open ? "rotate-180" : ""}`} />
      </button>
      </div>

      {/* The menu names delivery adapters while preserving one application lifecycle. */}
      {open && (
        <div
          id={menuId}
          role="menu"
          aria-label="Choose app type"
          className="absolute right-0 top-full z-30 mt-2 w-72 overflow-hidden rounded-xl border border-slate-200 bg-white p-1.5 text-left shadow-xl"
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
