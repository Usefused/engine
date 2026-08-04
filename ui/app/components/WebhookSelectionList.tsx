import { useState } from "react";
import { Search, X } from "lucide-react";

// Shared between endpoint lists (IntegrationObject) and webhook lists
// (WebhookObject) — only the fields actually rendered/searched here.
export interface SelectableEvent {
  id: string;
  name: string;
  description?: string;
  path?: string;
  resource?: string;
  method?: string;
}

export interface WebhookSelectionListProps {
  webhooks: SelectableEvent[];
  selectedIds: Set<string>;
  onToggle: (id: string, selected: boolean) => void;
  getId: (wh: SelectableEvent) => string;
  maxHeightClass?: string;
  disabled?: boolean;
}

export default function WebhookSelectionList({
  webhooks,
  selectedIds,
  onToggle,
  getId,
  maxHeightClass = "max-h-[300px]",
  disabled = false
}: WebhookSelectionListProps) {
  const [searchTerm, setSearchTerm] = useState("");

  const filtered = webhooks.filter(wh => {
    if (!searchTerm.trim()) return true;
    const term = searchTerm.toLowerCase();
    return (
      wh.path?.toLowerCase().includes(term) ||
      wh.name?.toLowerCase().includes(term) ||
      wh.description?.toLowerCase().includes(term) ||
      wh.resource?.toLowerCase().includes(term)
    );
  });

  return (
    <div className="border border-slate-200 rounded-lg bg-white overflow-hidden flex flex-col shadow-sm">
      {/* Search Input */}
      <div className="relative border-b border-slate-100 bg-slate-50/50 p-2 flex items-center">
        <Search className="absolute left-4 w-4 h-4 text-slate-400 pointer-events-none" />
        <input
          type="text"
          value={searchTerm}
          onChange={(e) => setSearchTerm(e.target.value)}
          placeholder="Search proposed events..."
          className="w-full pl-9 pr-8 py-1.5 bg-white border border-slate-200 rounded-md text-xs placeholder:text-slate-400 focus:outline-none focus:ring-1 focus:ring-blue-500 focus:border-blue-500 transition-all text-slate-800"
        />
        {searchTerm && (
          <button
            data-track="clear_webhook_search"
            type="button"
            onClick={() => setSearchTerm("")}
            className="absolute right-4 text-slate-400 hover:text-slate-600 focus:outline-none cursor-pointer flex items-center"
          >
            <X className="w-3.5 h-3.5" />
          </button>
        )}
      </div>

      {/* List */}
      <div className={`overflow-y-auto divide-y divide-slate-100 flex-1 ${maxHeightClass}`}>
        {filtered.length === 0 ? (
          <div className="p-8 text-center text-sm text-slate-400">
            No matching events found
          </div>
        ) : (
          filtered.map((wh, idx) => {
            const id = getId(wh);
            const isSelected = selectedIds.has(id);
            return (
              <label
                key={`${id}-${idx}`}
                className={`flex items-start gap-3 p-3 cursor-pointer transition-colors ${isSelected ? 'bg-blue-50/40' : 'hover:bg-slate-50'}`}
              >
                <input
                  type="checkbox"
                  checked={isSelected}
                  disabled={disabled}
                  onChange={(e) => onToggle(id, e.target.checked)}
                  className="mt-1 flex-shrink-0 w-4 h-4 text-blue-600 rounded border-slate-300 focus:ring-blue-500 disabled:opacity-50"
                />
                <div className="flex-1 min-w-0">
                  <div className="flex items-center gap-2 mb-1">
                    <span className="text-[10px] font-bold px-1.5 py-0.5 rounded shadow-sm bg-purple-100 text-purple-700 border border-purple-200">
                      EVENT
                    </span>
                    <span className={`text-sm font-medium truncate ${isSelected ? 'text-blue-900' : 'text-slate-900'}`}>
                      {wh.path || wh.name}
                    </span>
                  </div>
                  <p className="text-xs text-slate-500 truncate font-sans">
                    {wh.description || ""}
                  </p>
                </div>
              </label>
            );
          })
        )}
      </div>
    </div>
  );
}
