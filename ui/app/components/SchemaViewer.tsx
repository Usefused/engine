import React, { useState } from "react";
import { ChevronDown, ChevronRight, Loader2, Link2, AlertCircle, Info } from "lucide-react";
import { api } from "~/lib/api";
import { planSchemaReference, resolvePlannedSchemaReference, schemaReferenceLabel } from "~/lib/schema-reference";
import type { ResolvedSchemaReference, SchemaReferencePlan } from "~/lib/schema-reference";

// Loosely-typed JSON Schema node used for the recursive tree renderer below.
// Deliberately permissive (all fields optional) since real-world schemas from
// observed webhook payloads / OpenAPI imports vary widely in which fields
// they populate.
export interface JsonSchemaNode {
  $ref?: string;
  type?: string;
  properties?: Record<string, JsonSchemaNode>;
  items?: JsonSchemaNode;
  required?: string[];
  format?: string;
  nullable?: boolean;
  description?: string;
  enum?: unknown[];
  examples?: unknown[];
  example?: unknown;
  default?: unknown;
  oneOf?: JsonSchemaNode[];
  anyOf?: JsonSchemaNode[];
  allOf?: JsonSchemaNode[];
}

interface ComponentResolutionContext {
  componentScope: string;
  allowRemoteRefs: boolean;
  document: unknown;
  cache: Map<string, unknown>;
}

const ComponentResolutionContext = React.createContext<ComponentResolutionContext>({
  componentScope: "",
  allowRemoteRefs: true,
  document: undefined,
  cache: new Map<string, unknown>(),
});

// ─── Type system ──────────────────────────────────────────────────────────────

type SchemaType =
  | "object" | "array" | "string" | "number" | "integer"
  | "boolean" | "null" | "ref" | "oneOf" | "anyOf" | "allOf" | "unknown";

// inferType classifies permissive JSON Schema nodes for the recursive renderer.
function inferType(node: unknown): SchemaType {
  if (!node) return "unknown";
  if (typeof node !== "object") return "unknown";
  const obj = node as JsonSchemaNode;
  if (obj.$ref)    return "ref";
  if (obj.oneOf)   return "oneOf";
  if (obj.anyOf)   return "anyOf";
  if (obj.allOf)   return "allOf";
  if (obj.properties) return "object";
  if (obj.items) return "array";
  if (["object", "array", "string", "number", "integer", "boolean", "null"].includes(String(obj.type))) return obj.type as SchemaType;
  return "unknown";
}

// ─── Type badge (text-only, tight pill) ──────────────────────────────────────

const BADGE: Record<SchemaType, string> = {
  string:  "text-slate-300 bg-slate-800/40 border border-slate-700/40",
  number:  "text-slate-300 bg-slate-800/40 border border-slate-700/40",
  integer: "text-slate-300 bg-slate-800/40 border border-slate-700/40",
  boolean: "text-slate-300 bg-slate-800/40 border border-slate-700/40",
  array:   "text-slate-300 bg-slate-800/40 border border-slate-700/40",
  object:  "text-slate-300 bg-slate-800/40 border border-slate-700/40",
  null:    "text-slate-400 bg-slate-800/40 border border-slate-700/40",
  ref:     "text-blue-400  bg-blue-950/30  border border-blue-900/30",
  oneOf:   "text-slate-300 bg-slate-800/40 border border-slate-700/40",
  anyOf:   "text-slate-300 bg-slate-800/40 border border-slate-700/40",
  allOf:   "text-slate-300 bg-slate-800/40 border border-slate-700/40",
  unknown: "text-slate-400 bg-slate-800/40 border border-slate-700/30",
};

function TypeBadge({ type, format, nullable }: { type: SchemaType; format?: string; nullable?: boolean }) {
  const label = format ? `${type}<${format}>` : type;
  return (
    <span className="inline-flex items-center gap-1 shrink-0">
      <span className={`inline-block px-1.5 py-0.5 rounded text-[10px] font-medium font-mono leading-none ${BADGE[type]}`}>
        {label}
      </span>
      {nullable && (
        <span className="inline-block px-1.5 py-0.5 rounded text-[10px] font-medium font-mono leading-none text-slate-400 bg-slate-800/40 border border-slate-700/40">
          null
        </span>
      )}
    </span>
  );
}

// ─── Schema info tooltip (webhook event observation notice) ───────────────────

function SchemaInfoTooltip() {
  return (
    <div className="relative inline-flex group/tip">
      <Info className="w-3 h-3 text-rose-400/60 hover:text-rose-400 cursor-help transition-colors shrink-0" />
      <div className="absolute bottom-full left-1/2 -translate-x-1/2 mb-2 w-60 px-2.5 py-2 rounded-md bg-slate-800 border border-slate-700 text-[10px] text-slate-300 leading-snug pointer-events-none opacity-0 group-hover/tip:opacity-100 transition-opacity z-50 shadow-lg">
        Fused observes incoming webhook events to infer their schema. Only field names and types are recorded — payload values are never stored.
        <div className="absolute top-full left-1/2 -translate-x-1/2 border-4 border-transparent border-t-slate-700" />
      </div>
    </div>
  );
}

// ─── Tree guide line ──────────────────────────────────────────────────────────

function TreeGuide({ children }: { children: React.ReactNode }) {
  return (
    <div className="ml-2 pl-3 border-l border-slate-600/50 mt-px">
      {children}
    </div>
  );
}

// ─── Main export ──────────────────────────────────────────────────────────────

interface SchemaViewerProps {
  schema: JsonSchemaNode | boolean | null | undefined;
  serviceId: string;
  componentScope?: string;
  allowRemoteRefs?: boolean;
  isWebhookEvent?: boolean;
}

// SchemaViewer renders a schema tree and scopes any remote component expansion.
export function SchemaViewer({ schema, serviceId, componentScope = "latest", allowRemoteRefs = true, isWebhookEvent }: SchemaViewerProps) {
  // Leaving a service/version view discards its private definitions instead of retaining a global cache.
  const cache = React.useMemo(() => new Map<string, unknown>(), [serviceId, componentScope]);
  // False is an authoritative schema that rejects every value, not missing data.
  if (schema == null) {
    return <p className="text-[11px] text-slate-500 italic py-1.5">No schema defined.</p>;
  }
  return (
    <ComponentResolutionContext.Provider key={`${serviceId}:${componentScope}`} value={{ componentScope, allowRemoteRefs, document: schema, cache }}>
      <div className="text-[11px] leading-normal select-text w-full">
        <SchemaNode node={schema} serviceId={serviceId} depth={0} isWebhookEvent={isWebhookEvent} />
      </div>
    </ComponentResolutionContext.Provider>
  );
}

// ─── Node dispatcher ──────────────────────────────────────────────────────────

interface NodeProps {
  node: unknown;
  serviceId: string;
  depth: number;
  name?: string;
  required?: boolean;
  isWebhookEvent?: boolean;
}

function SchemaNode({ node, serviceId, depth, name, required, isWebhookEvent }: NodeProps) {
  if (!node || typeof node !== "object") {
    return <Row name={name} required={required}><PrimitiveVal value={node} /></Row>;
  }
  const type = inferType(node);
  if (type === "ref")    return <RefNode    refStr={(node as JsonSchemaNode).$ref as string} serviceId={serviceId} depth={depth} name={name} required={required} />;
  if (type === "object") return <ObjectNode node={node as JsonSchemaNode} serviceId={serviceId} depth={depth} name={name} required={required} isWebhookEvent={depth === 0 && isWebhookEvent} />;
  if (type === "array")  return <ArrayNode  node={node as JsonSchemaNode} serviceId={serviceId} depth={depth} name={name} required={required} />;
  if (type === "oneOf" || type === "anyOf" || type === "allOf")
    return <CompositeNode node={node as JsonSchemaNode} serviceId={serviceId} depth={depth} ctype={type} name={name} required={required} />;
  return <ScalarNode node={node as JsonSchemaNode} type={type} name={name} required={required} />;
}

// ─── Shared row wrapper ───────────────────────────────────────────────────────

function Row({
  name, required, children, onToggle, expanded,
}: {
  name?: string; required?: boolean; children: React.ReactNode;
  onToggle?: () => void; expanded?: boolean;
}) {
  const inner = (
    <div className="flex items-start gap-1.5 min-w-0 py-[3px] pr-1">
      {/* Chevron for expandable rows */}
      {onToggle !== undefined ? (
        <span className="shrink-0 mt-[1px] text-slate-400 group-hover:text-slate-200 transition-colors">
          {expanded
            ? <ChevronDown className="w-[11px] h-[11px]" />
            : <ChevronRight className="w-[11px] h-[11px]" />}
        </span>
      ) : (
        <span className="shrink-0 w-[11px]" /> /* phantom spacer for alignment */
      )}

      {/* Name + required marker */}
      {name && (
        <span className="shrink-0 font-semibold text-white leading-snug whitespace-nowrap">
          {name}
          {required === true  && <span className="ml-0.5 text-red-400 font-bold text-[9px]">*</span>}
          {required === false && <span className="ml-0.5 text-slate-500 font-normal text-[9px]">?</span>}
        </span>
      )}

      {/* Content (badge, description, etc.) */}
      <div className="min-w-0 flex-1">{children}</div>
    </div>
  );

  if (onToggle) {
    return (
      <button
        data-track="toggle_schema_node"
        onClick={onToggle}
        className="group w-full text-left rounded-md hover:bg-white/[0.04] transition-colors cursor-pointer focus:outline-none"
      >
        {inner}
      </button>
    );
  }
  return <div className="rounded-md hover:bg-white/[0.03] transition-colors">{inner}</div>;
}

// ─── Scalar ───────────────────────────────────────────────────────────────────

function ScalarNode({ node, type, name, required }: { node: JsonSchemaNode; type: SchemaType; name?: string; required?: boolean }) {
  const format     = node.format;
  const desc       = node.description;
  const enums: unknown[] = node.enum ?? [];
  const def        = node.default;
  const ex         = node.example ?? node.examples?.[0];

  return (
    <Row name={name} required={required}>
      <div className="flex flex-wrap items-center gap-x-1.5 gap-y-0.5">
        <TypeBadge type={type} format={format} nullable={node.nullable} />
        {enums.length > 0 && (
          <span className="text-slate-400 text-[10px] flex flex-wrap gap-0.5">
            {enums.slice(0, 4).map(v => (
              <code key={String(v)} className="px-1 rounded bg-slate-700/60 text-slate-200 font-mono">{JSON.stringify(v)}</code>
            ))}
            {enums.length > 4 && <span className="text-slate-500">+{enums.length - 4}</span>}
          </span>
        )}
        {def !== undefined && (
          <span className="text-slate-400 text-[10px]">
            = <code className="font-mono text-slate-300 bg-slate-700/50 px-0.5 rounded">{JSON.stringify(def)}</code>
          </span>
        )}
        {ex !== undefined && (
          <span className="text-slate-400 text-[10px]">
            · e.g. <code className="font-mono text-slate-300 bg-slate-700/50 px-0.5 rounded">{JSON.stringify(ex)}</code>
          </span>
        )}
      </div>
      {desc && <p className="text-slate-400 text-[10px] leading-snug mt-0.5 font-sans">{desc}</p>}
    </Row>
  );
}

// ─── Object ───────────────────────────────────────────────────────────────────

function ObjectNode({ node, serviceId, depth, name, required, isWebhookEvent }: { node: JsonSchemaNode; serviceId: string; depth: number; name?: string; required?: boolean; isWebhookEvent?: boolean }) {
  const [open, setOpen] = useState(depth < 2);
  const props      = node.properties ?? {};
  const req: string[] = node.required ?? [];
  const keys       = Object.keys(props);
  const desc       = node.description;

  return (
    <div>
      <Row name={name} required={required} onToggle={() => setOpen(o => !o)} expanded={open}>
        <div className="flex flex-wrap items-center gap-x-1.5 gap-y-0.5">
          <TypeBadge type="object" nullable={node.nullable} />
          <span className="text-slate-400 text-[10px]">{keys.length} {keys.length === 1 ? "prop" : "props"}</span>
          {desc && !open && <span className="text-slate-400 text-[10px] truncate max-w-[140px]">{desc}</span>}
          {isWebhookEvent && <SchemaInfoTooltip />}
        </div>
      </Row>
      {open && (
        <>
          {desc && <p className="text-slate-400 text-[10px] leading-snug ml-6 mt-0.5 mb-1">{desc}</p>}
          <TreeGuide>
            {keys.length > 0
              ? keys.map(k => (
                  <SchemaNode
                    key={k} node={props[k]} serviceId={serviceId}
                    depth={depth + 1} name={k} required={req.includes(k)}
                  />
                ))
              : (
                <div className="py-1">
                  <p className="text-slate-400 text-[10px] italic py-0.5">empty object - Schema not fully inferred yet</p>
                </div>
              )
            }
          </TreeGuide>
        </>
      )}
    </div>
  );
}

// ─── Array ────────────────────────────────────────────────────────────────────

function ArrayNode({ node, serviceId, depth, name, required }: { node: JsonSchemaNode; serviceId: string; depth: number; name?: string; required?: boolean }) {
  const [open, setOpen] = useState(depth < 2);
  const items = node.items;
  const desc  = node.description;

  return (
    <div>
      <Row name={name} required={required} onToggle={() => setOpen(o => !o)} expanded={open}>
        <div className="flex flex-wrap items-center gap-x-1.5 gap-y-0.5">
          <TypeBadge type="array" nullable={node.nullable} />
          {desc && !open && <span className="text-slate-400 text-[10px] truncate max-w-[140px]">{desc}</span>}
        </div>
      </Row>
      {open && (
        <>
          {desc && <p className="text-slate-400 text-[10px] leading-snug ml-6 mt-0.5 mb-1">{desc}</p>}
          {items && (
            <TreeGuide>
              <span className="block text-[9px] text-slate-500 uppercase tracking-widest mb-0.5 font-semibold">items</span>
              <SchemaNode node={items} serviceId={serviceId} depth={depth + 1} />
            </TreeGuide>
          )}
        </>
      )}
    </div>
  );
}

// ─── Composite (oneOf / anyOf / allOf) ───────────────────────────────────────

function CompositeNode({ node, serviceId, depth, ctype, name, required }: { node: JsonSchemaNode; serviceId: string; depth: number; ctype: "oneOf" | "anyOf" | "allOf"; name?: string; required?: boolean }) {
  const [open, setOpen] = useState(depth === 0);
  const variants: JsonSchemaNode[] = node[ctype] ?? [];
  const desc = node.description;
  const LABEL: Record<string, string> = { oneOf: "one of", anyOf: "any of", allOf: "all of" };

  return (
    <div>
      <Row name={name} required={required} onToggle={() => setOpen(o => !o)} expanded={open}>
        <div className="flex flex-wrap items-center gap-x-1.5 gap-y-0.5">
          <TypeBadge type={ctype} />
          <span className="text-slate-400 text-[10px]">{LABEL[ctype]} · {variants.length}</span>
        </div>
      </Row>
      {open && (
        <TreeGuide>
          {desc && <p className="text-slate-400 text-[10px] leading-snug mb-1">{desc}</p>}
          {variants.map((v, i) => (
            <div key={i}>
              {variants.length > 1 && (
                <span className="block text-[9px] text-slate-500 uppercase tracking-widest mt-1 mb-0.5 font-semibold">
                  variant {i + 1}
                </span>
              )}
              <SchemaNode node={v} serviceId={serviceId} depth={depth + 1} />
            </div>
          ))}
        </TreeGuide>
      )}
    </div>
  );
}

// ─── $ref ─────────────────────────────────────────────────────────────────────

// RefNode expands local pointers before consulting the exact selected version's
// saved components and preserves the fetched definition's own document scope.
function RefNode({ refStr, serviceId, name, required }: { refStr: string; serviceId: string; depth: number; name?: string; required?: boolean }) {
  const context = React.useContext(ComponentResolutionContext);
  const plan = planSchemaReference(context.document, refStr);
  const unavailable = unavailableSchemaReference(plan, context);
  const { open, loading, data, err, toggle } = useSchemaReference(refStr, serviceId, context, plan);

  return (
    <div>
      <Row name={name} required={required} onToggle={unavailable ? undefined : toggle} expanded={open}>
        <div className="flex items-center gap-1.5 flex-wrap">
          <span className={`inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-[10px] font-medium font-mono leading-none ${BADGE.ref}`}>
            <Link2 className="w-2.5 h-2.5 shrink-0" />
            {schemaReferenceLabel(refStr)}
          </span>
          {loading && <Loader2 className="w-2.5 h-2.5 text-indigo-400 animate-spin shrink-0" />}
        </div>
      </Row>
      {unavailable && (
        <p className="ml-6 text-[10px] italic text-slate-500">
          {unavailable}
        </p>
      )}
      {open && (
        <TreeGuide>
          {err && (
            <div className="flex items-center gap-1 text-red-400 text-[10px] py-1">
              <AlertCircle className="w-3 h-3 shrink-0" /> {err}
            </div>
          )}
          {loading && !data && !err && (
            <p className="text-slate-400 text-[10px] italic py-1">Loading…</p>
          )}
          {/* depth=0 so the fetched schema always auto-opens regardless of nesting level */}
          {data && (
            <ComponentResolutionContext.Provider value={{ ...context, document: data.document }}>
              <SchemaNode node={data.schema} serviceId={serviceId} depth={0} />
            </ComponentResolutionContext.Provider>
          )}
        </TreeGuide>
      )}
    </div>
  );
}

// unavailableSchemaReference keeps local definitions usable while respecting
// a caller's explicit remote-expansion policy and unresolved version state.
function unavailableSchemaReference(plan: SchemaReferencePlan, context: ComponentResolutionContext): string | null {
  // Local JSON Pointers never need Registry permissions or a network request.
  if (plan.kind === "local") return null;
  // Unsupported URLs and broken local pointers remain visible, not guessed names.
  if (plan.kind === "unavailable") return plan.reason;
  // An unresolved selection must not float silently to the current service version.
  if (!context.componentScope || context.componentScope === "unresolved") return "Select a service version to expand this reference.";
  // Explicitly disabled remote reads remain disabled even when a component is cached.
  if (!context.allowRemoteRefs) return "Reference expansion is disabled for this view.";
  return null;
}

// useSchemaReference fences asynchronous results so switching versions or local
// document roots cannot display a response from the previous schema scope.
function useSchemaReference(refStr: string, serviceId: string, context: ComponentResolutionContext, plan: SchemaReferencePlan) {
  const [open, setOpen] = useState(false);
  const [loading, setLoading] = useState(false);
  const [data, setData] = useState<ResolvedSchemaReference | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const generation = React.useRef(0);
  React.useEffect(() => {
    generation.current++;
    setOpen(false);
    setLoading(false);
    setData(null);
    setErr(null);
    // Unmounted or replaced roots must not accept a late component response.
    return () => { generation.current++; };
  }, [refStr, serviceId, context.componentScope, context.document]);

  // toggle resolves one visible reference on demand, never recursively prefetching its graph.
  const toggle = async () => {
    // Collapsing a row must not issue another component request.
    if (open) { setOpen(false); return; }
    const current = ++generation.current;
    setOpen(true);
    setLoading(true);
    setErr(null);
    try {
      const resolved = await resolvePlannedSchemaReference(plan, (componentName) => fetchSchemaReferenceComponent(serviceId, context.componentScope, componentName, context.cache));
      // Only the still-selected document may consume the completed lookup.
      if (current === generation.current) setData(resolved);
    } catch (error) {
      // Old failures must not replace the current version's schema or error state.
      if (current === generation.current) setErr(error instanceof Error ? error.message : "Schema reference could not be loaded.");
    } finally {
      // A previous request must not clear a newer request's loading indicator.
      if (current === generation.current) setLoading(false);
    }
  };
  return { open, loading, data, err, toggle };
}

// fetchSchemaReferenceComponent requests one exact decoded component identity
// and caches only immutable version selections, not the moving latest alias.
async function fetchSchemaReferenceComponent(serviceId: string, componentScope: string, componentName: string, cache: Map<string, unknown>): Promise<unknown> {
  const version = componentScope === "latest" ? undefined : componentScope;
  const key = JSON.stringify([serviceId, version, componentName]);
  // Map membership distinguishes a saved false schema from a missing definition.
  if (version && cache.has(key)) return cache.get(key);
  const res = await api.graphql<{ getServiceComponent: { id: string; name: string; schema: unknown } | null }>(`
    query GetServiceComponent($serviceId: String!, $name: String!, $version: String) {
      getServiceComponent(serviceId: $serviceId, name: $name, version: $version) { id name schema }
    }
  `, { serviceId, name: componentName, version });
  const schema = res.getServiceComponent?.schema;
  // Null means absent; false is a valid restrictive JSON Schema definition.
  if (schema == null) throw new Error("Schema definition was not found in the selected version.");
  // Floating latest responses must not survive a subsequent version publication.
  if (version) cache.set(key, schema);
  return schema;
}

// ─── Raw primitives ───────────────────────────────────────────────────────────

function PrimitiveVal({ value }: { value: unknown }) {
  if (value == null)              return <span className="text-slate-400 italic text-[10px]">null</span>;
  if (typeof value === "string")  return <span className="text-emerald-400 text-[10px] font-mono">"{value}"</span>;
  if (typeof value === "number")  return <span className="text-amber-400   text-[10px] font-mono">{value}</span>;
  if (typeof value === "boolean") return <span className="text-purple-400  text-[10px] font-mono">{String(value)}</span>;
  return <span className="text-slate-300 text-[10px] font-mono">{JSON.stringify(value)}</span>;
}
