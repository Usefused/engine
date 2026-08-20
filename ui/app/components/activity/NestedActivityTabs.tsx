export interface NestedActivityTabOption<T extends string> {
  value: T;
  label: string;
  badge?: number;
  trackingId?: string;
}

interface NestedActivityTabsProps<T extends string> {
  active: T;
  ariaLabel: string;
  onChange: (value: T) => void;
  options: Array<NestedActivityTabOption<T>>;
}

// Nested views use an underline so they remain visually subordinate to the
// filled primary tabs that select the parent page section.
export function NestedActivityTabs<T extends string>({ active, ariaLabel, onChange, options }: NestedActivityTabsProps<T>) {
  return (
    <div className="max-w-full overflow-x-auto overscroll-x-contain border-b border-slate-200 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
      <div className="flex min-w-max touch-pan-x gap-4 sm:gap-6" role="tablist" aria-label={ariaLabel}>
        {options.map((option) => (
          <button
            key={option.value}
            type="button"
            role="tab"
            aria-selected={active === option.value}
            data-track={option.trackingId}
            onClick={() => onChange(option.value)}
            className={`flex shrink-0 items-center gap-2 border-b-2 px-0.5 pb-3 text-sm font-medium transition-colors ${
              active === option.value
                ? "border-slate-900 text-slate-900"
                : "border-transparent text-slate-500 hover:text-slate-800"
            }`}
          >
            {option.label}
            {option.badge ? <span className="rounded-full bg-slate-100 px-2 py-0.5 text-xs text-slate-600">{option.badge}</span> : null}
          </button>
        ))}
      </div>
    </div>
  );
}
