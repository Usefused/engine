import { useState } from "react";
import { stripLinks } from "~/lib/format";

const ENDPOINT_DESCRIPTION_COLLAPSE_THRESHOLD = 240;

interface EndpointRowData {
  id: string;
  method?: string;
  path?: string;
  name?: string;
  description?: string;
  deprecated?: boolean;
  status?: string;
}

interface WebhookRowData {
  id: string;
  name?: string;
  method?: string;
  description?: string;
}

export const METHOD_COLORS: Record<string, string> = {
  GET: "bg-green-100 text-green-700 border-green-200",
  POST: "bg-blue-100 text-blue-700 border-blue-200",
  PUT: "bg-yellow-100 text-yellow-700 border-yellow-200",
  PATCH: "bg-orange-100 text-orange-700 border-orange-200",
  DELETE: "bg-red-100 text-red-700 border-red-200",
};

interface RowSelectionControlsProps {
  selectable?: boolean;
  selected?: boolean;
  bulkMode?: boolean;
  onSelect?: (selected: boolean) => void;
}

// Shared between EndpointRow and WebhookRow: the bulk-mode checkbox (shown
// when bulkMode is on) and the hover "Select" button (shown otherwise).
function BulkCheckbox({ selected, onSelect }: RowSelectionControlsProps) {
  return (
    <div className="flex items-center justify-center shrink-0 animate-in fade-in zoom-in duration-200" onClick={(e) => e.stopPropagation()}>
      <input
        type="checkbox"
        className="rounded border-slate-300 text-blue-600 focus:ring-blue-500 w-4 h-4 cursor-pointer"
        checked={selected || false}
        onChange={(e) => onSelect?.(e.target.checked)}
      />
    </div>
  );
}

function SelectButton({ selected, onSelect }: RowSelectionControlsProps) {
  return (
    <div className="shrink-0 ml-auto p-2 -m-2 group/select" onClick={(e) => e.stopPropagation()}>
      <button
        type="button"
        className={`text-xs font-medium uppercase tracking-wide transition-opacity duration-200 ${
          selected 
            ? 'opacity-100 text-blue-600' 
            : 'opacity-0 group-hover/select:opacity-100 focus-within:opacity-100 text-slate-400 hover:text-slate-600'
        }`}
        onClick={(e) => {
          e.preventDefault();
          onSelect?.(!selected);
        }}
      >
        {selected ? 'Selected' : 'Select'}
      </button>
    </div>
  );
}

function MethodBadge({ method }: { method?: string }) {
  return (
    <span className={`shrink-0 text-xs font-bold px-1.5 py-0.5 rounded ${(method && METHOD_COLORS[method]) ?? "bg-slate-100 text-slate-700 border-slate-200"}`}>
      {method || "EP"}
    </span>
  );
}

function EndpointStatusBadges({ ep }: { ep: EndpointRowData }) {
  return (
    <>
      {ep.deprecated && <span className="shrink-0 inline-flex items-center px-1.5 py-0.5 rounded text-[10px] font-semibold bg-red-50 text-red-700 border border-red-200 uppercase tracking-wider">Deprecated</span>}
      {ep.status === "drifted" && <span className="shrink-0 ml-2 inline-flex items-center px-1.5 py-0.5 rounded text-[10px] font-semibold bg-orange-100 text-orange-800 border border-orange-200 uppercase tracking-wider">Drifted</span>}
    </>
  );
}

/** Estimates visible description length without counting markup as user-facing text. */
function descriptionTextLength(html: string): number {
  return html
    .replace(/<[^>]*>/g, " ")
    .replace(/&(?:#\d+|#x[\da-f]+|[a-z][\w-]*);/gi, " ")
    .replace(/\s+/g, " ")
    .trim()
    .length;
}

/** Keeps long operation documentation compact until the reader explicitly expands it. */
function EndpointDescription({ description }: { description: string }) {
  const [expanded, setExpanded] = useState(false);
  const safeDescription = stripLinks(description);
  const collapsible = descriptionTextLength(safeDescription) > ENDPOINT_DESCRIPTION_COLLAPSE_THRESHOLD;

  /** Toggles only the documentation disclosure without opening the endpoint detail drawer. */
  const toggleExpanded = (event: React.MouseEvent<HTMLButtonElement>) => {
    event.preventDefault();
    event.stopPropagation();
    setExpanded((current) => !current);
  };

  return (
    <div className="mt-1">
      <div
        // Short descriptions remain natural-height; only genuinely long copy receives the clamp.
        className={`text-xs text-slate-500 [&_p]:mb-1 last:[&_p]:mb-0 [&_code]:rounded [&_code]:bg-slate-100 [&_code]:px-1 [&_code]:py-0.5 ${collapsible && !expanded ? "line-clamp-3" : ""}`}
        dangerouslySetInnerHTML={{ __html: safeDescription }}
      />
      {collapsible ? (
        <button
          type="button"
          aria-expanded={expanded}
          onClick={toggleExpanded}
          className="mt-1 text-xs font-medium text-blue-600 hover:text-blue-700 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-blue-500 focus-visible:ring-offset-2"
        >
          {expanded ? "Show less" : "Show more"}
        </button>
      ) : null}
    </div>
  );
}

/** Renders one operation summary with bounded documentation and optional selection controls. */
export function EndpointRow({ ep, onClick, selectable, selected, bulkMode, onSelect }: { ep: EndpointRowData; onClick?: () => void; selectable?: boolean; selected?: boolean; bulkMode?: boolean; onSelect?: (selected: boolean) => void }) {
  const method = ep.method?.toUpperCase();
  return (
    <div className={`px-4 py-4 transition-colors sm:px-5 ${onClick ? 'cursor-pointer hover:bg-slate-50 group' : 'hover:bg-slate-50 group'}`} onClick={onClick}>
      <div className="flex items-start sm:items-center justify-between gap-3 mb-1 flex-col sm:flex-row">
        <div className="flex items-start sm:items-center gap-3 min-w-0">
          {bulkMode && selectable && <BulkCheckbox selected={selected} onSelect={onSelect} />}
          <MethodBadge method={method} />
          <code className="min-w-0 break-all text-sm text-slate-700">{ep.path || ep.name}</code>
          <EndpointStatusBadges ep={ep} />
        </div>
        {selectable && !bulkMode && <SelectButton selected={selected} onSelect={onSelect} />}
      </div>
      {ep.description ? <EndpointDescription description={ep.description} /> : null}
    </div>
  );
}

export function WebhookRow({ wh, onClick, selectable, selected, bulkMode, onSelect }: { wh: WebhookRowData; onClick?: () => void; selectable?: boolean; selected?: boolean; bulkMode?: boolean; onSelect?: (selected: boolean) => void }) {
  return (
    <div className={`px-4 py-4 transition-colors sm:px-5 ${onClick ? 'cursor-pointer hover:bg-slate-50 group' : 'hover:bg-slate-50 group'}`} onClick={onClick}>
      <div className="flex items-start sm:items-center justify-between gap-3 mb-1 flex-col sm:flex-row">
        <div className="flex items-start sm:items-center gap-3 min-w-0">
          {bulkMode && selectable && <BulkCheckbox selected={selected} onSelect={onSelect} />}
          <span className="shrink-0 text-[10px] font-bold px-1.5 py-0.5 rounded bg-purple-100 text-purple-700 border border-purple-200 uppercase tracking-wider">Webhook</span>
          <span className="text-sm text-slate-700 font-semibold truncate min-w-0">{wh.name || wh.method}</span>
        </div>
        {selectable && !bulkMode && <SelectButton selected={selected} onSelect={onSelect} />}
      </div>
      {wh.description && <p className="text-xs text-slate-500 mt-1 ml-1">{wh.description}</p>}
    </div>
  );
}
