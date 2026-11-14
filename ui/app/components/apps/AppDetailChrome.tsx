import type { ReactNode } from "react";
import { Link } from "@remix-run/react";
import { Code2, Database, Globe2, Layers3, TerminalSquare, Webhook } from "lucide-react";
import { PageBackLink } from "~/components/layout/PageBackLink";
import { AppRuntimeStatus } from "~/components/apps/AppRuntimeStatus";

export interface AppDetailVersion {
  id: string;
  version: string;
}

export interface AppDetailTab<T extends string> {
  value: T;
  label: string;
}

const detailKinds = {
  sdk: { label: "SDK", Icon: Code2, summary: "A reusable interface for the services and operations this app can use." },
  api: { label: "REST API", Icon: Globe2, summary: "A reusable interface for the services and operations this app can use." },
  mcp: { label: "MCP", Icon: TerminalSquare, summary: "A reusable interface for the services and operations this MCP server exposes." },
  unified_app: { label: "Unified App", Icon: Layers3, summary: "Combine approved services into one typed action for onboarding, fulfillment, or reporting." },
  webhook: { label: "Webhook", Icon: Webhook, summary: "Receive service events and inspect deliveries for this webhook registration." },
};

interface AppDetailHeaderProps {
  kind: keyof typeof detailKinds;
  name: string;
  summary?: string;
  status?: string;
  version?: string;
  language?: string;
  bucket?: { id: string; name: string } | null;
  service?: { name: string; to: string };
  createdAt?: string;
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

/** Owns the identity layout and metadata design for every app and webhook detail page. */
export function AppDetailHeader({ name, summary, status, action, ...metadata }: AppDetailHeaderProps) {
  return (
    <header className="flex min-w-0 flex-col justify-between gap-4 md:flex-row md:items-start">
      <div className="min-w-0">
        <h1 className="break-words text-2xl font-bold text-slate-900">{name}</h1>
        <p className="mt-1 text-slate-500">{summary || detailKinds[metadata.kind].summary}</p>
        <AppRuntimeStatus className="mt-1.5" status={status} />
        <AppDetailMetadata {...metadata} />
      </div>
      {action}
    </header>
  );
}

/** Routes supply metadata values; ordering, icons, links, and badges have one owner. */
function AppDetailMetadata({ kind, version, language, bucket, service, createdAt }: Omit<AppDetailHeaderProps, "name">) {
  const { label, Icon } = detailKinds[kind];
  const languageNames: Record<string, string> = { typescript: "TypeScript", python: "Python", go: "Go" };
  return (
    <div className="mt-3 flex flex-wrap items-center gap-3 text-sm">
      {version && <span className="rounded bg-slate-100 px-2.5 py-1 font-medium text-slate-700">{version}</span>}
      <span className="inline-flex items-center gap-1.5 text-[var(--brand-violet)]"><Icon className="h-4 w-4 shrink-0" aria-hidden="true" />{label}</span>
      {language && <span className="text-slate-600">{languageNames[language] || language}</span>}
      {bucket && <Link to={`/integrations/buckets?bucket=${encodeURIComponent(bucket.id)}`} className="inline-flex min-w-0 items-center gap-1.5 rounded border border-slate-200 bg-slate-50 px-2 py-0.5 text-slate-600 transition-colors hover:text-[var(--brand-violet)]"><Database className="h-3.5 w-3.5 shrink-0 text-slate-400" aria-hidden="true" /><span className="break-all">{bucket.name}</span></Link>}
      {service && <Link to={service.to} className="break-all text-slate-600 hover:text-[var(--brand-violet)] hover:underline">{service.name}</Link>}
      {createdAt && <time dateTime={createdAt} className="text-slate-600">Created {appCreatedDate(createdAt)}</time>}
    </div>
  );
}

/** Returns every app adapter to its owning catalogue from the shared row above page actions. */
export function AppDetailBackLink({ to, className = "" }: AppDetailBackLinkProps) {
  return <PageBackLink to={to} className={className}>Back to apps</PageBackLink>;
}

/** Applies one consistent primary-action treatment while allowing each adapter to own its behavior. */
export function AppDetailPrimaryAction({ icon, label, onClick }: AppDetailPrimaryActionProps) {
  return (
    <button type="button" onClick={onClick} className="inline-flex w-full cursor-pointer items-center justify-center gap-2 rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white shadow-sm transition-colors hover:bg-slate-800 md:w-auto">
      {icon}
      {label}
    </button>
  );
}

/** Renders scrollable immutable-version navigation and hides it for single-version families. */
export function AppVersionSwitcher({ label, versions, currentId, onSelect }: AppVersionSwitcherProps) {
  // A single immutable version needs no secondary family navigation.
  if (versions.length <= 1) return null;
  return (
    <div className="flex flex-wrap items-center gap-2">
      <span className="mr-1 text-xs font-medium uppercase tracking-wider text-slate-500">{label}</span>
      <div className="fused-tabs-scroll gap-0.5 rounded-lg bg-slate-100/80 p-1">
        {versions.map((version) => (
          <button key={version.id} type="button" onClick={() => onSelect(version.id)} className={`cursor-pointer rounded-md px-3 py-1 text-xs font-medium transition-all ${version.id === currentId ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700"}`}>
            {version.version}
          </button>
        ))}
      </div>
    </div>
  );
}

/** Keeps primary detail tabs touch-friendly on phones while retaining the compact desktop treatment. */
export function AppDetailTabs<T extends string>({ label, active, tabs, onChange }: AppDetailTabsProps<T>) {
  return (
    <nav aria-label={label} className="flex max-w-full overflow-x-auto whitespace-nowrap rounded-lg bg-slate-100/80 p-1 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
      {tabs.map((tab) => (
        <button key={tab.value} type="button" onClick={() => onChange(tab.value)} className={`shrink-0 cursor-pointer rounded-md px-3 py-3 text-sm font-medium transition-all sm:px-4 sm:py-1.5 ${tab.value === active ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700"}`}>
          {tab.label}
        </button>
      ))}
    </nav>
  );
}
