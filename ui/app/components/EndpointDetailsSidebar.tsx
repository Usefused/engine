import { useState } from "react";
import { AlertTriangle, ChevronDown, ChevronUp, Loader2, Copy, Check } from "lucide-react";
import { SchemaViewer } from "~/components/SchemaViewer";
import { type JsonSchemaNode } from "~/components/SchemaViewer";
import {
  type DriftSnapshot,
  type IntegrationObject,
  type RequestContentContract,
  type ResponseContract,
  type SchemaContract,
} from "~/lib/api";
import { stripLinks } from "~/lib/format";
import { typescriptSDKCallExample } from "~/lib/sdk-code-example";

interface ComponentReferenceScope {
  componentScope: string;
  allowRemoteRefs: boolean;
}

// ResponseSchemaViewer renders one canonical response representation on demand.
function ResponseSchemaViewer({ code, schema, serviceId, componentScope, allowRemoteRefs }: {
  code: string;
  schema: JsonSchemaNode;
  serviceId: string;
} & ComponentReferenceScope) {
  const [isOpen, setIsOpen] = useState(false);

  const codeStyle = code.startsWith('2')
    ? { bg: "bg-emerald-50",    text: "text-emerald-700",    border: "border-emerald-200",    dotColor: "bg-emerald-500"    }
    : code.startsWith('4')
    ? { bg: "bg-amber-50",     text: "text-amber-700",      border: "border-amber-200",      dotColor: "bg-amber-500"     }
    : code.startsWith('5')
    ? { bg: "bg-red-50",       text: "text-red-700",        border: "border-red-200",        dotColor: "bg-red-500"       }
    : { bg: "bg-slate-50",     text: "text-slate-700",      border: "border-slate-200",      dotColor: "bg-slate-500"     };

  return (
    <div className="border border-slate-200 rounded-xl overflow-hidden shadow-sm">
      <button
        data-track="toggle_response_schema"
        onClick={() => setIsOpen(!isOpen)}
        className={`w-full px-4 py-2.5 flex items-center justify-between cursor-pointer transition-colors focus:outline-none ${isOpen ? 'bg-slate-50 border-b border-slate-200' : 'bg-white hover:bg-slate-50/50'}`}
      >
        <div className="flex items-center gap-2">
          <span className={`inline-flex items-center gap-1.5 text-[10px] font-bold px-2 py-0.5 rounded-md border uppercase tracking-wider ${codeStyle.bg} ${codeStyle.text} ${codeStyle.border}`}>
            <span className={`w-1.5 h-1.5 rounded-full ${codeStyle.dotColor}`} />
            {code}
          </span>
          <span className="text-xs font-medium text-slate-900">Schema</span>
        </div>
        {isOpen ? <ChevronUp className="w-4 h-4 text-slate-500" /> : <ChevronDown className="w-4 h-4 text-slate-500" />}
      </button>
      {isOpen && (
        <div className="bg-[#161c27] p-4 animate-in fade-in slide-in-from-top-1 duration-200">
          <SchemaViewer schema={schema} serviceId={serviceId} componentScope={componentScope} allowRemoteRefs={allowRemoteRefs} />
        </div>
      )}
    </div>
  );
}

// schemaContractProjection prefers the bounded projection while retaining raw-schema compatibility.
function schemaContractProjection(contract?: SchemaContract): JsonSchemaNode | undefined {
  return contract?.projection || contract?.raw;
}

// representationSchema normalizes object and item contracts into one renderable schema.
function representationSchema(schema?: SchemaContract, itemSchema?: SchemaContract): JsonSchemaNode | undefined {
  const direct = schemaContractProjection(schema);
  if (direct) return direct;
  const item = schemaContractProjection(itemSchema);
  return item ? { type: "array", items: item } : undefined;
}

// requestContentSchema selects the declared default media type before falling back to source order.
function requestContentSchema(content?: RequestContentContract): JsonSchemaNode | undefined {
  const representations = content?.representations || [];
  const selected = representations.find((item) => item.media_type === content?.default_media_type) || representations[0];
  return representationSchema(selected?.schema, selected?.item_schema);
}

interface CanonicalResponseSchema {
  key: string;
  label: string;
  schema: JsonSchemaNode;
}

// canonicalResponseSchemas preserves every v3 response media alternative.
function canonicalResponseSchemas(responses?: Record<string, ResponseContract>): CanonicalResponseSchema[] {
  const result: CanonicalResponseSchema[] = [];
  for (const [code, response] of Object.entries(responses || {})) {
    for (const [index, representation] of (response.representations || []).entries()) {
      const schema = representationSchema(representation.schema, representation.item_schema);
      if (!schema) continue;
      const mediaType = representation.media_type || `representation-${index + 1}`;
      result.push({ key: `${code}:${mediaType}:${index}`, label: `${code} · ${mediaType}`, schema });
    }
  }
  return result;
}

// responseContractsMatch checks canonical response media without reverting to legacy schema fields.
function responseContractsMatch(responses: Record<string, ResponseContract> | undefined, predicate: (response: ResponseContract) => boolean): boolean {
  return Object.values(responses || {}).some(predicate);
}

// operationDisplayType derives the badge from canonical protocol and response contracts.
function operationDisplayType(endpoint: IntegrationObject): string {
  if (endpoint.isWebhook) return "Webhook";
  if (endpoint.provider_protocol === "graphql" || endpoint.graphql_query || endpoint.method === "GRAPHQL") return "GraphQL";
  if (responseContractsMatch(endpoint.responses, (response) => response.representations?.some((item) => item.media_type === "application/octet-stream") === true)) return "File Transfer";
  if (responseContractsMatch(endpoint.responses, (response) => response.representations?.some((item) => Boolean(item.sse)) === true)) return "Event Stream";
  return "REST API";
}

// OperationMetadata surfaces persisted v3 identity and policy instead of hiding it in the raw response.
function OperationMetadata({ endpoint }: { endpoint: IntegrationObject }) {
  const facts = [
    ["Operation ID", endpoint.name],
    endpoint.provider_protocol && ["Protocol", endpoint.provider_protocol],
    endpoint.operation_kind && ["Operation", endpoint.operation_kind],
    endpoint.normalized_path && ["Normalized path", endpoint.normalized_path],
    endpoint.stable_key && ["Stable key", endpoint.stable_key],
  ].filter(Boolean) as string[][];
  const hasPolicy = Boolean(endpoint.pagination || endpoint.security_requirements?.length);
  if (facts.length === 0 && !hasPolicy) return null;
  return (
    <div className="mt-2 space-y-2 text-xs text-slate-500">
      {facts.map(([label, value]) => (
        <div key={label} className="flex min-w-0 gap-2">
          <span className="shrink-0 font-medium text-slate-600">{label}</span>
          <code className="min-w-0 break-all text-slate-500">{value}</code>
        </div>
      ))}
      {hasPolicy && (
        <details>
          <summary className="cursor-pointer font-medium text-slate-600">Execution metadata</summary>
          <pre className="mt-2 max-h-48 overflow-auto rounded-md bg-slate-950 p-3 text-[10px] text-slate-200">
            {JSON.stringify({ pagination: endpoint.pagination, security_requirements: endpoint.security_requirements }, null, 2)}
          </pre>
        </details>
      )}
    </div>
  );
}

export interface EndpointDetailsSidebarProps {
  selectedEndpoint: IntegrationObject;
  setSelectedEndpoint: (ep: IntegrationObject | null) => void;
  srv: { id: string; name: string };
  drift: DriftSnapshot[];
  driftAction: string | null;
  handleDismiss: (id: string) => void;
	handleApply: (id: string) => void;
  componentScope: string;
  allowRemoteRefs: boolean;
}

// EndpointDetailsSidebar presents an exact-version operation contract and its drift state.
export default function EndpointDetailsSidebar({
  selectedEndpoint,
  setSelectedEndpoint,
  srv,
  drift,
  driftAction,
	handleDismiss,
	handleApply,
  componentScope,
  allowRemoteRefs,
}: EndpointDetailsSidebarProps) {
  const [copiedPath, setCopiedPath] = useState(false);
  const isLoading = !selectedEndpoint._detailsLoaded && !selectedEndpoint.isWebhook;
  const endpointType = operationDisplayType(selectedEndpoint);
  const requestSchema = selectedEndpoint.isWebhook
    ? selectedEndpoint.request_body
    : requestContentSchema(selectedEndpoint.request_content);
  const responseSchemas = canonicalResponseSchemas(selectedEndpoint.responses);
  const endpointDrift = drift.filter((snapshot) => snapshot.integration_object_id === selectedEndpoint.id);

  return (
    <>
      <div className="fixed inset-0 bg-slate-900/20 z-40 transition-opacity" onClick={() => setSelectedEndpoint(null)} />
      <div className="fixed inset-y-0 right-0 w-full md:w-[600px] bg-white shadow-2xl z-50 overflow-y-auto overflow-x-hidden transform transition-transform border-l border-slate-200 flex flex-col">
        <EndpointSidebarHeader endpoint={selectedEndpoint} endpointType={endpointType} copiedPath={copiedPath} setCopiedPath={setCopiedPath} close={() => setSelectedEndpoint(null)} />
        <EndpointDriftPanel snapshots={endpointDrift} driftAction={driftAction} handleDismiss={handleDismiss} handleApply={handleApply} />
        <EndpointContractContent endpoint={selectedEndpoint} isLoading={isLoading} requestSchema={requestSchema} responseSchemas={responseSchemas} serviceId={srv.id} serviceName={srv.name} componentScope={componentScope} allowRemoteRefs={allowRemoteRefs} />
      </div>
    </>
  );
}

// methodBadgeClass maps operation verbs to stable header colors.
function methodBadgeClass(endpoint: IntegrationObject): string {
  if (endpoint.isWebhook || endpoint.method === "SOAP") return "bg-purple-100 text-purple-700";
  if (endpoint.method === "GET") return "bg-green-100 text-green-700";
  if (endpoint.method === "POST") return "bg-blue-100 text-blue-700";
  if (endpoint.method === "DELETE") return "bg-red-100 text-red-700";
  return "bg-slate-100 text-slate-700";
}

// endpointTypeClass maps operation contract kinds to a secondary badge.
function endpointTypeClass(endpointType: string): string {
  if (endpointType === "GraphQL") return "bg-pink-50 text-pink-700 border-pink-200";
  if (endpointType === "File Transfer") return "bg-indigo-50 text-indigo-700 border-indigo-200";
  if (endpointType === "Event Stream") return "bg-cyan-50 text-cyan-700 border-cyan-200";
  return "bg-slate-50 text-slate-500 border-slate-200";
}

// EndpointSidebarHeader renders identity, copy, and close controls.
function EndpointSidebarHeader({ endpoint, endpointType, copiedPath, setCopiedPath, close }: {
  endpoint: IntegrationObject;
  endpointType: string;
  copiedPath: boolean;
  setCopiedPath: (value: boolean) => void;
  close: () => void;
}) {
  // copyPath provides brief feedback without changing the selected operation.
  function copyPath(event: React.MouseEvent) {
    event.stopPropagation();
    navigator.clipboard.writeText(endpoint.path);
    setCopiedPath(true);
    setTimeout(() => setCopiedPath(false), 2000);
  }
  return (
    <div className="sticky top-0 z-10 flex min-w-0 items-center justify-between gap-2 border-b border-slate-100 bg-white/90 p-4 backdrop-blur sm:p-6">
      <div className="flex min-w-0 flex-1 flex-wrap items-center gap-2">
        <span className={`shrink-0 rounded px-2 py-1 text-xs font-bold ${methodBadgeClass(endpoint)}`}>{endpoint.isWebhook ? "WEBHOOK" : endpoint.method}</span>
        <code className="min-w-0 flex-1 break-all text-sm text-slate-800">{endpoint.path}</code>
        <button data-track="copy_endpoint_path" onClick={copyPath} className="rounded p-1 text-slate-400 hover:bg-slate-100" title="Copy Path">
          {copiedPath ? <Check className="h-3.5 w-3.5 text-green-500" /> : <Copy className="h-3.5 w-3.5" />}
        </button>
        {!endpoint.isWebhook && <span className={`rounded border px-1.5 py-0.5 text-[10px] font-bold uppercase ${endpointTypeClass(endpointType)}`}>{endpointType}</span>}
        {endpoint.deprecated && <span className="rounded border border-red-200 bg-red-50 px-2 py-0.5 text-xs font-semibold uppercase text-red-700">Deprecated</span>}
      </div>
      <button data-track="close_endpoint_sidebar" onClick={close} className="rounded-full p-2 text-slate-400 hover:bg-slate-100">✕</button>
    </div>
  );
}

// EndpointDriftPanel renders only drift snapshots belonging to the selected operation.
function EndpointDriftPanel({ snapshots, driftAction, handleDismiss, handleApply }: {
  snapshots: DriftSnapshot[];
  driftAction: string | null;
  handleDismiss: (id: string) => void;
  handleApply: (id: string) => void;
}) {
  if (snapshots.length === 0) return null;
  return (
    <div className="border-b border-orange-100 bg-orange-50 p-4 sm:p-6">
      <h3 className="mb-4 flex items-center gap-2 text-sm font-bold text-orange-900"><AlertTriangle className="h-4 w-4" />Pending Semantic Drift Detected</h3>
      {snapshots.map((snapshot) => (
        <div key={snapshot.id} className="mb-4 space-y-3">
          <div className="flex items-center justify-between gap-2">
            <p className="text-xs text-orange-800/80">Detected at {new Date(snapshot.detected_at).toLocaleString()}</p>
            <div className="flex gap-2">
              <button data-track="dismiss_drift_change" onClick={() => handleDismiss(snapshot.id)} disabled={driftAction === snapshot.id} className="rounded border border-orange-200 bg-white px-3 py-1.5 text-xs font-semibold text-orange-800">Dismiss</button>
              <button data-track="review_drift_import" onClick={() => handleApply(snapshot.id)} disabled={driftAction === snapshot.id} className="rounded bg-orange-600 px-3 py-1.5 text-xs font-semibold text-white">Review Import</button>
            </div>
          </div>
          {snapshot.diff.map((change, index) => <DriftChangeCard key={`${change.field}:${index}`} change={change} />)}
        </div>
      ))}
    </div>
  );
}

// DriftChangeCard compares one previous and current contract value.
function DriftChangeCard({ change }: { change: DriftSnapshot["diff"][number] }) {
  return (
    <div className="overflow-hidden rounded-lg border border-orange-200 bg-white">
      <div className="flex items-center justify-between bg-orange-100/50 px-3 py-2">
        <code className="text-xs font-bold text-orange-900">{change.field}</code>
        {change.severity === "breaking" && <span className="rounded bg-red-100 px-1.5 py-0.5 text-[10px] font-bold uppercase text-red-600">Breaking</span>}
      </div>
      {change.description && <p className="border-t border-orange-100 px-3 py-2 text-xs text-orange-800">{change.description}</p>}
      <div className="grid grid-cols-1 sm:grid-cols-2">
        <ContractValue label="Previous" value={change.old_value} className="bg-red-50/30 text-red-900" />
        <ContractValue label="New" value={change.new_value} className="bg-green-50/30 text-green-900" />
      </div>
    </div>
  );
}

// ContractValue formats arbitrary drift values without losing structured data.
function ContractValue({ label, value, className }: { label: string; value: unknown; className: string }) {
  const rendered = typeof value === "object" ? JSON.stringify(value, null, 2) : String(value ?? "null");
  return <div className={`p-3 ${className}`}><div className="mb-1 text-[10px] font-semibold uppercase text-slate-400">{label}</div><pre className="max-h-[400px] overflow-auto whitespace-pre-wrap break-words text-xs">{rendered}</pre></div>;
}

// EndpointContractContent switches between loading and exact contract sections.
function EndpointContractContent({ endpoint, isLoading, requestSchema, responseSchemas, serviceId, serviceName, componentScope, allowRemoteRefs }: {
  endpoint: IntegrationObject;
  isLoading: boolean;
  requestSchema?: JsonSchemaNode;
  responseSchemas: CanonicalResponseSchema[];
  serviceId: string;
  serviceName: string;
} & ComponentReferenceScope) {
  if (isLoading) return <div className="flex min-h-[300px] flex-1 items-center justify-center text-slate-400"><Loader2 className="mr-3 h-8 w-8 animate-spin text-blue-500" />Loading endpoint details...</div>;
  return (
    <div className="flex flex-1 flex-col space-y-8 p-4 sm:p-6">
      <EndpointSummary endpoint={endpoint} />
      <GeneratedSDKExample endpoint={endpoint} serviceName={serviceName} requestSchema={requestSchema} />
      <ParameterTable endpoint={endpoint} />
      <RequestSchemaSection endpoint={endpoint} schema={requestSchema} serviceId={serviceId} componentScope={componentScope} allowRemoteRefs={allowRemoteRefs} />
      <ResponseSchemasSection responses={responseSchemas} serviceId={serviceId} componentScope={componentScope} allowRemoteRefs={allowRemoteRefs} />
    </div>
  );
}

// EndpointSummary displays descriptive and deprecation metadata.
function EndpointSummary({ endpoint }: { endpoint: IntegrationObject }) {
  const deprecation = endpoint.deprecation_date
    ? `This endpoint will be removed on ${new Date(endpoint.deprecation_date).toLocaleDateString()}.`
    : "This endpoint is deprecated and should no longer be used.";
  return <div><h2 className="text-lg font-semibold text-slate-900">{endpoint.name}</h2><OperationMetadata endpoint={endpoint} />{(endpoint.deprecated || endpoint.deprecation_date) && <p className="mt-3 rounded-lg border border-red-200 bg-red-50 p-3 text-xs text-red-700">{deprecation}</p>}{endpoint.description && <div className="mt-2 text-sm text-slate-600" dangerouslySetInnerHTML={{ __html: stripLinks(endpoint.description) }} />}</div>;
}

// GeneratedSDKExample connects the exact operationId to a copyable generated
// TypeScript call without exposing credentials or provider-derived examples.
function GeneratedSDKExample({ endpoint, serviceName, requestSchema }: { endpoint: IntegrationObject; serviceName: string; requestSchema?: JsonSchemaNode }) {
  const [copied, setCopied] = useState(false);
  const code = typescriptSDKCallExample(serviceName, endpoint, requestSchema);

  // copyExample reports a short-lived local state change and never sends the
  // generated snippet or operation metadata to another service.
  function copyExample() {
    navigator.clipboard.writeText(code);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  }

  return (
    <details className="overflow-hidden rounded-lg border border-slate-200 bg-white">
      <summary className="flex cursor-pointer list-none items-center justify-between px-3 py-2.5 text-xs font-semibold text-slate-700 hover:bg-slate-50">
        <span>Generated SDK example</span>
        <span className="rounded bg-blue-50 px-1.5 py-0.5 text-[10px] font-semibold text-blue-700">TypeScript</span>
      </summary>
      <div className="relative border-t border-slate-200 bg-slate-950">
        <button onClick={copyExample} className="absolute right-2 top-2 rounded p-1.5 text-slate-400 hover:bg-slate-800 hover:text-white" title="Copy generated example" aria-label="Copy generated SDK example">
          {copied ? <Check className="h-3.5 w-3.5 text-emerald-400" /> : <Copy className="h-3.5 w-3.5" />}
        </button>
        <pre className="overflow-x-auto p-4 pr-10 text-[11px] leading-5 text-slate-200"><code>{code}</code></pre>
      </div>
    </details>
  );
}

type EndpointParameter = NonNullable<IntegrationObject["parameters"]>[number];

// ParameterEncodingNote surfaces only an explicit non-default wire decision so
// ordinary textual parameters are not burdened with a meaningless default.
function ParameterEncodingNote({ pathEncoding }: { pathEncoding?: string }) {
  if (!pathEncoding) return null;
  // Slash preservation is the only currently supported override and deserves
  // product language instead of its internal contract identifier.
  const label = pathEncoding === "preserve_slashes"
    ? "Preserves slashes"
    : `Encoding: ${pathEncoding.replace(/[_-]+/g, " ")}`;
  return <span className="mt-1 block break-words font-sans text-[10px] text-slate-500">{label}</span>;
}

// ParameterRequirementBadge makes required state scannable without consuming
// the width of separate Yes and No values.
function ParameterRequirementBadge({ required }: { required: boolean }) {
  const style = required
    ? "bg-blue-50 text-blue-700 ring-blue-200"
    : "bg-slate-50 text-slate-500 ring-slate-200";
  return <span className={`inline-flex rounded-full px-2 py-0.5 text-[10px] font-semibold ring-1 ring-inset ${style}`}>{required ? "Required" : "Optional"}</span>;
}

// ParameterRow keeps long identifiers contained while retaining the complete
// name in both visible wrapped text and the native hover tooltip.
function ParameterRow({ parameter }: { parameter: EndpointParameter }) {
  return (
    <tr className="border-t border-slate-200 align-top">
      <td className="w-[46%] px-3 py-3 sm:px-4">
        <code className="break-all text-xs text-slate-900" title={parameter.name}>{parameter.name}</code>
        <ParameterEncodingNote pathEncoding={parameter.path_encoding} />
      </td>
      <td className="w-[18%] px-2 py-3 text-xs capitalize text-slate-600 sm:px-4">{parameter.in}</td>
      <td className="w-[18%] break-all px-2 py-3 font-mono text-xs text-slate-700 sm:px-4">{parameter.type}</td>
      <td className="w-[18%] px-2 py-3 sm:px-4"><ParameterRequirementBadge required={parameter.required} /></td>
    </tr>
  );
}

// ParameterTable presents exact type and location data without forcing the
// sidebar into a horizontally scrolling desktop-width table.
function ParameterTable({ endpoint }: { endpoint: IntegrationObject }) {
  if (!endpoint.parameters?.length) return null;
  return (
    <div>
      <h3 className="mb-3 border-b pb-2 text-sm font-semibold">Parameters</h3>
      <div className="overflow-hidden rounded-lg border border-slate-200">
        <table className="w-full table-fixed text-left text-sm">
          <thead className="bg-slate-100 text-xs text-slate-700">
            <tr>
              <th className="w-[46%] px-3 py-2 sm:px-4">Name</th>
              <th className="w-[18%] px-2 py-2 sm:px-4">Location</th>
              <th className="w-[18%] px-2 py-2 sm:px-4">Type</th>
              <th className="w-[18%] px-2 py-2 sm:px-4">Requirement</th>
            </tr>
          </thead>
          <tbody>{endpoint.parameters.map((parameter) => <ParameterRow key={`${parameter.in}:${parameter.name}`} parameter={parameter} />)}</tbody>
        </table>
      </div>
    </div>
  );
}

// RequestSchemaSection renders the canonical default request representation.
function RequestSchemaSection({ endpoint, schema, serviceId, componentScope, allowRemoteRefs }: { endpoint: IntegrationObject; schema?: JsonSchemaNode; serviceId: string } & ComponentReferenceScope) {
  const [open, setOpen] = useState(false);
  if (!schema) return null;
  return <div><button data-track="toggle_request_schema" onClick={() => setOpen(!open)} className="flex w-full justify-between rounded border p-3 text-xs font-medium"><span>{endpoint.isWebhook ? "Event Schema" : "Request Schema"}</span>{open ? <ChevronUp className="h-4 w-4" /> : <ChevronDown className="h-4 w-4" />}</button>{open && <div className="bg-[#161c27] p-4"><SchemaViewer schema={schema} serviceId={serviceId} componentScope={componentScope} allowRemoteRefs={allowRemoteRefs} /></div>}</div>;
}

// ResponseSchemasSection renders every canonical response media representation.
function ResponseSchemasSection({ responses, serviceId, componentScope, allowRemoteRefs }: { responses: CanonicalResponseSchema[]; serviceId: string } & ComponentReferenceScope) {
  if (responses.length === 0) return null;
  return <div><h3 className="mb-3 border-b pb-2 text-sm font-semibold">Responses</h3><div className="space-y-4">{responses.map((response) => <ResponseSchemaViewer key={response.key} code={response.label} schema={response.schema} serviceId={serviceId} componentScope={componentScope} allowRemoteRefs={allowRemoteRefs} />)}</div></div>;
}
