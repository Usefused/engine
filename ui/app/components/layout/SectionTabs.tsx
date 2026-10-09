import { Link, useLocation } from "@remix-run/react";

export type SectionTab = {
  label: string;
  to: string;
  // Defaults to a pathname-prefix match against `to` when omitted.
  isActive?: (pathname: string) => boolean;
};

/** Keeps route destinations on one scrollable strip while preserving native link and keyboard navigation. */
export function SectionTabs({ tabs, label = "Section navigation" }: { tabs: SectionTab[]; label?: string }) {
  const location = useLocation();
  return (
    <nav aria-label={label} className="fused-tabs-scroll mb-6 -mt-1 items-center gap-1 border-b border-slate-200">
      {tabs.map((tab) => {
        // Custom route matching stays authoritative for sections with nested destinations.
        const isActive = tab.isActive ? tab.isActive(location.pathname) : location.pathname.startsWith(tab.to);
        return (
          <Link
            key={tab.to}
            to={tab.to}
            // Expose the current destination to screen readers as well as visually.
            aria-current={isActive ? "page" : undefined}
            className={`flex min-h-11 items-center whitespace-nowrap border-b-2 px-4 py-2.5 text-sm font-medium transition-colors focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-slate-950 ${
              // Selection remains distinct without changing destination widths while scrolling.
              isActive ? "border-slate-900 text-slate-900" : "border-transparent text-slate-500 hover:text-slate-700 hover:border-slate-300"
            }`}
          >
            {tab.label}
          </Link>
        );
      })}
    </nav>
  );
}
