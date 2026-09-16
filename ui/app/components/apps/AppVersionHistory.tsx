import { useState, type ReactNode } from "react";
import { ChevronDown, Loader2, Trash2 } from "lucide-react";

export interface AppVersionHistoryItem {
  id: string;
  version: string;
  created_at: string;
}

interface AppVersionHistoryProps<T extends AppVersionHistoryItem> {
  versions: T[];
  currentId: string;
  canDelete: boolean;
  deletingVersionId: string;
  onSelect: (id: string) => void;
  onDelete: (version: T) => void;
  renderDetails?: (version: T) => ReactNode;
}

interface AppVersionHistoryRowProps<T extends AppVersionHistoryItem> {
  version: T;
  currentId: string;
  canDelete: boolean;
  deletingVersionId: string;
  expanded: boolean;
  onToggle: (id: string) => void;
  onSelect: (id: string) => void;
  onDelete: (version: T) => void;
  renderDetails?: (version: T) => ReactNode;
}

/** Renders one version as direct navigation or an adapter-owned expandable card. */
function AppVersionHistoryRow<T extends AppVersionHistoryItem>({ version, currentId, canDelete, deletingVersionId, expanded, onToggle, onSelect, onDelete, renderDetails }: AppVersionHistoryRowProps<T>) {
  const deleting = deletingVersionId === version.id;
  // Adapter-supplied details convert this row from direct navigation into a disclosure card.
  const expandable = Boolean(renderDetails);
  return (
    <div className="text-sm">
      <div className="flex min-w-0 items-center justify-between gap-3 px-4 py-3 sm:px-5">
        {/* MCP supplies details, while SDK and REST retain direct exact-version navigation. */}
        {expandable ? (
          <button type="button" onClick={() => onToggle(version.id)} className="flex min-w-0 flex-1 items-center gap-2 text-left" aria-expanded={expanded}>
            <span className="min-w-0 break-all font-medium text-slate-800">{version.version}</span>
            {/* The marker identifies the open immutable version without implying it is the family default. */}
            {version.id === currentId ? <span className="shrink-0 rounded bg-blue-50 px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-blue-700">Current</span> : null}
          </button>
        ) : (
          <div className="flex min-w-0 items-center gap-2">
            <button type="button" onClick={() => onSelect(version.id)} className="min-w-0 break-all text-left font-medium text-slate-800 hover:text-blue-600">
              {version.version}
            </button>
            {/* The marker identifies the open immutable version without implying it is the family default. */}
            {version.id === currentId ? <span className="shrink-0 rounded bg-blue-50 px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-blue-700">Current</span> : null}
          </div>
        )}
        <div className="flex shrink-0 items-center gap-3">
          <span className="text-xs text-slate-400">{new Date(version.created_at).toLocaleDateString()}</span>
          {/* Hard deletion is available only to family managers and always targets this exact immutable ID. */}
          {canDelete ? (
            <button
              type="button"
              disabled={Boolean(deletingVersionId)}
              onClick={() => onDelete(version)}
              className="inline-flex h-8 w-8 items-center justify-center rounded-lg bg-red-50 text-red-600 transition-colors hover:bg-red-100 disabled:cursor-not-allowed disabled:opacity-50"
              title={`Delete version ${version.version}`}
              aria-label={`Delete version ${version.version}`}
            >
              {deleting ? <Loader2 className="h-4 w-4 animate-spin" /> : <Trash2 className="h-4 w-4" />}
            </button>
          ) : null}
          {/* The chevron is present only when this adapter supplies expandable version details. */}
          {expandable ? <ChevronDown className={`h-4 w-4 text-slate-400 transition-transform ${expanded ? "rotate-180" : ""}`} aria-hidden="true" /> : null}
        </div>
      </div>
      {/* Expanded details remain scoped to this immutable version and never inherit sibling endpoints. */}
      {expanded && renderDetails ? (
        <div className="space-y-3 border-t border-slate-100 bg-slate-50/60 px-4 py-4 sm:px-5">
          {renderDetails(version)}
          <button type="button" onClick={() => onSelect(version.id)} className="text-xs font-medium text-blue-600 hover:text-blue-700">
            Open version details
          </button>
        </div>
      ) : null}
    </div>
  );
}

/** Renders family-scoped immutable versions with exact-version navigation and lifecycle controls. */
export function AppVersionHistory<T extends AppVersionHistoryItem>({ versions, currentId, canDelete, deletingVersionId, onSelect, onDelete, renderDetails }: AppVersionHistoryProps<T>) {
  const [expandedVersionId, setExpandedVersionId] = useState("");

  /** Toggles one optional detail panel while keeping sibling version cards compact. */
  const toggleDetails = (versionId: string) => {
    // Re-selecting an open card collapses it instead of leaving an irreversible disclosure.
    setExpandedVersionId((current) => current === versionId ? "" : versionId);
  };

  return (
    <div className="overflow-hidden rounded-xl border border-slate-200 bg-white">
      <div className="border-b border-slate-100 bg-slate-50 px-5 py-3">
        <h4 className="text-sm font-semibold text-slate-800">Version history</h4>
      </div>
      <div className="divide-y divide-slate-100">
        {versions.map((version) => (
          // Only one immutable version expands at a time so long histories remain scannable.
          <AppVersionHistoryRow
            key={version.id}
            version={version}
            currentId={currentId}
            canDelete={canDelete}
            deletingVersionId={deletingVersionId}
            expanded={Boolean(renderDetails) && expandedVersionId === version.id}
            onToggle={toggleDetails}
            onSelect={onSelect}
            onDelete={onDelete}
            renderDetails={renderDetails}
          />
        ))}
      </div>
    </div>
  );
}
