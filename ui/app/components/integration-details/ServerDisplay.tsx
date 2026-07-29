import { useRef, useState } from "react";
import { ChevronDown, Pencil } from "lucide-react";
import { useToast } from "~/components/Toast";

interface Server {
  url: string;
  description?: string;
}

interface ServerDisplayProps {
  srv: {
    servers?: Server[];
    base_url?: string;
    is_owner?: boolean;
  };
  isAuth: boolean;
  onEditClick?: (draft: Server[]) => void;
}

function getEnvBadgeStyles(desc: string) {
  const d = desc.toLowerCase();
  if (d.includes("prod") || d.includes("production")) return "bg-emerald-50 text-emerald-700 border-emerald-200/60";
  if (d.includes("sandbox") || d.includes("dev") || d.includes("staging") || d.includes("test")) return "bg-amber-50 text-amber-700 border-amber-200/60";
  return "bg-slate-50 text-slate-600 border-slate-200";
}

export function ServerDisplay({ srv, isAuth, onEditClick }: ServerDisplayProps) {
  const toast = useToast();
  const [dropdownOpen, setDropdownOpen] = useState(false);
  const dropdownRef = useRef<HTMLDivElement>(null);

  let primaryIndex = 0;
  if (srv.servers && srv.servers.length > 0) {
    const prodIdx = srv.servers.findIndex(s =>
      s.description?.toLowerCase().includes("prod") || s.description?.toLowerCase().includes("production")
    );
    if (prodIdx !== -1) primaryIndex = prodIdx;
  }

  const primaryServer = srv.servers?.[primaryIndex];
  const primaryUrl = primaryServer?.url || srv.base_url;
  const primaryDesc = primaryServer?.description;
  const extraServers = srv.servers ? srv.servers.filter((_, i) => i !== primaryIndex) : [];

  function handleEditClick() {
    let draft: Server[];
    if (srv.servers && srv.servers.length > 0) {
      draft = JSON.parse(JSON.stringify(srv.servers));
    } else if (srv.base_url) {
      draft = [{ url: srv.base_url, description: "" }];
    } else {
      draft = [{ url: "", description: "" }];
    }
    onEditClick?.(draft);
  }

  return (
    <div className="flex items-center gap-2 mt-1.5" ref={dropdownRef}>
      {primaryDesc && (
        <span className={`px-2 py-0.5 rounded text-[10px] font-semibold uppercase tracking-wider border ${getEnvBadgeStyles(primaryDesc)}`}>
          {primaryDesc}
        </span>
      )}
      {primaryUrl ? (
        <button
          onClick={() => {
            navigator.clipboard.writeText(primaryUrl);
            toast.success("URL copied to clipboard");
          }}
          className="text-xs font-mono text-slate-600 hover:text-blue-600 transition-colors truncate max-w-sm cursor-pointer"
          title="Click to copy URL"
        >
          {primaryUrl}
        </button>
      ) : (
        <span className="text-xs text-slate-400 italic">No Base URL configured</span>
      )}
      {extraServers.length > 0 && (
        <div className="relative flex items-center">
          <button
            data-track="toggle_server_dropdown"
            onClick={() => setDropdownOpen(o => !o)}
            className="flex items-center p-1 text-slate-400 hover:text-slate-600 hover:bg-slate-100 rounded transition-colors cursor-pointer"
            title="Other environments"
          >
            <ChevronDown className={`w-3.5 h-3.5 transition-transform duration-200 ${dropdownOpen ? "rotate-180" : ""}`} />
          </button>
          {dropdownOpen && (
            <div className="absolute top-full left-0 mt-1 z-30 bg-white border border-slate-200 rounded-lg shadow-lg py-1.5 min-w-[280px] divide-y divide-slate-100 overflow-hidden animate-in fade-in slide-in-from-top-1 duration-150">
              {extraServers.map((s, i) => (
                <button
                  key={i}
                  onClick={() => {
                    navigator.clipboard.writeText(s.url);
                    toast.success("URL copied to clipboard");
                    setDropdownOpen(false);
                  }}
                  className="w-full text-left flex items-start justify-between gap-3 px-4 py-2.5 hover:bg-slate-50 transition-colors group cursor-pointer"
                >
                  <div className="flex flex-col min-w-0">
                    {s.description && (
                      <span className={`w-fit px-1.5 py-0.5 rounded text-[9px] font-semibold uppercase tracking-wider border mb-1 ${getEnvBadgeStyles(s.description)}`}>
                        {s.description}
                      </span>
                    )}
                    <span className="text-xs font-mono text-slate-500 group-hover:text-blue-600 transition-colors truncate">{s.url}</span>
                  </div>
                </button>
              ))}
            </div>
          )}
        </div>
      )}
      {isAuth && srv.is_owner !== false && onEditClick && (
        <button
          data-track="edit_base_urls"
          onClick={handleEditClick}
          className="p-1 ml-1 text-slate-400 hover:text-slate-600 hover:bg-slate-100 rounded transition-colors"
          title="Edit Base URLs"
        >
          <Pencil className="w-3 h-3" />
        </button>
      )}
    </div>
  );
}
