import { useId, useState, type KeyboardEvent } from "react";
import { Select } from "~/components/forms/Select";
import type { AppBuildSelector } from "~/lib/app-builder-contract";

const runtimeChoice = "__fused_runtime_bucket__";

/** Keeps fixed routing and runtime opt-in in the same bucket field without changing the family's default binding. */
export function UnifiedAppBucketScope({ value, buckets, bucket, lockedBucketName, onBucketChange, onChange }: {
  value: string[];
  buckets: AppBuildSelector[];
  bucket: string;
  lockedBucketName?: string;
  onBucketChange: (bucket: string) => void;
  onChange: (names: string[]) => void;
}) {
  const dynamic = value.length > 0;
  // Stable option order prevents checks from jumping; saved names remain reviewable after access changes.
  const names = [...new Set([...buckets.map((item) => item.display_name), ...value])];
  const defaultName = lockedBucketName ?? buckets.find((item) => item.resource_id === bucket)?.display_name;

  /** Enabling starts with the configured bucket when available; a fixed choice explicitly removes runtime scope. */
  function choose(selection: string) {
    // Runtime selection needs an exact candidate rather than an empty or wildcard scope.
    if (selection === runtimeChoice) {
      const candidate = defaultName ?? buckets[0]?.display_name;
      // Empty selector results cannot accidentally enable an unbounded capability.
      if (candidate) onChange([candidate]);
      return;
    }
    onChange([]);
    // Successor versions retain their family's default while allowing runtime scope changes.
    if (lockedBucketName === undefined) onBucketChange(selection);
  }

  /** Reuses the same default choices so switching modes never invents a second credential binding. */
  function defaultOptions() {
    // Existing families expose their immutable default even when it is absent from current selectors.
    if (lockedBucketName !== undefined) return <option value={bucket}>{lockedBucketName}</option>;
    return <><option value="">Choose a bucket</option>{buckets.map((item) => <option key={item.resource_id} value={item.resource_id}>{item.display_name}</option>)}</>;
  }

  return <div className="rounded-lg border border-slate-300 bg-white focus-within:border-violet-400">
    <Select id="unified-credential-bucket" required className="w-full rounded-lg border-0 px-3 py-2.5 text-sm focus-visible:outline-offset-[-2px]" value={dynamic ? runtimeChoice : bucket} onChange={(event) => choose(event.target.value)}>
      {defaultOptions()}
      <option value={runtimeChoice} disabled={!names.length && !defaultName}>Choose at runtime</option>
    </Select>
    {/* Runtime details stay attached to the bucket field and disappear for a fixed selection. */}
    {dynamic && <div className="space-y-3 rounded-b-lg border-t border-slate-200 bg-slate-50/60 px-3 py-3 text-sm font-normal" aria-label="Runtime bucket scope">
      <RuntimeBucketTags value={value} names={buckets.map((item) => item.display_name)} onChange={onChange} />
      <label className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 border-t border-slate-200 pt-3"><span className="text-xs text-slate-600">Default bucket <span className="text-slate-400">· when none is selected</span></span>
        <Select aria-label="Default bucket" required className="w-full rounded-md border-slate-200 bg-white text-sm sm:w-48" value={bucket} disabled={lockedBucketName !== undefined} onChange={(event) => onBucketChange(event.target.value)}>{defaultOptions()}</Select>
      </label>
      <p className="text-xs leading-5 text-slate-500">Each run can use a permitted bucket. Access is checked on every execution.</p>
    </div>}
  </div>;
}

/** Presents the finite bucket allowlist as searchable tags while preserving native keyboard focus. */
function RuntimeBucketTags({ value, names, onChange }: { value: string[]; names: string[]; onChange: (names: string[]) => void }) {
  const id = useId();
  const [query, setQuery] = useState("");
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const matches = [...new Set(names)].filter((name) => !value.includes(name) && name.toLowerCase().includes(query.toLowerCase()));
  const activeIndex = Math.min(active, Math.max(0, matches.length - 1));
  const expanded = open && value.length < 32;

  /** Adds only an available exact identity and keeps repeated selection from duplicating a tag. */
  function add(name: string) {
    // Typed text is a filter, never authority to invent a bucket or broaden the bounded scope.
    if (!names.includes(name) || value.includes(name) || value.length >= 32) return;
    onChange([...value, name]);
    setQuery(""); setActive(0); setOpen(false);
  }

  /** Removing tags narrows scope without silently disabling the runtime mode on its last member. */
  function remove(name: string) {
    // The main bucket dropdown owns the transition back to fixed routing.
    if (value.length <= 1) return;
    onChange(value.filter((item) => item !== name));
  }

  /** Supports suggestion navigation and ordinary tag-input keys without submitting the app form. */
  function keyDown(event: KeyboardEvent<HTMLInputElement>) {
    // Arrow keys navigate only the current authorized, filtered options.
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault(); setOpen(true);
      // Empty results have no active descendant or wraparound arithmetic.
      if (matches.length) setActive((activeIndex + (event.key === "ArrowDown" ? 1 : matches.length - 1)) % matches.length);
      return;
    }
    // Enter commits a suggestion; unknown input never becomes an allowed bucket name.
    if (event.key === "Enter") {
      event.preventDefault();
      // Hidden suggestions require opening before Enter can add anything unexpectedly.
      if (expanded && matches[activeIndex]) add(matches[activeIndex]);
      else setOpen(true);
      return;
    }
    // Escape dismisses suggestions without affecting existing tags.
    if (event.key === "Escape") { event.preventDefault(); setOpen(false); return; }
    // Empty-input Backspace follows standard tag editing while retaining one permitted bucket.
    if (event.key === "Backspace" && !query && value.length > 1) { event.preventDefault(); remove(value[value.length - 1]); }
  }

  return <div className="relative space-y-2">
    <label htmlFor={id} className="block text-xs font-medium text-slate-700">Permitted buckets</label>
    <div className="flex min-h-11 flex-wrap items-center gap-1.5 rounded-md border border-slate-200 bg-white p-1.5 focus-within:border-violet-400 focus-within:ring-2 focus-within:ring-violet-100">
      {value.map((name) => <span key={name} className="inline-flex max-w-full items-center gap-1 rounded bg-violet-50 py-1 pl-2 pr-1 text-xs font-medium text-violet-900">
        <span className="truncate" title={name}>{name}</span>
        <button type="button" aria-label={`Remove ${name}`} disabled={value.length === 1} title="Keep at least one permitted bucket" onClick={() => remove(name)} className="flex size-5 shrink-0 items-center justify-center rounded text-violet-500 hover:bg-violet-100 hover:text-violet-900 disabled:opacity-30">
          <svg aria-hidden="true" viewBox="0 0 16 16" className="size-3" fill="none" stroke="currentColor" strokeWidth="1.5"><path d="m4 4 8 8M12 4l-8 8" /></svg>
        </button>
      </span>)}
      <input id={id} role="combobox" aria-autocomplete="list" aria-expanded={expanded} aria-controls={`${id}-options`} aria-activedescendant={expanded && matches.length ? `${id}-option-${activeIndex}` : undefined}
        autoComplete="off" placeholder="Add a bucket…" value={query} disabled={value.length >= 32}
        onFocus={() => setOpen(true)} onBlur={() => setOpen(false)} onChange={(event) => { setQuery(event.target.value); setActive(0); setOpen(true); }} onKeyDown={keyDown}
        className="min-w-24 flex-1 border-0 bg-transparent px-1 py-1 text-sm text-slate-700 outline-none placeholder:text-slate-400 focus:ring-0" />
    </div>
    {/* Suggestions never include selected tags or arbitrary typed names. */}
    {expanded && <div className="absolute inset-x-0 top-full z-20 mt-1 overflow-hidden rounded-lg border border-slate-200 bg-white p-1 shadow-lg">
      <div id={`${id}-options`} role="listbox" aria-label="Available buckets" className="max-h-40 overflow-y-auto">
        {matches.map((name, index) => <button key={name} type="button" id={`${id}-option-${index}`} role="option" aria-selected={index === activeIndex}
          onMouseDown={(event) => event.preventDefault()} onClick={() => add(name)}
          className="block w-full truncate rounded px-3 py-2 text-left text-sm text-slate-700 hover:bg-violet-50 aria-selected:bg-violet-50 aria-selected:text-violet-900">{name}</button>)}
      </div>
      {/* Empty search results explain why Enter cannot grant a new or unavailable bucket. */}
      {!matches.length && <p role="status" className="px-3 py-2 text-xs text-slate-500">No more matching buckets available.</p>}
    </div>}
  </div>;
}
