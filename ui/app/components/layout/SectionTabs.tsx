import { Link, useLocation } from "@remix-run/react";

export type SectionTab = {
  label: string;
  to: string;
  // Defaults to a pathname-prefix match against `to` when omitted.
  isActive?: (pathname: string) => boolean;
};

/** Shares route navigation, allowing long-label sections to keep every destination visible on phones. */
export function SectionTabs({ tabs, mobileLayout = "scroll", label = "Section navigation" }: { tabs: SectionTab[]; mobileLayout?: "scroll" | "grid"; label?: string }) {
  const location = useLocation();
  // Access uses a compact grid on phones; other sections retain their horizontal navigation.
  const grid = mobileLayout === "grid";
  const layout = grid
    ? "grid grid-cols-2 gap-1 rounded-xl border border-slate-200 bg-slate-100/70 p-1 sm:flex sm:items-center sm:rounded-none sm:border-x-0 sm:border-t-0 sm:bg-transparent sm:p-0"
    : "flex items-center gap-1 overflow-x-auto border-b border-slate-200";
  return (
    <nav aria-label={label} className={`mb-6 -mt-1 min-w-0 ${layout}`}>
      {tabs.map((tab) => {
        // Custom route matching stays authoritative for sections with nested destinations.
        const isActive = tab.isActive ? tab.isActive(location.pathname) : location.pathname.startsWith(tab.to);
        // The mobile grid uses a filled selection; desktop keeps the familiar underline.
        const shape = grid ? "justify-center rounded-lg px-3 sm:rounded-none sm:px-4 sm:-mb-px" : "px-4 -mb-px";
        const selected = grid ? "border-transparent bg-white text-slate-900 shadow-sm sm:border-slate-900 sm:bg-transparent sm:shadow-none" : "border-slate-900 text-slate-900";
        return (
          <Link
            key={tab.to}
            to={tab.to}
            // Expose the current destination to screen readers as well as visually.
            aria-current={isActive ? "page" : undefined}
            className={`flex min-h-11 min-w-0 shrink-0 items-center whitespace-nowrap py-2.5 text-sm font-medium border-b-2 transition-colors focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-blue-600 ${shape} ${
              // Inactive destinations remain distinct from the selected surface at every width.
              isActive
                ? selected
                : "border-transparent text-slate-500 hover:text-slate-700 hover:border-slate-300"
            }`}
          >
            {tab.label}
          </Link>
        );
      })}
    </nav>
  );
}
