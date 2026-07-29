import { FormEvent } from "react";
import { Trash } from "lucide-react";

interface ServerDraft {
  url: string;
  description: string;
}

interface ServersFormProps {
  draftServers: ServerDraft[];
  setDraftServers: React.Dispatch<React.SetStateAction<ServerDraft[]>>;
  savingServers: boolean;
  handleSaveServers: (e: FormEvent) => void;
  setEditingServers: (editing: boolean) => void;
}

export function ServersForm({
  draftServers,
  setDraftServers,
  savingServers,
  handleSaveServers,
  setEditingServers,
}: ServersFormProps) {
  return (
    <div className="bg-white rounded-xl border border-slate-200 p-5 mt-2">
      <h3 className="text-sm font-semibold text-slate-800 mb-3">Edit Base URLs (Servers)</h3>
      <div className="space-y-4">
        {draftServers.map((s, i) => (
          <div
            key={i}
            className="flex flex-col sm:flex-row gap-3 items-stretch sm:items-center pb-3 sm:pb-0 border-b border-slate-100 sm:border-0 last:border-0"
          >
            <div className="flex-1 flex flex-col sm:flex-row gap-2 sm:gap-4">
              <input
                type="text"
                placeholder="https://api.example.com"
                value={s.url}
                onChange={(e) => {
                  const newServers = [...draftServers];
                  newServers[i].url = e.target.value;
                  setDraftServers(newServers);
                }}
                className="flex-1 text-sm border border-slate-200 rounded-md px-3 py-2 focus:border-blue-500 focus:ring-1 focus:ring-blue-500 outline-none w-full"
              />
              <input
                type="text"
                placeholder="Description (e.g. Production)"
                value={s.description || ""}
                onChange={(e) => {
                  const newServers = [...draftServers];
                  newServers[i].description = e.target.value.replace(/[^a-zA-Z0-9]/g, "");
                  setDraftServers(newServers);
                }}
                title="Must be a single alphanumeric word (e.g. Production)"
                className="w-full sm:w-48 text-sm border border-slate-200 rounded-md px-3 py-2 focus:border-blue-500 focus:ring-1 focus:ring-blue-500 outline-none text-slate-500"
              />
            </div>
            <div className="flex justify-end sm:justify-start">
              <button
                data-track="remove_server_url"
                onClick={() => setDraftServers((ds) => ds.filter((_, idx) => idx !== i))}
                className="p-1.5 text-slate-400 hover:text-red-500 cursor-pointer flex items-center gap-1 sm:block"
                title="Remove URL"
              >
                <Trash className="w-4 h-4" />
                <span className="text-xs sm:hidden">Remove URL</span>
              </button>
            </div>
          </div>
        ))}
      </div>
      <div className="mt-2">
        <button
          data-track="add_server_url"
          onClick={() => setDraftServers((ds) => [...ds, { url: "", description: "" }])}
          className="text-xs font-medium text-blue-600 hover:text-blue-700 hover:underline cursor-pointer"
        >
          + Add URL
        </button>
      </div>
      <div className="flex justify-end gap-2 mt-4">
        <button
          data-track="cancel_edit_servers"
          onClick={() => setEditingServers(false)}
          className="px-3 py-1.5 text-sm font-medium text-slate-600 hover:bg-slate-50 rounded border border-slate-200 transition-colors cursor-pointer"
        >
          Cancel
        </button>
        <button
          data-track="save_servers"
          onClick={handleSaveServers}
          disabled={savingServers}
          className="px-3 py-1.5 text-sm font-medium text-white bg-blue-600 hover:bg-blue-700 rounded disabled:opacity-50 transition-colors"
        >
          {savingServers ? "Saving..." : "Save"}
        </button>
      </div>
    </div>
  );
}
