import React, { useState } from "react";
import { ChevronDown, ChevronRight, Loader2, Link2, AlertCircle, Info } from "lucide-react";
import { api } from "~/lib/api";

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

// Global cache for fetched $ref components
const componentCache: Record<string, JsonSchemaNode> = {};

// ─── Type system ──────────────────────────────────────────────────────────────

type SchemaType =
  | "object" | "array" | "string" | "number" | "integer"
  | "boolean" | "null" | "ref" | "oneOf" | "anyOf" | "allOf" | "unknown";

function inferType(node: unknown): SchemaType {
  if (!node || typeof node !== "object") return "unknown";
  const obj = node as JsonSchemaNode;
  if (obj.$ref)    return "ref";
  if (obj.oneOf)   return "oneOf";
  if (obj.anyOf)   return "anyOf";
  if (obj.allOf)   return "allOf";
  if (obj.type === "object" || obj.properties) return "object";
  if (obj.type === "array"  || obj.items)      return "array";
  if (obj.type === "string")  return "string";
  if (obj.type === "number")  return "number";
  if (obj.type === "integer") return "integer";
  if (obj.type === "boolean") return "boolean";
  if (obj.type === "null")    return "null";
  return "unknown";
}

const getRefName = (ref: string) => ref.split("/").at(-1) ?? ref;

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
  schema: JsonSchemaNode | null | undefined;
  serviceId: string;
  isWebhookEvent?: boolean;
}

export function SchemaViewer({ schema, serviceId, isWebhookEvent }: SchemaViewerProps) {
  if (!schema) {
    return <p className="text-[11px] text-slate-500 italic py-1.5">No schema defined.</p>;
  }
  return (
    <div className="text-[11px] leading-normal select-text w-full">
      <SchemaNode node={schema} serviceId={serviceId} depth={0} isWebhookEvent={isWebhookEvent} />
    </div>
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

function RefNode({ refStr, serviceId, name, required }: { refStr: string; serviceId: string; depth: number; name?: string; required?: boolean }) {
  const [open, setOpen]       = useState(false);
  const [loading, setLoading] = useState(false);
  const [data, setData]       = useState<JsonSchemaNode | null>(null);
  const [err, setErr]         = useState<string | null>(null);
  const refName = getRefName(refStr);

  const toggle = async () => {
    // If already open, collapse
    if (open) { setOpen(false); return; }
    // Open immediately so loading state is visible
    setOpen(true);
    const key = `${serviceId}:${refName}`;
    // Cache hit — data already set, nothing more to do
    if (componentCache[key]) { setData(componentCache[key]); return; }
    // Fetch
    setLoading(true);
    setErr(null);
    try {
      const res = await api.graphql<{ getServiceComponent: { id: string; name: string; schema: JsonSchemaNode } | null }>(`
        query GetServiceComponent($serviceId: String!, $name: String!) {
          getServiceComponent(serviceId: $serviceId, name: $name) { id name schema }
        }
      `, { serviceId, name: refName });
      const schema = res.getServiceComponent?.schema;
      if (schema) {
        componentCache[key] = schema;
        // Setting data while open=true → React will render it immediately, no extra click needed
        setData(schema);
      } else {
        setErr("Not found");
      }
    } catch (e) {
      setErr(e instanceof Error ? e.message : "Failed");
    } finally {
      setLoading(false);
    }
  };

  return (
    <div>
      <Row name={name} required={required} onToggle={toggle} expanded={open}>
        <div className="flex items-center gap-1.5 flex-wrap">
          <span className={`inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-[10px] font-medium font-mono leading-none ${BADGE.ref}`}>
            <Link2 className="w-2.5 h-2.5 shrink-0" />
            {refName}
          </span>
          {loading && <Loader2 className="w-2.5 h-2.5 text-indigo-400 animate-spin shrink-0" />}
        </div>
      </Row>
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
          {data && <SchemaNode node={data} serviceId={serviceId} depth={0} />}
        </TreeGuide>
      )}
    </div>
  );
}

// ─── Raw primitives ───────────────────────────────────────────────────────────

function PrimitiveVal({ value }: { value: unknown }) {
  if (value == null)              return <span className="text-slate-400 italic text-[10px]">null</span>;
  if (typeof value === "string")  return <span className="text-emerald-400 text-[10px] font-mono">"{value}"</span>;
  if (typeof value === "number")  return <span className="text-amber-400   text-[10px] font-mono">{value}</span>;
  if (typeof value === "boolean") return <span className="text-purple-400  text-[10px] font-mono">{String(value)}</span>;
  return <span className="text-slate-300 text-[10px] font-mono">{JSON.stringify(value)}</span>;
}
