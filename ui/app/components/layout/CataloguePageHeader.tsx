import type { ComponentType, ReactNode } from "react";
import type { LucideProps } from "lucide-react";

export const catalogueActionClassName = "inline-flex h-9 shrink-0 items-center justify-center gap-2 whitespace-nowrap rounded-md border border-slate-950 bg-slate-950 px-2.5 sm:px-3 text-sm font-medium text-white shadow-sm transition-colors hover:bg-slate-800 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-slate-950";

type CataloguePageHeaderProps = {
  title: string;
  icon: ComponentType<LucideProps>;
  description: string;
  actions?: ReactNode;
  children?: ReactNode;
};

/** Keeps titles and right-aligned creation actions together across every catalogue page and viewport. */
export function CataloguePageHeader({ title, icon: Icon, description, actions, children }: CataloguePageHeaderProps) {
  return <header className="space-y-3">
    <div className="flex min-w-0 items-center justify-between gap-2 sm:gap-4">
      <div className="flex min-w-0 items-center gap-2 sm:gap-3">
        <span className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-slate-100 text-slate-600 sm:h-10 sm:w-10"><Icon className="h-5 w-5" aria-hidden="true" /></span>
        <h1 className="min-w-0 text-lg font-semibold text-slate-900 sm:text-xl">{title}</h1>
      </div>
      {actions}
    </div>
    <p className="text-sm text-slate-600">{description}</p>
    {children}
  </header>;
}
