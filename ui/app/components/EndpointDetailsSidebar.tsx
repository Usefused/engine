import { OperationDetailsHeader, OperationParameterTable, OperationCodeExample, OperationDetailsPanel, OperationDetailsBody, OperationSummary, OperationTechnicalDetails, OperationSection, OperationSchemaDisclosure } from "~/components/OperationDetailsParts";
import { AlertTriangle, Loader2, Info } from "lucide-react";
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
import { webhookSchemaPending } from "~/lib/webhook-schema";

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
  // Status families retain the endpoint inspector’s semantic response colors.
  const codeStyle = code.startsWith('2')
    ? { bg: "bg-emerald-50",    text: "text-emerald-700",    border: "border-emerald-200",    dotColor: "bg-emerald-500"    }
    : code.startsWith('4')
    ? { bg: "bg-amber-50",     text: "text-amber-700",      border: "border-amber-200",      dotColor: "bg-amber-500"     }
    : code.startsWith('5')
    ? { bg: "bg-red-50",       text: "text-red-700",        border: "border-red-200",        dotColor: "bg-red-500"       }
    : { bg: "bg-slate-50",     text: "text-slate-700",      border: "border-slate-200",      dotColor: "bg-slate-500"     };

  return <OperationSchemaDisclosure track="toggle_response_schema" label="Schema" badge={
    <span className={`inline-flex items-center gap-1.5 rounded-md border px-2 py-0.5 text-[10px] font-bold uppercase tracking-wider ${codeStyle.bg} ${codeStyle.text} ${codeStyle.border}`}>
      <span className={`h-1.5 w-1.5 rounded-full ${codeStyle.dotColor}`} />{code}
    </span>
  }>
      <SchemaViewer schema={schema} serviceId={serviceId} componentScope={componentScope} allowRemoteRefs={allowRemoteRefs} />
  </OperationSchemaDisclosure>;
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

/** Keep exact technical identity available without repeating it in the operation overview. */
function OperationMetadata({ endpoint }: { endpoint: IntegrationObject }) {
  // Only declared facts appear; absence must not imply a protocol or policy default.
  const facts = [
    ["Operation ID", endpoint.name],
    endpoint.provider_protocol && ["Protocol", endpoint.provider_protocol.toUpperCase()],
    endpoint.operation_kind && ["Operation", endpoint.operation_kind],
    endpoint.normalized_path && ["Normalized path", endpoint.normalized_path],
    endpoint.stable_key && ["Stable key", endpoint.stable_key],
  ].filter(Boolean) as [string, string][];
  const hasPolicy = Boolean(endpoint.pagination || endpoint.security_requirements?.length);
  // An empty technical disclosure provides no useful inspection target.
  if (facts.length === 0 && !hasPolicy) return null;
  return <OperationTechnicalDetails facts={facts}>
      {/* Raw policy remains inspectable on demand rather than dominating the summary. */}
      {hasPolicy && <OperationCodeExample label="Execution policy" language="JSON" code={JSON.stringify({ pagination: endpoint.pagination, security_requirements: endpoint.security_requirements }, null, 2)} copyLabel="Copy execution policy" />}
  </OperationTechnicalDetails>;
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
  const isLoading = !selectedEndpoint._detailsLoaded && !selectedEndpoint.isWebhook;
  const endpointType = operationDisplayType(selectedEndpoint);
  const requestSchema = selectedEndpoint.isWebhook
    ? selectedEndpoint.request_body
    : requestContentSchema(selectedEndpoint.request_content);
  const responseSchemas = canonicalResponseSchemas(selectedEndpoint.responses);
  const endpointDrift = drift.filter((snapshot) => snapshot.integration_object_id === selectedEndpoint.id);

  return <OperationDetailsPanel label={`Operation ${selectedEndpoint.name}`} onClose={/** Dismiss without changing the selected service version. */ () => setSelectedEndpoint(null)} header={<EndpointSidebarHeader endpoint={selectedEndpoint} endpointType={endpointType} close={/** Header dismissal shares the panel lifecycle. */ () => setSelectedEndpoint(null)} />}>
    <EndpointDriftPanel snapshots={endpointDrift} driftAction={driftAction} handleDismiss={handleDismiss} handleApply={handleApply} />
    <EndpointContractContent endpoint={selectedEndpoint} isLoading={isLoading} requestSchema={requestSchema} responseSchemas={responseSchemas} serviceId={srv.id} serviceName={srv.name} componentScope={componentScope} allowRemoteRefs={allowRemoteRefs} />
  </OperationDetailsPanel>;
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

// EndpointSidebarHeader adapts physical endpoint identity to the shared inspector header.
function EndpointSidebarHeader({ endpoint, endpointType, close }: {
  endpoint: IntegrationObject;
  endpointType: string;
  close: () => void;
}) {
  // Webhooks have their own primary badge; ordinary operations retain their HTTP method.
  const badge = endpoint.isWebhook ? "WEBHOOK" : endpoint.method;
  // The webhook badge already identifies its kind, so it needs no secondary label.
  const kind = endpoint.isWebhook ? undefined : endpointType;
  return <OperationDetailsHeader badge={badge} badgeClass={methodBadgeClass(endpoint)} identity={endpoint.path}
    copyTrack="copy_endpoint_path" closeTrack="close_endpoint_sidebar" copyLabel="Copy Path" kind={kind} kindClass={endpointTypeClass(endpointType)} deprecated={endpoint.deprecated} onClose={close} />;
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
              <button data-track="dismiss_drift_change" onClick={() => handleDismiss(snapshot.id)} disabled={driftAction === snapshot.id} className="rounded border border-slate-300 bg-white px-3 py-1.5 text-xs font-semibold text-slate-950">Dismiss</button>
              <button data-track="review_drift_import" onClick={() => handleApply(snapshot.id)} disabled={driftAction === snapshot.id} className="rounded border border-slate-300 bg-white px-3 py-1.5 text-xs font-semibold text-slate-700">Review Import</button>
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
  // Defer contract presentation until the selected version has finished loading.
  if (isLoading) return <div className="flex min-h-[300px] flex-1 items-center justify-center text-slate-400"><Loader2 className="mr-3 h-8 w-8 animate-spin text-blue-500" />Loading endpoint details...</div>;
  return (
    <OperationDetailsBody>
      <EndpointSummary endpoint={endpoint} />
      <GeneratedSDKExample endpoint={endpoint} serviceName={serviceName} requestSchema={requestSchema} />
      {/* Missing parameter contracts stay empty rather than inventing fields. */}
      <OperationParameterTable parameters={endpoint.parameters ?? []} />
      <RequestSchemaSection endpoint={endpoint} schema={requestSchema} serviceId={serviceId} componentScope={componentScope} allowRemoteRefs={allowRemoteRefs} />
      <ResponseSchemasSection responses={responseSchemas} serviceId={serviceId} componentScope={componentScope} allowRemoteRefs={allowRemoteRefs} />
    </OperationDetailsBody>
  );
}

/** Lead with the operation's purpose and keep implementation metadata behind an optional disclosure. */
function EndpointSummary({ endpoint }: { endpoint: IntegrationObject }) {
  // A dated deprecation is more actionable than the generic retired-operation notice.
  const deprecation = endpoint.deprecation_date
    ? `This endpoint will be removed on ${new Date(endpoint.deprecation_date).toLocaleDateString()}.`
    : "This endpoint is deprecated and should no longer be used.";
  return <OperationSummary title={endpoint.name}
    // Render only the supplied description and explicit deprecation notice.
    description={endpoint.description && <div dangerouslySetInnerHTML={{ __html: stripLinks(endpoint.description) }} />}
    notice={(endpoint.deprecated || endpoint.deprecation_date) && <p className="rounded-lg bg-red-50 p-3 text-xs leading-5 text-red-700">{deprecation}</p>}>
    <OperationMetadata endpoint={endpoint} />
  </OperationSummary>;
}

// GeneratedSDKExample displays the matching generated call or subscription
// without exposing credentials or provider-derived examples.
function GeneratedSDKExample({ endpoint, serviceName, requestSchema }: { endpoint: IntegrationObject; serviceName: string; requestSchema?: JsonSchemaNode }) {
  const code = typescriptSDKCallExample(serviceName, endpoint, requestSchema);
  return <OperationSection title="Use in an app"><OperationCodeExample label="Generated SDK example" language="TypeScript" code={code} copyLabel="Copy generated SDK example" /></OperationSection>;
}

// WebhookSchemaNotice explains the observation path without presenting an
// empty body as validated or promising a completion notification.
function WebhookSchemaNotice() {
  return (
    <section aria-label="Event schema status" className="rounded-lg border border-indigo-200 bg-indigo-50/60 p-4">
      <div className="flex items-start gap-2.5">
        <Info aria-hidden="true" className="mt-0.5 h-4 w-4 shrink-0 text-indigo-600" />
        <div className="space-y-2">
          <h3 className="text-sm font-semibold text-indigo-950">Event body schema pending</h3>
          <p className="text-xs leading-relaxed text-indigo-900">
            Fused infers event-body schemas from incoming webhook events in real time. This event does not yet have a defined body schema; observations need validation before the shape can be treated as confirmed.
          </p>
          <p className="text-xs leading-relaxed text-indigo-800">
            Schema inference records only field names and types, not payload values. You can still receive events while the body schema is pending.
          </p>
        </div>
      </div>
    </section>
  );
}

// RequestSchemaSection keeps unknown webhook bodies visible while preserving
// declared scalar, reference, and object schemas as expandable contracts.
function RequestSchemaSection({ endpoint, schema, serviceId, componentScope, allowRemoteRefs }: { endpoint: IntegrationObject; schema?: JsonSchemaNode; serviceId: string } & ComponentReferenceScope) {
  // A missing or empty inbound body is pending knowledge, not an absent UI section.
  if (endpoint.isWebhook && webhookSchemaPending(schema)) return <WebhookSchemaNotice />;
  // Outbound operations without a body should not inherit webhook inference copy.
  if (!schema) return null;
  // Inbound and outbound contracts share presentation while retaining their direction labels.
  return <OperationSchemaDisclosure track="toggle_request_schema" label={endpoint.isWebhook ? "Event Schema" : "Request Schema"}>
    <SchemaViewer schema={schema} serviceId={serviceId} componentScope={componentScope} allowRemoteRefs={allowRemoteRefs} isWebhookEvent={endpoint.isWebhook} />
  </OperationSchemaDisclosure>;
}

// ResponseSchemasSection renders every canonical response media representation.
function ResponseSchemasSection({ responses, serviceId, componentScope, allowRemoteRefs }: { responses: CanonicalResponseSchema[]; serviceId: string } & ComponentReferenceScope) {
  // Omit a response section only when the contract declares no representations.
  if (responses.length === 0) return null;
  return <OperationSection title="Responses">{responses.map(/** Retain each media representation as its own expandable schema. */ (response) => <ResponseSchemaViewer key={response.key} code={response.label} schema={response.schema} serviceId={serviceId} componentScope={componentScope} allowRemoteRefs={allowRemoteRefs} />)}</OperationSection>;
}
