import { Select } from "../forms/Select.ts";
import type { ReactNode } from "react";
import type { AppVersionHistoryItem } from "./AppVersionHistory";

interface AppVersionSelectProps {
  versions: AppVersionHistoryItem[];
  current: AppVersionHistoryItem;
  activeId: string | null;
  busy: boolean;
  canDelete: boolean;
  actions: ReactNode;
  onSelect: (id: string) => void;
  onDelete: (version: AppVersionHistoryItem) => void;
}

/** Selects a readable version label while keeping exact identifiers behind navigation and mutations. */
export function AppVersionSelect({ versions, current, activeId, busy, canDelete, actions, onSelect, onDelete }: AppVersionSelectProps) {
  return <section className="overflow-hidden rounded-xl border border-slate-200 bg-white">
    <div className="space-y-4 p-4 sm:p-5">
      <div><h2 className="text-sm font-semibold text-slate-900">Versions</h2><p className="mt-1 text-sm text-slate-500">Select a version to view its details and activity, or make it active.</p></div>
      <div className="flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
        <label className="block w-full text-sm font-medium text-slate-700 sm:max-w-xs">Version
          <Select aria-label="App version" value={current.id} disabled={busy} onChange={(event) => onSelect(event.target.value)} className="mt-1.5 h-11 w-full rounded-lg border border-slate-300 bg-white px-3 text-sm text-slate-900 focus:outline-none focus:ring-2 focus:ring-[var(--brand-violet)] disabled:opacity-50">
            {/* The label communicates traffic state without exposing the underlying UUID. */}
            {versions.map((version) => <option key={version.id} value={version.id}>{version.version}{version.id === activeId ? " · Receiving traffic" : ""}</option>)}
          </Select>
        </label>
        <p className="text-xs text-slate-500 sm:pb-3">Created {new Date(current.created_at).toLocaleDateString()}</p>
      </div>
    </div>
    <div className="flex flex-wrap items-center justify-between gap-3 border-t border-slate-100 bg-slate-50/60 px-4 py-3 sm:px-5">
      {actions}
      {/* Only managers may delete the exact version selected above. */}
      {canDelete && <button type="button" disabled={busy} onClick={() => onDelete(current)} className="ml-auto min-h-11 rounded-lg px-3 text-sm font-medium text-slate-950 hover:bg-slate-50 disabled:opacity-50">Delete version</button>}
    </div>
  </section>;
}
