import type { ReactNode } from "react";
import { Link } from "@remix-run/react";
import { ArrowLeft } from "lucide-react";
import { AppRuntimeStatus } from "~/components/apps/AppRuntimeStatus";

export interface AppDetailVersion {
  id: string;
  version: string;
}

export interface AppDetailTab<T extends string> {
  value: T;
  label: string;
}

interface AppDetailHeaderProps {
  name: string;
  summary: string;
  status: string;
  version: string;
  createdAt?: string;
  leadingMetadata?: ReactNode;
  trailingMetadata?: ReactNode;
  action?: ReactNode;
}

interface AppDetailPrimaryActionProps {
  icon: ReactNode;
  label: string;
  onClick: () => void;
}

interface AppDetailBackLinkProps {
  to: string;
  className?: string;
}

interface AppVersionSwitcherProps {
  label: string;
  versions: AppDetailVersion[];
  currentId: string;
  onSelect: (id: string) => void;
}

interface AppDetailTabsProps<T extends string> {
  label: string;
  active: T;
  tabs: Array<AppDetailTab<T>>;
  onChange: (tab: T) => void;
}

/** Formats one optional app creation date without leaking locale work into adapter routes. */
function appCreatedDate(value?: string): string {
  // Missing historical timestamps should not produce an invalid browser date.
  return value ? new Date(value).toLocaleDateString() : "";
}

/** Renders the common identity hierarchy for SDK, REST, and MCP immutable-version pages. */
export function AppDetailHeader({ name, summary, status, version, createdAt, leadingMetadata, trailingMetadata, action }: AppDetailHeaderProps) {
  return (
    <header className="flex min-w-0 flex-col justify-between gap-4 md:flex-row md:items-start">
      <div className="min-w-0">
        <h1 className="break-words text-2xl font-bold text-slate-900">{name}</h1>
        <p className="mt-1 text-slate-500">{summary}</p>
        <AppRuntimeStatus className="mt-1.5" status={status} />
        <div className="mt-3 flex flex-wrap items-center gap-3 text-sm">
          <span className="rounded bg-slate-100 px-2.5 py-1 font-medium text-slate-700">{version}</span>
          {leadingMetadata}
          {/* Creation metadata is absent only for older rows that predate the shared projection. */}
          {createdAt ? <span className="text-slate-600">Created {appCreatedDate(createdAt)}</span> : null}
          {trailingMetadata}
        </div>
      </div>
      {action}
    </header>
  );
}

/** Returns every app adapter to its owning catalogue with one consistent affordance. */
export function AppDetailBackLink({ to, className = "" }: AppDetailBackLinkProps) {
  return <Link to={to} className={`inline-flex items-center text-sm text-slate-500 transition-colors hover:text-slate-800 ${className}`}><ArrowLeft className="mr-2 h-4 w-4" />Back to apps</Link>;
}

/** Applies one consistent primary-action treatment while allowing each adapter to own its behavior. */
export function AppDetailPrimaryAction({ icon, label, onClick }: AppDetailPrimaryActionProps) {
  return (
    <button type="button" onClick={onClick} className="inline-flex w-full cursor-pointer items-center justify-center gap-2 rounded-lg bg-blue-600 px-4 py-2 text-sm font-medium text-white shadow-sm transition-colors hover:bg-blue-700 md:w-auto">
      {icon}
      {label}
    </button>
  );
}

/** Renders shared immutable-version navigation and hides it for single-version families. */
export function AppVersionSwitcher({ label, versions, currentId, onSelect }: AppVersionSwitcherProps) {
  // A single immutable version needs no secondary family navigation.
  if (versions.length <= 1) return null;
  return (
    <div className="flex flex-wrap items-center gap-2">
      <span className="mr-1 text-xs font-medium uppercase tracking-wider text-slate-500">{label}</span>
      <div className="flex flex-wrap gap-0.5 rounded-lg bg-slate-100/80 p-1">
        {versions.map((version) => (
          <button key={version.id} type="button" onClick={() => onSelect(version.id)} className={`cursor-pointer rounded-md px-3 py-1 text-xs font-medium transition-all ${version.id === currentId ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700"}`}>
            {version.version}
          </button>
        ))}
      </div>
    </div>
  );
}

/** Renders the common primary app-detail tab treatment from adapter-owned tab choices. */
export function AppDetailTabs<T extends string>({ label, active, tabs, onChange }: AppDetailTabsProps<T>) {
  return (
    <nav aria-label={label} className="flex max-w-full overflow-x-auto whitespace-nowrap rounded-lg bg-slate-100/80 p-1 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
      {tabs.map((tab) => (
        <button key={tab.value} type="button" onClick={() => onChange(tab.value)} className={`shrink-0 cursor-pointer rounded-md px-4 py-1.5 text-sm font-medium transition-all ${tab.value === active ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700"}`}>
          {tab.label}
        </button>
      ))}
    </nav>
  );
}
