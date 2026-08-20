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

interface ServerSelection {
  primaryUrl?: string;
  primaryDescription?: string;
  extraServers: Server[];
}

// getEnvBadgeStyles gives known deployment classes a stable semantic colour
// while leaving unfamiliar provider labels visually neutral.
function getEnvBadgeStyles(desc: string) {
  const d = desc.toLowerCase();
  // Production and non-production labels need distinct scanning cues in the
  // compact environment picker, but the original text remains authoritative.
  if (d.includes("prod")) return "bg-emerald-50 text-emerald-700 border-emerald-200/60";
  if (d.includes("sandbox") || d.includes("dev") || d.includes("staging") || d.includes("test")) return "bg-amber-50 text-amber-700 border-amber-200/60";
  return "bg-slate-50 text-slate-600 border-slate-200";
}

// isProductionServer identifies the preferred collapsed endpoint without
// assigning meaning to unknown provider-specific environment descriptions.
function isProductionServer(server: Server): boolean {
  return server.description?.toLowerCase().includes("prod") ?? false;
}

// selectServerURLs chooses one collapsed endpoint and keeps all other declared
// environments in their original order for the dropdown.
function selectServerURLs(srv: ServerDisplayProps["srv"]): ServerSelection {
  const servers = srv.servers ?? [];
  if (servers.length === 0) {
    return { primaryUrl: srv.base_url, extraServers: [] };
  }
  const productionIndex = servers.findIndex(isProductionServer);
  // A declaration without a production label retains its first endpoint as the
  // stable primary choice instead of reordering provider data.
  const primaryIndex = productionIndex >= 0 ? productionIndex : 0;
  return {
    primaryUrl: servers[primaryIndex].url,
    primaryDescription: servers[primaryIndex].description,
    extraServers: servers.filter((_, index) => index !== primaryIndex),
  };
}

// createServerDraft clones declared endpoints and supplies one empty row when
// no server exists so the editor can always render a useful starting state.
function createServerDraft(srv: ServerDisplayProps["srv"]): Server[] {
  if (srv.servers && srv.servers.length > 0) {
    return srv.servers.map((server) => ({ ...server }));
  }
  if (srv.base_url) return [{ url: srv.base_url, description: "" }];
  return [{ url: "", description: "" }];
}

// EnvironmentBadge keeps environment styling identical between the collapsed
// endpoint and alternate-server rows.
function EnvironmentBadge({ description, compact = false }: { description?: string; compact?: boolean }) {
  if (!description) return null;
  const spacing = compact ? "mb-1 px-1.5 text-[9px]" : "px-2 text-[10px]";
  return (
    <span className={`w-fit rounded border py-0.5 font-semibold uppercase tracking-wider ${spacing} ${getEnvBadgeStyles(description)}`}>
      {description}
    </span>
  );
}

// PrimaryServerURL renders a truncated copy control while the native tooltip
// and accessible name retain the complete URL.
function PrimaryServerURL({ url, description, onCopy }: { url?: string; description?: string; onCopy: (url: string) => void }) {
  if (!url) return <span className="text-xs italic text-slate-400">No Base URL configured</span>;
  return (
    <>
      <EnvironmentBadge description={description} />
      <button
        onClick={() => onCopy(url)}
        className="min-w-0 max-w-full cursor-pointer truncate text-left font-mono text-xs text-slate-600 transition-colors hover:text-blue-600 sm:max-w-sm"
        title={url}
        aria-label={`Copy URL ${url}`}
      >
        {url}
      </button>
    </>
  );
}

// ExtraServerMenu keeps the dropdown concern isolated from primary endpoint
// selection and edit authorization.
function ExtraServerMenu({ servers, open, onToggle, onCopy }: { servers: Server[]; open: boolean; onToggle: () => void; onCopy: (url: string) => void }) {
  if (servers.length === 0) return null;
  return (
    <div className="relative flex items-center">
      <button
        data-track="toggle_server_dropdown"
        onClick={onToggle}
        className="flex cursor-pointer items-center rounded p-1 text-slate-400 transition-colors hover:bg-slate-100 hover:text-slate-600"
        title="Other environments"
      >
        <ChevronDown className={`h-3.5 w-3.5 transition-transform duration-200 ${open ? "rotate-180" : ""}`} />
      </button>
      {open && (
        <div className="absolute right-0 top-full z-30 mt-1 w-[min(280px,calc(100vw-2rem))] divide-y divide-slate-100 overflow-hidden rounded-lg border border-slate-200 bg-white py-1.5 shadow-lg animate-in fade-in slide-in-from-top-1 duration-150 sm:left-0 sm:right-auto">
          {servers.map((server, index) => (
            <button
              key={index}
              onClick={() => onCopy(server.url)}
              className="group flex w-full cursor-pointer items-start justify-between gap-3 px-4 py-2.5 text-left transition-colors hover:bg-slate-50"
              title={server.url}
              aria-label={`Copy URL ${server.url}`}
            >
              <div className="flex min-w-0 flex-col">
                <EnvironmentBadge description={server.description} compact />
                <span className="truncate font-mono text-xs text-slate-500 transition-colors group-hover:text-blue-600">{server.url}</span>
              </div>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

// EditServerButton omits the control entirely when the caller lacks ownership
// or the surrounding surface does not provide an editor.
function EditServerButton({ enabled, onClick }: { enabled: boolean; onClick: () => void }) {
  if (!enabled) return null;
  return (
    <button
      data-track="edit_base_urls"
      onClick={onClick}
      className="ml-1 rounded p-1 text-slate-400 transition-colors hover:bg-slate-100 hover:text-slate-600"
      title="Edit Base URLs"
    >
      <Pencil className="h-3 w-3" />
    </button>
  );
}

// ServerDisplay keeps long URLs compact while preserving exact hover, focus,
// and copy access for every configured environment.
export function ServerDisplay({ srv, isAuth, onEditClick }: ServerDisplayProps) {
  const toast = useToast();
  const [dropdownOpen, setDropdownOpen] = useState(false);
  const dropdownRef = useRef<HTMLDivElement>(null);
  const selection = selectServerURLs(srv);

  // handleEditClick gives the editor an isolated draft so cancelling cannot
  // mutate server values already rendered elsewhere on the page.
  function handleEditClick() {
    if (!onEditClick) return;
    onEditClick(createServerDraft(srv));
  }

  // copyServerURL centralizes copy feedback and optionally closes the picker
  // after an alternate environment has been selected.
  function copyServerURL(url: string, closeDropdown = false) {
    navigator.clipboard.writeText(url);
    toast.success("URL copied to clipboard");
    if (closeDropdown) setDropdownOpen(false);
  }

  return (
    <div className="mt-1.5 flex max-w-full min-w-0 items-center gap-2" ref={dropdownRef}>
      <PrimaryServerURL url={selection.primaryUrl} description={selection.primaryDescription} onCopy={copyServerURL} />
      <ExtraServerMenu
        servers={selection.extraServers}
        open={dropdownOpen}
        onToggle={() => setDropdownOpen((open) => !open)}
        onCopy={(url) => copyServerURL(url, true)}
      />
      <EditServerButton enabled={isAuth && srv.is_owner !== false && Boolean(onEditClick)} onClick={handleEditClick} />
    </div>
  );
}
