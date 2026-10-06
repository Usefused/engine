import { CopyValue } from "~/components/CopyValue";

export interface McpTransportURLs {
  streamable_http?: string | null;
  sse?: string | null;
  versioned_streamable_http?: string | null;
  versioned_sse?: string | null;
}

export interface McpTransportEndpointData {
  default_transport?: string | null;
  stable?: boolean | null;
  stable_version_id?: string | null;
  transport_urls?: McpTransportURLs | null;
}

export type McpTransportName = keyof McpTransportURLs;

interface McpTransportEndpointsProps {
  endpoints: McpTransportEndpointData;
  enabled?: boolean;
  onCopied?: (transport: McpTransportName) => void;
}

/** Distinguishes stable, pinned, and legacy transport guidance without changing URL authority. */
function TransportBadge({ children, legacy = false }: { children: string; legacy?: boolean }) {
  const colors = legacy
    ? "border-amber-200 bg-amber-50 text-amber-700"
    : "border-violet-200 bg-violet-50 text-violet-700";
  return <span className={`rounded-full border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${colors}`}>{children}</span>;
}

/** Copies one Engine-projected endpoint without reconstructing it in the browser. */
function EndpointRow({ label, transport, url, onCopy }: {
  label: string;
  transport: McpTransportName;
  url: string;
  onCopy: (transport: McpTransportName, url: string) => void;
}) {
  return <CopyValue value={url} label={`${label} URL`} className="mt-2" onCopied={() => onCopy(transport, url)} />;
}

/** Reads only typed Engine discovery fields so public-origin rules stay server-owned. */
function transportURL(endpoints: McpTransportEndpointData, transport: McpTransportName): string {
  const value = endpoints.transport_urls?.[transport];
  return typeof value === "string" ? value.trim() : "";
}

/** Presents the stable family URL as the endpoint users normally configure once. */
function RecommendedEndpoint({ url, isDefault, isStable, stableVersionID, onCopy }: {
  url: string;
  isDefault: boolean;
  isStable: boolean;
  stableVersionID: string;
  onCopy: (transport: McpTransportName, url: string) => void;
}) {
  return (
    <div className="rounded-xl border border-violet-100 bg-violet-50/40 p-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xs font-semibold text-slate-800">Streamable HTTP · Stable</span>
        <TransportBadge>Recommended</TransportBadge>
        {isDefault && <span className="text-[11px] text-slate-500">Fused default</span>}
        {isStable && <span className="text-[11px] font-medium text-emerald-700">This version is promoted</span>}
      </div>
      {url && !isStable && stableVersionID ? <p className="mt-2 text-xs text-amber-700">This URL currently routes to Version ID <code>{stableVersionID}</code>.</p> : null}
      {url ? (
        <EndpointRow label="Streamable HTTP" transport="streamable_http" url={url} onCopy={onCopy} />
      ) : (
        <p className="mt-2 text-xs text-slate-500">No version is currently promoted to the stable endpoint.</p>
      )}
    </div>
  );
}

/** Renders one immutable version URL with only compatibility metadata that adds new information. */
function VersionEndpoint({ label, transport, url, legacy = false, onCopy }: {
  label: string;
  transport: McpTransportName;
  url: string;
  legacy?: boolean;
  onCopy: (transport: McpTransportName, url: string) => void;
}) {
  // An absent pinned projection cannot be reconstructed safely from a public origin.
  if (!url) return null;
  return (
    <div className="rounded-xl border border-slate-200 bg-white p-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xs font-semibold text-slate-700">{label}</span>
        {/* Legacy belongs to the exact endpoint it qualifies, not to an ambiguous group heading. */}
        {legacy ? <TransportBadge legacy>Legacy</TransportBadge> : null}
      </div>
      <EndpointRow label={label} transport={transport} url={url} onCopy={onCopy} />
    </div>
  );
}

/** Shows the stable SSE compatibility route with its lifecycle identity attached. */
function LegacyStableEndpoint({ stableURL, onCopy }: {
  stableURL: string;
  onCopy: (transport: McpTransportName, url: string) => void;
}) {
  // Engines without SSE discovery should not render an empty compatibility endpoint.
  if (!stableURL) return null;
  return (
    <div className="rounded-xl border border-slate-200 bg-white p-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xs font-semibold text-slate-700">SSE · Stable</span>
        <TransportBadge>Stable</TransportBadge>
        <TransportBadge legacy>Legacy</TransportBadge>
      </div>
      <EndpointRow label="stable SSE" transport="sse" url={stableURL} onCopy={onCopy} />
    </div>
  );
}

/** Renders Engine-owned MCP transport discovery without rebuilding endpoint URLs in the browser. */
export function McpTransportEndpoints({ endpoints, enabled = true, onCopied }: McpTransportEndpointsProps) {

  // A non-runnable exact version must not expose either stable or pinned copy controls.
  if (!enabled) {
    return (
      <div className="rounded-lg border border-slate-200 bg-white px-3 py-2 text-xs italic text-slate-500">
        Server unavailable -- restore it to reconnect
      </div>
    );
  }

  /** Receives confirmation only after the shared control has copied the Engine-owned URL. */
  const copyEndpoint = (transport: McpTransportName) => {
    // Notification callbacks run only after confirmed clipboard success.
    if (onCopied) onCopied(transport);
  };

  return (
    <div className="space-y-3" data-default-transport={endpoints.default_transport || "unknown"}>
      <RecommendedEndpoint
        url={transportURL(endpoints, "streamable_http")}
        isDefault={endpoints.default_transport === "streamable_http"}
        isStable={endpoints.stable === true}
        stableVersionID={typeof endpoints.stable_version_id === "string" ? endpoints.stable_version_id : ""}
        onCopy={copyEndpoint}
      />
      <LegacyStableEndpoint
        stableURL={transportURL(endpoints, "sse")}
        onCopy={copyEndpoint}
      />
    </div>
  );
}

/** Renders only the immutable endpoints belonging to one expanded version-history card. */
export function McpVersionTransportEndpoints({ endpoints, enabled = true, onCopied }: McpTransportEndpointsProps) {

  // Deactivated versions must not retain copyable runtime addresses in history.
  if (!enabled) {
    return <p className="text-xs italic text-slate-500">This version is no longer available for connections.</p>;
  }

  /** Reports the exact transport identity after the shared control confirms a successful copy. */
  const copyEndpoint = (transport: McpTransportName) => {
    // Copy feedback remains optional so the history card can be reused in read-only surfaces.
    // Notification callbacks run only after confirmed clipboard success.
    if (onCopied) onCopied(transport);
  };

  return (
    <div className="space-y-3">
      <VersionEndpoint
        label="Streamable HTTP · Version-pinned"
        transport="versioned_streamable_http"
        url={transportURL(endpoints, "versioned_streamable_http")}
        onCopy={copyEndpoint}
      />
      <VersionEndpoint
        label="SSE · Version-pinned"
        transport="versioned_sse"
        url={transportURL(endpoints, "versioned_sse")}
        legacy
        onCopy={copyEndpoint}
      />
    </div>
  );
}
