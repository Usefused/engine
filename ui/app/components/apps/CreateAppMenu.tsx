import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";
import { useEffect, useId, useRef, useState, type ComponentType } from "react";
import { Link } from "@remix-run/react";
import { ChevronDown, Code2, Globe2, Layers3, Plus, TerminalSquare, type LucideProps } from "lucide-react";
import type { AppCreationMode } from "~/lib/app-builder-contract";
import { serviceAppHref, type ServiceAppSource } from "~/lib/service-app-launch";

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
  service?: ServiceAppSource;
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
    description: "Call operations through Fused",
    to: "/integrations/builder?tab=api",
    icon: Globe2,
  },
];

/** Offers authorized creation paths with compact service actions and a menu contained by the mobile action row. */
export function CreateAppMenu({ className = "", align = "end", service }: CreateAppMenuProps) {
  const { access } = useCurrentActorAccess();
  // Each choice appears only when its own creation permission is available.
  const options = CREATE_APP_OPTIONS.filter((option) => {
    // Service shortcuts offer the three creation flows supported by this entry point.
    return (!service || option.mode !== "api") && hasWorkspacePermission(access, `app.${option.mode}.create`);
  });
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
  // Service menus use the whole action row on phones so a trailing action cannot push the popup off-screen.
  const rootPosition = service ? "static shrink-0 sm:relative" : "relative min-w-0";
  const menuPosition = service ? "inset-x-0 w-full sm:left-auto sm:right-0 sm:w-72" : `${alignmentClass} w-72`;
  // A disclosure with no authorized destinations would leave readers at a dead end.
  if (options.length === 0) return null;

  return (
    <div ref={rootRef} className={`inline-flex ${rootPosition} ${className}`}>
      <button
        ref={triggerRef}
        type="button"
        onClick={toggleMenu}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={menuId}
        className="inline-flex h-11 w-full shrink-0 items-center justify-center gap-1.5 whitespace-nowrap rounded-lg bg-slate-950 px-3 text-sm font-medium text-white shadow-sm transition-colors hover:bg-slate-800 sm:h-9 sm:w-auto sm:gap-2 sm:px-4"
      >
        {/* The label carries the mobile action; the decorative plus returns when space allows. */}
        <Plus className={`h-4 w-4 shrink-0 ${service ? "hidden sm:block" : ""}`} aria-hidden="true" />
        {/* Service details provide context; the catalogue retains its general creation label. */}
        {service ? "Use in App" : "Create App"}
        <ChevronDown className={`h-4 w-4 shrink-0 transition-transform ${open ? "rotate-180" : ""}`} />
      </button>

      {/* The menu offers only authorized app types in their catalogue order. */}
      {open && (
        <div
          id={menuId}
          role="menu"
          aria-label="Choose app type"
          className={`absolute ${menuPosition} top-full z-50 mt-2 max-w-[calc(100vw-2rem)] max-h-[min(28rem,calc(100dvh-6rem))] overflow-y-auto rounded-xl border border-slate-200 bg-white p-1.5 text-left shadow-xl`}
        >
          {options.map((option, index) => {
            const Icon = option.icon;
            return (
              <Link
                key={option.to}
                ref={index === 0 ? firstItemRef : undefined}
                role="menuitem"
                to={serviceAppHref(option.to, service)}
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
