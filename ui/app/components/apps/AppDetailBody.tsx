import type { ReactNode } from "react";
import { NestedActivityTabs, type NestedActivityTabOption } from "~/components/activity/NestedActivityTabs";
import { AppConnectedServices } from "~/components/apps/AppConnectedServices";
import { AppVersionHistory, type AppVersionHistoryItem } from "~/components/apps/AppVersionHistory";
import type { AppConnectedServiceSelection } from "~/lib/app-connected-services";

interface AppDetailBodyProps {
  children: ReactNode;
}

interface AppDetailSectionProps {
  title: string;
  children: ReactNode;
}

interface AppOverviewBodyProps {
  selections: AppConnectedServiceSelection[];
  adapterDetails?: ReactNode;
}

interface AppActivityBodyProps<T extends string> {
  active: T;
  ariaLabel: string;
  options: Array<NestedActivityTabOption<T>>;
  onChange: (tab: T) => void;
  intro?: ReactNode;
  children: ReactNode;
}

interface AppChangesBodyProps<T extends AppVersionHistoryItem> {
  versions: T[];
  currentId: string;
  canDelete: boolean;
  deletingVersionId: string;
  onSelect: (id: string) => void;
  onDelete: (version: T) => void;
  renderDetails?: (version: T) => ReactNode;
}

/** Applies one body width and spacing contract to every immutable app detail page. */
export function AppDetailBody({ children }: AppDetailBodyProps) {
  return <div className="min-w-0 max-w-full p-1">{children}</div>;
}

/** Renders a shared detail subsection while leaving its contents adapter-owned. */
export function AppDetailSection({ title, children }: AppDetailSectionProps) {
  return (
    <section>
      <h2 className="mb-3 text-sm font-semibold uppercase tracking-wider text-slate-700">{title}</h2>
      {children}
    </section>
  );
}

/** Keeps immutable connected-service details in the same position for every app adapter. */
export function AppOverviewBody({ selections, adapterDetails }: AppOverviewBodyProps) {
  return (
    <div className="space-y-7">
      {adapterDetails}
      <AppDetailSection title="Connected services">
        <AppConnectedServices selections={selections} />
      </AppDetailSection>
    </div>
  );
}

/** Applies one nested Activity layout while adapters supply only their extra activity sections. */
export function AppActivityBody<T extends string>({ active, ariaLabel, options, onChange, intro, children }: AppActivityBodyProps<T>) {
  return (
    <div className="min-w-0 max-w-full space-y-5 overflow-x-hidden sm:space-y-6">
      {intro}
      <NestedActivityTabs active={active} ariaLabel={ariaLabel} onChange={onChange} options={options} />
      {children}
    </div>
  );
}

/** Renders the family Changes surface with the same lifecycle controls for every adapter. */
export function AppChangesBody<T extends AppVersionHistoryItem>({ versions, currentId, canDelete, deletingVersionId, onSelect, onDelete, renderDetails }: AppChangesBodyProps<T>) {
  return (
    <AppVersionHistory
      versions={versions}
      currentId={currentId}
      canDelete={canDelete}
      deletingVersionId={deletingVersionId}
      onSelect={onSelect}
      onDelete={onDelete}
      renderDetails={renderDetails}
    />
  );
}
