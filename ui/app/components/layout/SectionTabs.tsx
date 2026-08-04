import { Link, useLocation } from "@remix-run/react";

export type SectionTab = {
  label: string;
  to: string;
  // Defaults to a pathname-prefix match against `to` when omitted.
  isActive?: (pathname: string) => boolean;
};

// Shared tab row used to present several existing routes as one grouped
// section (e.g. Consumers = SDK History + MCP Servers, Access = People +
// Teams + Buckets), without merging their routes, data loading, or API
// calls. Purely a navigation affordance -- each tab is a real Link to its
// own route.
export function SectionTabs({ tabs }: { tabs: SectionTab[] }) {
  const location = useLocation();
  return (
    <div className="flex items-center gap-1 border-b border-slate-200 mb-6 -mt-1">
      {tabs.map((tab) => {
        const isActive = tab.isActive ? tab.isActive(location.pathname) : location.pathname.startsWith(tab.to);
        return (
          <Link
            key={tab.to}
            to={tab.to}
            className={`px-4 py-2.5 text-sm font-medium border-b-2 -mb-px transition-colors ${
              isActive
                ? "border-slate-900 text-slate-900"
                : "border-transparent text-slate-500 hover:text-slate-700 hover:border-slate-300"
            }`}
          >
            {tab.label}
          </Link>
        );
      })}
    </div>
  );
}
