import { Link, useLocation } from "@remix-run/react";

export type SectionTab = {
  label: string;
  to: string;
  // Defaults to a pathname-prefix match against `to` when omitted.
  isActive?: (pathname: string) => boolean;
};

/** Keeps route navigation accessible while distinguishing outer segmented navigation from inner underlined sections. */
export function SectionTabs({ tabs, label = "Section navigation", variant = "underline" }: { tabs: SectionTab[]; label?: string; variant?: "underline" | "segmented" }) {
  const location = useLocation();
  // Existing section consumers retain their underline treatment; integration sources opt into the outer level.
  const segmented = variant === "segmented";
  return (
    <nav aria-label={label} className={`fused-tabs-scroll mb-6 items-center gap-1 ${
      // The outer strip groups destinations, while the inner strip aligns its active underline with the content edge.
      segmented ? "bg-slate-100 p-1 rounded-lg w-full sm:w-fit" : "-mt-1 border-b border-slate-200"
    }`}>
      {tabs.map((tab) => {
        // Custom route matching stays authoritative for sections with nested destinations.
        const isActive = tab.isActive ? tab.isActive(location.pathname) : location.pathname.startsWith(tab.to);
        return (
          <Link
            key={tab.to}
            to={tab.to}
            // Expose the current destination to screen readers as well as visually.
            aria-current={isActive ? "page" : undefined}
            className={`flex shrink-0 items-center whitespace-nowrap text-sm font-medium transition-colors focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-slate-950 ${
              // Each level keeps stable dimensions when selection moves between destinations.
              segmented ? "rounded-md px-3 sm:px-4 py-1.5" : "min-h-11 border-b-2 px-4 py-2.5"
            } ${
              // Filled selection identifies the outer route without borrowing the inner underline design.
              segmented
                ? (isActive ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700 hover:bg-slate-200/50")
                : (isActive ? "border-slate-900 text-slate-900" : "border-transparent text-slate-500 hover:text-slate-700 hover:border-slate-300")
            }`}
          >
            {tab.label}
          </Link>
        );
      })}
    </nav>
  );
}
