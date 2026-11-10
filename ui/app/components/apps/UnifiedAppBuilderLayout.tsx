import { useId, type KeyboardEvent, type ReactNode } from "react";

export type UnifiedAppBuilderTab = "config" | "code";

/** Keeps configuration and source in one compact workspace without remounting either editor on tab changes. */
export function UnifiedAppBuilderLayout({ tab, onTabChange, config, code, tools, footer }: {
  tab: UnifiedAppBuilderTab;
  onTabChange: (tab: UnifiedAppBuilderTab) => void;
  config: ReactNode;
  code: ReactNode;
  tools?: ReactNode;
  footer?: ReactNode;
}) {
  const id = useId();

  /** Arrow and boundary keys move focus together with selection, following the standard tab keyboard model. */
  function navigateTabs(event: KeyboardEvent<HTMLDivElement>) {
    // Other keys retain normal focus and activation behavior.
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    // With two tabs either arrow moves to the other panel; boundary keys select their named edge.
    const next = event.key === "Home" ? "config" : event.key === "End" ? "code" : tab === "config" ? "code" : "config";
    onTabChange(next);
    event.currentTarget.querySelector<HTMLButtonElement>(`[id="${id}-${next}-tab"]`)?.focus();
  }

  return <section aria-label="Unified App builder" className="min-w-0 rounded-xl border border-slate-200 bg-white">
    <div className="flex flex-wrap items-center justify-between gap-2 border-b border-slate-200 px-3 py-2">
      <div role="tablist" aria-label="App editor" onKeyDown={navigateTabs} className="inline-flex gap-1 rounded-lg bg-slate-100 p-1">
        {/* Stable tab and panel identities let assistive technology follow selection without discarding editor state. */}
        {(["config", "code"] as const).map(/** Each tab owns one persistent panel and only the active tab enters the tab order. */ (value) => <button key={value} id={`${id}-${value}-tab`} type="button" role="tab" aria-selected={tab === value} aria-controls={`${id}-${value}-panel`} tabIndex={tab === value ? 0 : -1} onClick={/** Selecting a panel changes presentation without invalidating the reviewed draft. */ () => onTabChange(value)} className="rounded-md px-4 py-1.5 text-sm font-medium text-slate-500 hover:text-slate-900 aria-selected:bg-white aria-selected:text-slate-950 aria-selected:shadow-sm focus-visible:outline-2 focus-visible:outline-violet-500">
          {/* Labels stay short so the same toolbar fits narrow screens. */}
          {value === "config" ? "Config" : "Code"}
        </button>)}
      </div>
      {tools}
    </div>
    <div id={`${id}-config-panel`} role="tabpanel" aria-labelledby={`${id}-config-tab`} hidden={tab !== "config"} className="min-w-0 p-4">{config}</div>
    <div id={`${id}-code-panel`} role="tabpanel" aria-labelledby={`${id}-code-tab`} hidden={tab !== "code"} className="min-w-0 p-3">{code}</div>
    {/* A shared footer makes validation, errors, and deployment available from either authoring view. */}
    {footer && <footer className="sticky bottom-0 z-10 rounded-b-xl border-t border-slate-200 bg-white/95 px-4 py-3 backdrop-blur">{footer}</footer>}
  </section>;
}
