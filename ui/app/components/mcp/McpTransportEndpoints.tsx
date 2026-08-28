import { useState } from "react";
import { Check, ChevronDown, Copy } from "lucide-react";

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

/** Distinguishes recommended, pinned, and legacy transport guidance without changing URL authority. */
function TransportBadge({ children, legacy = false }: { children: string; legacy?: boolean }) {
  const colors = legacy
    ? "border-amber-200 bg-amber-50 text-amber-700"
    : "border-violet-200 bg-violet-50 text-violet-700";
  return <span className={`rounded-full border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${colors}`}>{children}</span>;
}

/** Copies one Engine-projected endpoint without reconstructing it in the browser. */
function EndpointRow({ label, transport, url, copied, onCopy }: {
  label: string;
  transport: McpTransportName;
  url: string;
  copied: boolean;
  onCopy: (transport: McpTransportName, url: string) => void;
}) {
  return (
    <div className="mt-2 flex min-w-0 items-center gap-2">
      <code className="min-w-0 flex-1 break-all rounded-lg border border-slate-200 bg-white px-3 py-2 font-mono text-xs text-slate-800 shadow-sm">
        {url}
      </code>
      <button
        type="button"
        data-track={`copy_mcp_${transport}_url`}
        onClick={() => onCopy(transport, url)}
        className="inline-flex h-9 w-9 shrink-0 items-center justify-center rounded-lg border border-slate-200 bg-white text-slate-600 shadow-sm transition-colors hover:bg-slate-50 hover:text-slate-900"
        title={`Copy ${label} URL`}
        aria-label={`Copy ${label} URL`}
      >
        {copied ? <Check className="h-4 w-4 text-emerald-600" /> : <Copy className="h-4 w-4" />}
      </button>
    </div>
  );
}

/** Reads only typed Engine discovery fields so public-origin rules stay server-owned. */
function transportURL(endpoints: McpTransportEndpointData, transport: McpTransportName): string {
  const value = endpoints.transport_urls?.[transport];
  return typeof value === "string" ? value.trim() : "";
}

/** Presents the stable family URL as the endpoint users normally configure once. */
function RecommendedEndpoint({ url, copied, isDefault, isStable, stableVersionID, onCopy }: {
  url: string;
  copied: boolean;
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
        {isDefault && <span className="text-[11px] text-slate-500">Engine default</span>}
        {isStable && <span className="text-[11px] font-medium text-emerald-700">This version is promoted</span>}
      </div>
      {url && !isStable && stableVersionID ? <p className="mt-2 text-xs text-amber-700">This URL currently routes to Version ID <code>{stableVersionID}</code>.</p> : null}
      {url ? (
        <EndpointRow label="Streamable HTTP" transport="streamable_http" url={url} copied={copied} onCopy={onCopy} />
      ) : (
        <p className="mt-2 text-xs text-slate-500">No version is currently promoted to the stable endpoint.</p>
      )}
    </div>
  );
}

/** Keeps an immutable Streamable HTTP URL available for deliberate version pinning. */
function PinnedEndpoint({ url, copied, onCopy }: {
  url: string;
  copied: boolean;
  onCopy: (transport: McpTransportName, url: string) => void;
}) {
  // An absent pinned projection cannot be reconstructed safely from a public origin.
  if (!url) return null;
  return (
    <div className="rounded-xl border border-slate-200 bg-white p-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xs font-semibold text-slate-700">Streamable HTTP · Version-pinned</span>
        <TransportBadge>Pinned</TransportBadge>
      </div>
      <EndpointRow label="version-pinned Streamable HTTP" transport="versioned_streamable_http" url={url} copied={copied} onCopy={onCopy} />
    </div>
  );
}

/** Groups transitional SSE URLs while preserving stable and pinned identities. */
function LegacyEndpoints({ stableURL, pinnedURL, copied, onCopy }: {
  stableURL: string;
  pinnedURL: string;
  copied: McpTransportName | null;
  onCopy: (transport: McpTransportName, url: string) => void;
}) {
  // Engines without SSE discovery should not render an empty compatibility panel.
  if (!stableURL && !pinnedURL) return null;
  return (
    <details className="group rounded-xl border border-slate-200 bg-white">
      <summary className="flex cursor-pointer list-none items-center justify-between gap-3 px-3 py-2.5 text-xs font-semibold text-slate-600 marker:content-none">
        <span>Legacy compatibility</span>
        <ChevronDown className="h-4 w-4 shrink-0 transition-transform group-open:rotate-180" />
      </summary>
      <div className="border-t border-slate-100 px-3 pb-3 pt-2">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-xs font-semibold text-slate-700">SSE</span>
          <TransportBadge legacy>Legacy</TransportBadge>
        </div>
        {stableURL ? <EndpointRow label="stable SSE" transport="sse" url={stableURL} copied={copied === "sse"} onCopy={onCopy} /> : null}
        {pinnedURL ? <EndpointRow label="version-pinned SSE" transport="versioned_sse" url={pinnedURL} copied={copied === "versioned_sse"} onCopy={onCopy} /> : null}
      </div>
    </details>
  );
}

/** Renders Engine-owned MCP transport discovery without rebuilding endpoint URLs in the browser. */
export function McpTransportEndpoints({ endpoints, enabled = true, onCopied }: McpTransportEndpointsProps) {
  const [copied, setCopied] = useState<McpTransportName | null>(null);

  // A non-runnable exact version must not expose either stable or pinned copy controls.
  if (!enabled) {
    return (
      <div className="rounded-lg border border-slate-200 bg-white px-3 py-2 text-xs italic text-slate-500">
        Server unavailable -- restore it to reconnect
      </div>
    );
  }

  /** Records only the route kind after the browser copies the Engine-owned URL. */
  const copyEndpoint = async (transport: McpTransportName, url: string) => {
    await navigator.clipboard.writeText(url);
    setCopied(transport);
    if (onCopied) onCopied(transport);
  };

  return (
    <div className="space-y-3" data-default-transport={endpoints.default_transport || "unknown"}>
      <RecommendedEndpoint
        url={transportURL(endpoints, "streamable_http")}
        copied={copied === "streamable_http"}
        isDefault={endpoints.default_transport === "streamable_http"}
        isStable={endpoints.stable === true}
        stableVersionID={typeof endpoints.stable_version_id === "string" ? endpoints.stable_version_id : ""}
        onCopy={copyEndpoint}
      />
      <PinnedEndpoint
        url={transportURL(endpoints, "versioned_streamable_http")}
        copied={copied === "versioned_streamable_http"}
        onCopy={copyEndpoint}
      />
      <LegacyEndpoints
        stableURL={transportURL(endpoints, "sse")}
        pinnedURL={transportURL(endpoints, "versioned_sse")}
        copied={copied}
        onCopy={copyEndpoint}
      />
    </div>
  );
}
