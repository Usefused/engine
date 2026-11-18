import { useEffect, useId, useState, type KeyboardEvent } from "react";
import { ChevronRight } from "lucide-react";
import { Select } from "~/components/forms/Select";
import type { IntegrationObject, Service, WebhookObject } from "~/lib/api";

type CapabilityTab = "operations" | "events";
interface CapabilityRow { id: string; name: string; description: string; method?: string; path?: string }
interface CompactServiceData { service: Service; integrations: IntegrationObject[]; webhooks: WebhookObject[]; serviceVersions: Array<{ id: string; name: string; header_value?: string }> }
interface Props {
  item: CompactServiceData; selectedOperations: Set<string>; selectedEvents: Set<string>;
  version: string; loaded: boolean; loading: boolean; allowEvents?: boolean;
  expanded?: boolean; onToggleExpanded?: () => void;
  preferEventsVersion?: number; eventNotice?: string;
  onLoad: () => void; onVersion: (version: string) => void;
  onOperation: (id: string) => void; onEvent: (id: string) => void;
  onResource: (id: string, name: string) => void; onMore: (name: string) => void;
  resourceLoading: Record<string, boolean>; resourceMore: Record<string, boolean>;
}

/** Summarizes selected operations and events so collapsed service headers still show active scope. */
function selectionSummary(operations: number, events: number, allowEvents?: boolean): string {
  const parts: string[] = [];
  // Omit zero counts so unselected services stay uncluttered in the collapsed list.
  if (operations > 0) parts.push(`${operations} ${operations === 1 ? "op" : "ops"}`);
  if (allowEvents && events > 0) parts.push(`${events} ${events === 1 ? "event" : "events"}`);
  return parts.join(" · ");
}

/** Renders the service heading, selection summary, optional collapse toggle, and inline version picker. */
function CompactServiceHeader({ props, id, isExpanded, summary }: { props: Props; id: string; isExpanded: boolean; summary: string }) {
  const { item } = props;
  const title = <>
    <h4 title={item.service.canonical_ref ?? undefined} className="min-w-0 truncate text-sm font-semibold text-slate-900">{item.service.name}</h4>
    {summary && <span className="shrink-0 text-xs font-normal text-slate-400">{summary}</span>}
  </>;
  return <div className="flex min-w-0 flex-wrap items-center justify-between gap-2">
    {/* Keep the toggle button separate from the version selector so changing versions never collapses the service. */}
    {props.onToggleExpanded ? <button type="button" aria-expanded={isExpanded} aria-controls={`${id}-body`} onClick={props.onToggleExpanded} className="flex min-w-0 flex-1 items-center gap-2 text-left">
      <ChevronRight className={`h-4 w-4 shrink-0 text-slate-400 transition-transform ${isExpanded ? "rotate-90" : ""}`} aria-hidden="true" />
      {title}
    </button> : <div className="flex min-w-0 flex-1 items-center gap-2">{title}</div>}
    {/* Version belongs beside the service identity, without a separate configuration card. */}
    {item.serviceVersions.length > 0 && <Select aria-label={`${item.service.name} version`} density="compact" value={props.version} onChange={/** Pin every subsequent operation/resource read to the explicitly chosen immutable version. */ (event) => props.onVersion(event.target.value)} className="w-28">
      {item.serviceVersions.map(/** Versions remain exact Registry identities rather than labels resolved at deployment time. */ (version) => <option key={version.id} value={version.id}>{version.header_value || version.name}</option>)}
    </Select>}
  </div>;
}

/** Renders the capability tabs and rows inside an expanded service block. */
function CompactServiceBody({ props, id, tab, query, onTab, onQuery, onNavigate }: {
  props: Props; id: string; tab: CapabilityTab; query: string;
  onTab: (tab: CapabilityTab) => void; onQuery: (query: string) => void;
  onNavigate: (event: KeyboardEvent<HTMLDivElement>) => void;
}) {
  const { item } = props;
  // Catalogue rows load their contract explicitly; hydrated source selections are already ready to edit.
  if (!props.loaded) {
    return <button type="button" disabled={props.loading} onClick={props.onLoad} className="mt-2 text-xs font-medium text-[var(--brand-violet)] disabled:opacity-50">{props.loading ? "Loading capabilities…" : "Choose capabilities"}</button>;
  }
  return <div className="mt-2 space-y-2">
    <div role="tablist" aria-label={`${item.service.name} capability type`} onKeyDown={onNavigate} className="flex gap-4 border-b border-slate-200">
      <CapabilityTabButton id={id} value="operations" active={tab} label="Operations" count={item.service.endpoint_count ?? item.integrations.length} selected={props.selectedOperations.size} onSelect={onTab} />
      {/* Event selection is available only to callers that explicitly enabled provider ingress capabilities. */}
      {props.allowEvents && <CapabilityTabButton id={id} value="events" active={tab} label="Events" count={item.service.webhook_count ?? item.webhooks.length} selected={props.selectedEvents.size} onSelect={onTab} />}
    </div>
    <CapabilityOptions settings={props} id={id} tab={tab} query={query} onQuery={onQuery} />
  </div>;
}

/** Presents exact-version capabilities in a collapsible service block with flat capability tabs inside. */
export function CompactServiceCapabilities(props: Props) {
  const { item } = props;
  const isExpanded = props.expanded ?? true;
  const preferEvents = Boolean(props.allowEvents && props.preferEventsVersion);
  // Open directly on Events when this service was focused from a webhook registration choice.
  const [tab, setTab] = useState<CapabilityTab>(preferEvents ? "events" : "operations");
  const [query, setQuery] = useState("");
  const id = useId();
  const summary = selectionSummary(props.selectedOperations.size, props.selectedEvents.size, props.allowEvents);
  // Re-choosing a webhook for an already-mounted service switches it directly to Events without altering selections.
  useEffect(() => {
    if (preferEvents) {
      setTab("events");
      setQuery("");
    }
  }, [preferEvents, props.preferEventsVersion]);
  /** Keeps keyboard and pointer tab selection on the same visible list without carrying an unrelated search across categories. */
  function selectTab(next: CapabilityTab) { setTab(next); setQuery(""); }
  /** Provides native tab-list navigation while keeping one category in the page's tab order. */
  function navigate(event: KeyboardEvent<HTMLDivElement>) {
    const tabs = Array.from(event.currentTarget.querySelectorAll<HTMLButtonElement>('[role="tab"]'));
    const current = tabs.indexOf(event.target as HTMLButtonElement);
    // Only tab buttons own category navigation; ordinary search and checkbox keys remain native.
    if (current < 0) return;
    const direction: Record<string, number> = { ArrowRight: 1, ArrowLeft: -1, Home: -current, End: tabs.length - 1 - current };
    // Unhandled keys retain their normal browser behavior.
    if (!(event.key in direction)) return;
    event.preventDefault();
    const target = tabs[(current + direction[event.key] + tabs.length) % tabs.length];
    target.click(); target.focus();
  }
  return <section aria-label={`${item.service.name} capabilities`} className="min-w-0 border-t border-slate-200 py-3 first:border-t-0 first:pt-0">
    <CompactServiceHeader props={props} id={id} isExpanded={isExpanded} summary={summary} />
    {/* Only the active service renders its capability list; collapsed services keep their selections intact. */}
    {isExpanded && <div id={`${id}-body`}><CompactServiceBody props={props} id={id} tab={tab} query={query} onTab={selectTab} onQuery={setQuery} onNavigate={navigate} /></div>}
  </section>;
}

/** Keeps search and direct checkbox rows independent of the service header and category navigation. */
function CapabilityOptions({ settings: props, id, tab, query, onQuery }: { settings: Props; id: string; tab: CapabilityTab; query: string; onQuery: (query: string) => void }) {
  const { item } = props;
  // Events remain distinct from operations so switching tabs never widens a source app's permissions.
  const rows: CapabilityRow[] = tab === "operations" ? item.integrations : item.webhooks;
  const selected = tab === "operations" ? props.selectedOperations : props.selectedEvents;
  const toggle = tab === "operations" ? props.onOperation : props.onEvent;
  const filtered = rows.filter(/** Search only the currently visible exact-version capabilities. */ (row) => [row.name, row.path, row.description, row.method].join(" ").toLowerCase().includes(query.trim().toLowerCase()));
  return <div role="tabpanel" id={`${id}-${tab}-panel`} aria-labelledby={`${id}-${tab}-tab`} className="space-y-2">
        {/* Surface attachment coverage mismatches inside Events before the author attempts to compile. */}
        {tab === "events" && props.eventNotice && <p role="note" className="rounded-md border border-amber-200 bg-amber-50 px-2.5 py-2 text-xs text-amber-900">{props.eventNotice}</p>}
        {/* A single plain search field replaces the nested resource/search card hierarchy. */}
        {rows.length > 6 && <input aria-label={`Search ${tab} for ${item.service.name}`} value={query} onChange={/** Search affects presentation only; selected IDs and exact-version pins remain unchanged. */ (event) => onQuery(event.target.value)} placeholder={`Search ${tab}…`} className="h-8 w-full rounded-md border border-slate-200 px-2 text-xs" />}
        <div className="max-h-64 overflow-y-auto divide-y divide-slate-100">
          {filtered.map(/** Every checkbox changes exactly one capability without a resource-level bulk grant. */ (row) => <label key={row.id} title={row.description} className="flex min-w-0 cursor-pointer items-center gap-2 rounded px-1 py-2 text-xs hover:bg-slate-50">
            <input type="checkbox" aria-label={row.name} checked={selected.has(row.id)} onChange={/** Selected IDs flow through the existing version-aware builder state. */ () => toggle(row.id)} className="h-3.5 w-3.5 shrink-0 rounded border-slate-300 accent-[var(--brand-violet)]" />
            <span className="min-w-0 flex-1 truncate font-medium text-slate-700">{row.name}</span>
            {/* Operation routing stays visible without another expanded details block. */}
            {row.path && <span className="min-w-0 max-w-[55%] truncate font-mono text-[10px] text-slate-400">{row.method} {row.path}</span>}
          </label>)}
        </div>
        {/* Unloaded resource pages remain explicit, flat actions rather than nested accordion headings. */}
        {tab === "operations" && <CompactResourceActions {...props} />}
        {/* An empty category is a readable terminal state, not an invisible disabled accordion. */}
        {filtered.length === 0 && <p className="py-2 text-xs text-slate-400">No {query ? "matching " : ""}{tab} available.</p>}
      </div>;
}

/** Shares accessible category buttons while keeping counts beside the category instead of nested section headers. */
function CapabilityTabButton({ id, value, active, label, count, selected, onSelect }: { id: string; value: CapabilityTab; active: CapabilityTab; label: string; count: number; selected: number; onSelect: (tab: CapabilityTab) => void }) {
  // The active category alone participates in the tab sequence and owns the visible underline.
  const current = active === value;
  return <button type="button" role="tab" id={`${id}-${value}-tab`} aria-controls={`${id}-${value}-panel`} aria-selected={current} tabIndex={current ? 0 : -1} onClick={/** Category changes do not mutate the selected provider capabilities. */ () => onSelect(value)} className={`border-b-2 py-1.5 text-xs font-medium ${current ? "border-slate-900 text-slate-900" : "border-transparent text-slate-400 hover:text-slate-700"}`}>{label} <span className="text-slate-400">{selected}/{count}</span></button>;
}

/** Keeps catalogue resource loading and pagination reachable without restoring the removed nesting. */
function CompactResourceActions(props: Props) {
  return <div className="flex flex-wrap gap-2">
    {props.item.service.resources?.map(/** Each resource action retains the existing immutable-version loader and page offset. */ (resource) => {
      const loaded = props.item.integrations.some(/** Resource identity is stable even when display names repeat in another service. */ (operation) => operation.resource_id === resource.id || operation.resource === resource.name);
      const more = props.resourceMore[resource.id];
      // Fully loaded resources need no extra control below their operation rows.
      if (loaded && !more) return null;
      return <button key={resource.id} type="button" disabled={props.resourceLoading[resource.name]} onClick={/** Continue a loaded page or explicitly fetch an unloaded resource, using the same selected service version. */ () => { if (loaded) props.onMore(resource.name); else props.onResource(resource.id, resource.name); }} className="text-xs text-[var(--brand-violet)] disabled:opacity-50">{loaded ? "More" : "Load"} {resource.name} operations</button>;
    })}
  </div>;
}
