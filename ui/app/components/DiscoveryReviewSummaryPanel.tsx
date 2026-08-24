import { ChevronDown, Globe2, KeyRound, Loader2, ShieldCheck } from "lucide-react";
import type { ReactNode } from "react";
import { METHOD_COLORS } from "~/components/EndpointRow";
import type {
  DiscoveryReviewAuthScheme,
  DiscoveryReviewListCounts,
  DiscoveryReviewOperation,
  DiscoveryReviewSecurityAlternative,
  DiscoveryReviewSummary,
} from "~/lib/api";

interface DiscoveryReviewSummaryPanelProps {
  summary: DiscoveryReviewSummary | null;
  loading: boolean;
  error: string;
}

// DiscoveryReviewSummaryPanel renders only the Registry's bounded structural projection.
export default function DiscoveryReviewSummaryPanel({ summary, loading, error }: DiscoveryReviewSummaryPanelProps) {
  if (loading) {
    return <div className="rounded-xl border border-slate-200 bg-white p-5 text-sm text-slate-500 flex items-center gap-3"><Loader2 className="w-4 h-4 animate-spin text-[var(--brand-violet)]" />Loading the contract review summary...</div>;
  }
  if (error) {
    return <div className="rounded-xl border border-red-200 bg-red-50 p-5 text-sm text-red-700"><strong>Contract summary unavailable</strong><p className="mt-1">{error}</p></div>;
  }
  if (!summary) {
    return <div className="rounded-xl border border-slate-200 bg-white p-5 text-sm text-slate-500">Waiting for the immutable contract summary...</div>;
  }
  return (
    <div className="rounded-2xl border border-slate-200 bg-white overflow-hidden">
      <div className="p-5 border-b border-slate-100">
        <div className="flex items-start justify-between gap-4">
          <div><h3 className="font-semibold text-slate-900">{summary.info.title}</h3><p className="text-xs text-slate-500 mt-1">Contract version {summary.info.version || "not declared"}</p></div>
          <div className="text-right text-[10px] uppercase tracking-wide text-slate-400"><div>{summary.evidence_count} evidence item{summary.evidence_count === 1 ? "" : "s"}</div><div>{summary.diagnostic_count} review note{summary.diagnostic_count === 1 ? "" : "s"}</div></div>
        </div>
      </div>
      <div className="p-5 space-y-6">
        <ReviewServers summary={summary} />
        <ReviewAuthSchemes summary={summary} />
        <ReviewOperations summary={summary} />
      </div>
    </div>
  );
}

// ReviewServers presents only credential-free server URLs admitted by Registry sanitization.
function ReviewServers({ summary }: { summary: DiscoveryReviewSummary }) {
  const servers = summary.servers || [];
  return (
    <ReviewSection icon={<Globe2 className="w-4 h-4" />} title="Servers" counts={summary.server_counts}>
      {servers.length === 0 ? <EmptyReviewValue>None safely declared</EmptyReviewValue> : <div className="space-y-2">{servers.map((server) => <div key={server.url} className="rounded-lg bg-slate-50 px-3 py-2"><code className="text-xs break-all text-slate-800">{server.url}</code>{(server.environment || server.description) && <p className="text-[11px] text-slate-500 mt-1">{[server.environment, server.description].filter(Boolean).join(" · ")}</p>}</div>)}</div>}
    </ReviewSection>
  );
}

// ReviewAuthSchemes explains protocol behavior without rendering credentials or extension parameter values.
function ReviewAuthSchemes({ summary }: { summary: DiscoveryReviewSummary }) {
  const schemes = summary.auth_schemes || [];
  return (
    <ReviewSection icon={<KeyRound className="w-4 h-4" />} title="Authentication" counts={summary.auth_scheme_counts}>
      {schemes.length === 0 ? <EmptyReviewValue>No named authentication scheme</EmptyReviewValue> : <div className="grid gap-2">{schemes.map((scheme) => <ReviewAuthScheme key={`${scheme.name}:${scheme.type}`} scheme={scheme} />)}</div>}
    </ReviewSection>
  );
}

// ReviewAuthScheme renders public wire metadata and bounded OAuth flow names.
function ReviewAuthScheme({ scheme }: { scheme: DiscoveryReviewAuthScheme }) {
  const protocol = [scheme.type, scheme.scheme, scheme.location && `${scheme.location}${scheme.key_name ? ` · ${scheme.key_name}` : ""}`].filter(Boolean).join(" · ");
  const flows = scheme.oauth_flows || [];
  return <div className="rounded-lg border border-slate-100 p-3"><div className="font-mono text-xs text-slate-800">{scheme.name}</div><p className="text-[11px] text-slate-500 mt-1">{protocol}</p>{flows.length > 0 && <div className="mt-2 flex flex-wrap gap-1.5">{flows.map((flow) => <span key={flow.type} className="px-2 py-1 rounded bg-violet-50 text-violet-700 text-[10px]">{flow.type}{flow.scopes?.length ? ` · ${flow.scopes.join(", ")}` : ""}</span>)}</div>}</div>;
}

// ReviewOperations renders stable method/path identities and shallow contract structure.
function ReviewOperations({ summary }: { summary: DiscoveryReviewSummary }) {
  const operations = summary.operations || [];
  return (
    <ReviewSection icon={<ShieldCheck className="w-4 h-4" />} title="Operations" counts={summary.operation_counts}>
      {operations.length === 0 ? <EmptyReviewValue>No REST operations</EmptyReviewValue> : <div className="space-y-3">{operations.map((operation) => <ReviewOperation key={`${operation.method}:${operation.path}:${operation.operation_id}`} operation={operation} />)}</div>}
    </ReviewSection>
  );
}

// ReviewOperation keeps the service-detail operation identity visible while
// placing bounded contract facts behind an initially closed disclosure.
function ReviewOperation({ operation }: { operation: DiscoveryReviewOperation }) {
  const parameters = operation.parameters || [];
  const responses = operation.responses || [];
  const method = operation.method.toUpperCase();
  return (
    <details className="group overflow-hidden rounded-xl border border-slate-200 bg-white">
      <summary className="flex cursor-pointer list-none items-start justify-between gap-3 px-4 py-4 transition-colors hover:bg-slate-50 [&::-webkit-details-marker]:hidden">
        <div className="min-w-0 flex-1">
          <div className="flex min-w-0 items-start gap-3 sm:items-center">
            <span className={`shrink-0 rounded px-1.5 py-0.5 text-xs font-bold ${METHOD_COLORS[method] ?? "bg-slate-100 text-slate-700 border-slate-200"}`}>{method}</span>
            <code className="min-w-0 break-all text-sm text-slate-700">{operation.path}</code>
          </div>
          <div className="mt-1.5 break-all font-mono text-xs text-slate-500">{operation.operation_id}</div>
        </div>
        <ChevronDown className="mt-0.5 h-4 w-4 shrink-0 text-slate-400 transition-transform group-open:rotate-180" />
      </summary>
      <div className="border-t border-slate-100 px-4 pb-4 pt-3">
        {operation.summary && <p className="text-xs text-slate-600">{operation.summary}</p>}
        <div className={`${operation.summary ? "mt-3" : ""} grid gap-3 text-[11px] md:grid-cols-2`}>
          <ReviewFact label="Parameters" counts={operation.parameter_counts} values={parameters.map((parameter) => `${parameter.in}:${parameter.name}${parameter.required ? "*" : ""}`)} />
          <ReviewFact label="Request media" counts={operation.request_media_type_counts} values={operation.request_media_types || []} />
          <ReviewFact label="Responses" counts={operation.response_counts} values={responses.map((response) => `${response.status}${response.media_types?.length ? ` · ${response.media_types.join(", ")}` : ""}`)} />
          <ReviewFact label="Security" counts={operation.security_alternative_counts} values={(operation.security || []).map(reviewSecurityLabel)} />
        </div>
      </div>
    </details>
  );
}

// reviewSecurityLabel converts one already-bounded OR alternative into concise public protocol text.
function reviewSecurityLabel(alternative: DiscoveryReviewSecurityAlternative): string {
  if (alternative.anonymous) return "Anonymous";
  return (alternative.schemes || []).map((scheme) => `${scheme.name}${scheme.scopes?.length ? ` (${scheme.scopes.join(", ")})` : ""}`).join(" + ");
}

// ReviewFact pairs bounded values with explicit omitted counts.
function ReviewFact({ label, values, counts }: { label: string; values: string[]; counts: DiscoveryReviewListCounts }) {
  return <div className="rounded-lg bg-slate-50 p-3"><div className="font-semibold text-slate-700">{label}</div><div className="mt-1 text-slate-500 break-words">{values.length > 0 ? values.join(" · ") : "None"}</div><OmittedReviewCount counts={counts} /></div>;
}

// ReviewSection keeps each bounded collection's total and omission signal visible.
function ReviewSection({ icon, title, counts, children }: { icon: ReactNode; title: string; counts: DiscoveryReviewListCounts; children: ReactNode }) {
  return <section><div className="flex items-center justify-between gap-3 mb-2"><h4 className="flex items-center gap-2 text-sm font-semibold text-slate-800">{icon}{title}</h4><span className="text-[10px] text-slate-400">{counts.returned} of {counts.total}</span></div>{children}<OmittedReviewCount counts={counts} /></section>;
}

// OmittedReviewCount states truncation without inventing details for omitted provider structure.
function OmittedReviewCount({ counts }: { counts: DiscoveryReviewListCounts }) {
  if (counts.omitted === 0) return null;
  return <p className="mt-2 text-[10px] text-amber-700">{counts.omitted} additional item{counts.omitted === 1 ? "" : "s"} omitted by the review limit.</p>;
}

// EmptyReviewValue distinguishes an admitted empty collection from a loading fallback.
function EmptyReviewValue({ children }: { children: ReactNode }) {
  return <p className="text-xs text-slate-400">{children}</p>;
}
