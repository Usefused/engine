import { useState } from "react";
import { Check, ChevronDown, Copy } from "lucide-react";

export interface McpTransportURLs {
  streamable_http?: string | null;
  sse?: string | null;
}

export interface McpTransportEndpointData {
  default_transport?: string | null;
  transport_urls?: McpTransportURLs | null;
}

interface McpTransportEndpointsProps {
  endpoints: McpTransportEndpointData;
  enabled?: boolean;
  onCopied?: (transport: "streamable_http" | "sse") => void;
}

function TransportBadge({ children, legacy = false }: { children: string; legacy?: boolean }) {
  const colors = legacy
    ? "border-amber-200 bg-amber-50 text-amber-700"
    : "border-violet-200 bg-violet-50 text-violet-700";
  return <span className={`rounded-full border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${colors}`}>{children}</span>;
}

function EndpointRow({ label, transport, url, copied, onCopy }: {
  label: string;
  transport: "streamable_http" | "sse";
  url: string;
  copied: boolean;
  onCopy: (transport: "streamable_http" | "sse", url: string) => void;
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

function transportURL(endpoints: McpTransportEndpointData, transport: "streamable_http" | "sse"): string {
  const value = endpoints.transport_urls?.[transport];
  return typeof value === "string" ? value.trim() : "";
}

function RecommendedEndpoint({ url, copied, isDefault, onCopy }: {
  url: string;
  copied: boolean;
  isDefault: boolean;
  onCopy: (transport: "streamable_http" | "sse", url: string) => void;
}) {
  return (
    <div className="rounded-xl border border-violet-100 bg-violet-50/40 p-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xs font-semibold text-slate-800">Streamable HTTP</span>
        <TransportBadge>Recommended</TransportBadge>
        {isDefault && <span className="text-[11px] text-slate-500">Engine default</span>}
      </div>
      {url ? (
        <EndpointRow label="Streamable HTTP" transport="streamable_http" url={url} copied={copied} onCopy={onCopy} />
      ) : (
        <p className="mt-2 text-xs text-slate-500">Streamable HTTP endpoint unavailable.</p>
      )}
    </div>
  );
}

function LegacyEndpoint({ url, copied, onCopy }: {
  url: string;
  copied: boolean;
  onCopy: (transport: "streamable_http" | "sse", url: string) => void;
}) {
  if (!url) return null;
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
        <EndpointRow label="SSE" transport="sse" url={url} copied={copied} onCopy={onCopy} />
      </div>
    </details>
  );
}

/** Renders Engine-owned MCP transport discovery without rebuilding endpoint URLs in the browser. */
export function McpTransportEndpoints({ endpoints, enabled = true, onCopied }: McpTransportEndpointsProps) {
  const [copied, setCopied] = useState<"streamable_http" | "sse" | null>(null);

  if (!enabled) {
    return (
      <div className="rounded-lg border border-slate-200 bg-white px-3 py-2 text-xs italic text-slate-500">
        Server unavailable -- restore it to reconnect
      </div>
    );
  }

  const copyEndpoint = async (transport: "streamable_http" | "sse", url: string) => {
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
        onCopy={copyEndpoint}
      />
      <LegacyEndpoint url={transportURL(endpoints, "sse")} copied={copied === "sse"} onCopy={copyEndpoint} />
    </div>
  );
}
