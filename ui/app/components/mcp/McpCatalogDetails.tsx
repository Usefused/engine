import { useEffect, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from "react";
import { createPortal } from "react-dom";
import { Check, ChevronDown, Copy, X } from "lucide-react";
import { SchemaViewer } from "~/components/SchemaViewer";
import { mcpItemKey, type McpCatalogItem, type McpCatalogKind } from "~/lib/mcp-catalog";

export const catalogTypeBadges: Record<McpCatalogKind, { label: string; color: string }> = {
  tools: { label: "TOOL", color: "bg-blue-100 text-blue-700 border-blue-200" },
  prompts: { label: "PROMPT", color: "bg-purple-100 text-purple-700 border-purple-200" },
  resources: { label: "RESOURCE", color: "bg-green-100 text-green-700 border-green-200" },
  resource_templates: { label: "TEMPLATE", color: "bg-orange-100 text-orange-700 border-orange-200" },
};

/** Gives imported capabilities a readable overview while retaining their complete provider definition. */
export function McpCatalogDetails({ kind, item, onClose }: { kind: McpCatalogKind; item: McpCatalogItem; onClose: () => void }) {
  const [view, setView] = useState<"overview" | "definition">("overview");
  const panel = useRef<HTMLDivElement>(null);
  const close = useRef(onClose);
  close.current = onClose;
  // A modal inspection must return focus to its resource row and preserve the page's scroll position.
  useEffect(() => {
    const trigger = document.activeElement as HTMLElement;
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    panel.current?.focus();
    /** Routes dismissal and focus wrapping through the active inspector only. */
    function keyboard(event: KeyboardEvent) { inspectorKeyboard(event, panel.current, () => close.current()); }
    document.addEventListener("keydown", keyboard);
    return () => { document.removeEventListener("keydown", keyboard); document.body.style.overflow = previousOverflow; trigger?.focus(); };
  }, []);
  /** Arrow keys follow the two visible tabs, keeping one entry point in the focus order. */
  function changeTab(event: ReactKeyboardEvent<HTMLButtonElement>) {
    // Only tab-navigation keys override normal button keyboard behavior.
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    // Home and End target the edges; either arrow toggles this two-tab strip.
    const next = event.key === "Home" ? "overview" : event.key === "End" ? "definition" : view === "overview" ? "definition" : "overview";
    setView(next);
    panel.current?.querySelector<HTMLButtonElement>(`#mcp-${next}-tab`)?.focus();
  }
  const badge = catalogTypeBadges[kind];
  // Portaling avoids list layout and ancestor transforms changing the fixed inspector's viewport.
  return createPortal(<div className="contents">
    <div className="fixed inset-0 z-40 bg-slate-900/20 transition-opacity" onClick={onClose} />
    <div ref={panel} role="dialog" aria-modal="true" aria-label={`${badge.label} details: ${item.name}`} tabIndex={-1} data-fused-detail-sidebar className="fixed inset-y-0 right-0 z-50 flex w-full flex-col overflow-hidden border-l border-slate-200 bg-white text-slate-900 shadow-2xl outline-none sm:max-w-[640px]">
      <McpDetailsHeading kind={kind} item={item} onClose={onClose} />
      <div className="flex shrink-0 gap-6 border-b border-slate-200 px-5 sm:px-8" role="tablist" aria-label="MCP details view">
        {/* Both views inspect the same saved definition without contacting the upstream server. */}
        {(["overview", "definition"] as const).map((tab) => <button key={tab} type="button" role="tab" id={`mcp-${tab}-tab`} aria-selected={view === tab} tabIndex={view === tab ? 0 : -1} onKeyDown={changeTab} aria-controls="mcp-details-content" onClick={() => setView(tab)} className={`border-b-2 py-3 text-sm font-medium transition-colors ${view === tab ? "border-slate-950 text-slate-950" : "border-transparent text-slate-500 hover:text-slate-900"}`}>{tab === "overview" ? "Overview" : "Definition"}</button>)}
      </div>
      <div id="mcp-details-content" role="tabpanel" aria-labelledby={`mcp-${view}-tab`} className="min-h-0 flex-1 overflow-y-auto overscroll-contain p-5 sm:p-8">
        {/* Raw JSON is secondary to readable metadata, but never shortened or discarded. */}
        {view === "definition" ? <pre className="overflow-x-auto rounded-xl border border-slate-200 bg-slate-50 p-4 text-xs leading-6 text-slate-700">{JSON.stringify(item, null, 2)}</pre> : <div className="space-y-7">
          {/* Provider prose stays inert and keeps its original paragraph breaks. */}
          {item.description && <p className="whitespace-pre-wrap text-sm leading-6 text-slate-600">{item.description}</p>}
          <McpResourceFacts item={item} />
          <McpPromptArguments argumentsList={item.arguments} />
          <McpSchema title="Input" schema={item.inputSchema} />
          <McpSchema title="Output" schema={item.outputSchema} />
        </div>}
      </div>
    </div>
  </div>, document.body);
}

/** Keeps one capability identity visible and gives secondary actions proper touch targets. */
function McpDetailsHeading({ kind, item, onClose }: { kind: McpCatalogKind; item: McpCatalogItem; onClose: () => void }) {
  const [copied, setCopied] = useState(false);
  const badge = catalogTypeBadges[kind];
  const identity = mcpItemKey(kind, item);
  // A provider title is optional; the original identifier remains copyable in either case.
  const title = item.title || item.name;
  /** Confirms copying only after the browser accepts the public capability identifier. */
  async function copyIdentifier() {
    try {
      await navigator.clipboard.writeText(identity);
      setCopied(true);
    } catch {
      // Clipboard denial must not show a false success confirmation.
      setCopied(false);
    }
  }
  return <header className="shrink-0 px-5 pb-5 pt-5 sm:px-8 sm:pt-7">
    <div className="mb-5 flex items-center justify-between gap-3">
      <div className="flex items-center gap-2.5"><span className={`rounded-md border px-2 py-1 text-[10px] font-semibold tracking-wide ${badge.color}`}>{badge.label}</span><span className="text-xs text-slate-500">MCP catalog</span></div>
      <button type="button" onClick={onClose} aria-label="Close MCP details" className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg text-slate-500 transition-colors hover:bg-slate-100 hover:text-slate-900"><X className="h-4 w-4" /></button>
    </div>
    <div className="flex items-start justify-between gap-4">
      <h2 className="min-w-0 break-words text-xl font-semibold leading-7 tracking-tight sm:text-2xl sm:leading-8">{title}</h2>
      <button type="button" onClick={copyIdentifier} aria-label="Copy MCP identifier" title="Copy MCP identifier" className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg border border-slate-200 bg-white text-slate-500 transition-colors hover:bg-slate-50">
        {/* Success changes the icon without moving the title or copy target. */}
        {copied ? <Check className="h-4 w-4" /> : <Copy className="h-4 w-4" />}
      </button>
    </div>
    {/* Show the wire identifier separately only when it differs from the display title. */}
    {title !== identity && <p className="mt-2 break-all font-mono text-xs leading-5 text-slate-500">{identity}</p>}
    <span role="status" className="sr-only">{copied && "Identifier copied"}</span>
  </header>;
}

/** Keeps keyboard users inside the inspector until they explicitly dismiss it. */
function inspectorKeyboard(event: KeyboardEvent, panel: HTMLElement | null, close: () => void) {
  // Escape closes only the active inspection, without changing the selected catalog filter.
  if (event.key === "Escape") { event.preventDefault(); close(); return; }
  // Ordinary key presses remain available to schema disclosures and copy controls.
  if (event.key !== "Tab" || !panel) return;
  const controls = Array.from(panel.querySelectorAll<HTMLElement>('button:not(:disabled):not([tabindex="-1"]), a[href], summary, [tabindex="0"]')).filter((node) => node.getClientRects().length > 0);
  const first = controls[0];
  const last = controls[controls.length - 1];
  // The initial panel focus joins the same backward wrap as the first focusable control.
  if (event.shiftKey && [first, panel].includes(document.activeElement as HTMLElement)) { event.preventDefault(); last?.focus(); }
  else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first?.focus(); }
}

/** Shows only metadata supplied by resource and resource-template definitions. */
function McpResourceFacts({ item }: { item: McpCatalogItem }) {
  const facts = [["URI", item.uri], ["URI template", item.uriTemplate], ["Content type", item.mimeType]].filter(([, value]) => value);
  // Tools and prompts without resource metadata do not reserve an empty details block.
  if (facts.length === 0) return null;
  return <dl className="space-y-4 rounded-xl border border-slate-200 bg-slate-50/50 p-4 text-sm">{facts.map(([label, value]) => <div key={label}><dt className="text-xs font-medium text-slate-500">{label}</dt><dd className="mt-1 break-all font-mono text-slate-800">{value}</dd></div>)}</dl>;
}

/** Displays prompt inputs without rendering or executing the remote prompt. */
function McpPromptArguments({ argumentsList }: { argumentsList?: McpCatalogItem["arguments"] }) {
  // An absent or empty declaration must not invent prompt inputs.
  if (!argumentsList?.length) return null;
  return <section><h3 className="mb-3 text-sm font-semibold text-slate-900">Arguments</h3><div className="divide-y divide-slate-200 overflow-hidden rounded-lg border border-slate-200">{argumentsList.map((argument) => <div key={argument.name} className="p-4"><div className="flex justify-between gap-3"><code className="break-all text-sm text-slate-800">{argument.name}</code>{/* Requirement labels come only from the provider's declaration. */}<span className="text-xs text-slate-500">{argument.required ? "Required" : "Optional"}</span></div>{/* Arguments may omit descriptive copy. */}{argument.description && <p className="mt-2 whitespace-pre-wrap text-sm text-slate-500">{argument.description}</p>}</div>)}</div></section>;
}

/** Shows declared fields at a glance, retaining the recursive schema for constraints and nested types. */
function McpSchema({ title, schema }: { title: string; schema?: Record<string, unknown> }) {
  // Missing schemas are not evidence of an empty input or output contract.
  if (!schema) return null;
  const properties = schema.properties;
  // Only object property maps can become readable field rows; other schema shapes retain the full viewer.
  const fields = properties && typeof properties === "object" && !Array.isArray(properties) ? Object.entries(properties) : [];
  // Required is provider-controlled metadata, never inferred from a field's name or description.
  const required = Array.isArray(schema.required) ? schema.required : [];
  return <section aria-label={`${title} schema`}>
    <h3 className="mb-3 flex items-center gap-2 text-sm font-semibold text-slate-900">{title}{/* Counts describe declared fields, not the number of possible values. */}{fields.length > 0 && <span className="rounded-md bg-slate-100 px-1.5 py-0.5 text-xs font-medium text-slate-500">{fields.length}</span>}</h3>
    {/* Complex root schemas use the existing recursive viewer instead of a misleading flat summary. */}
    {fields.length > 0 ? <div className="mb-3 divide-y divide-slate-100 overflow-hidden rounded-xl border border-slate-200">{fields.map(([name, field]) => <McpSchemaField key={name} name={name} field={field} required={required.includes(name)} />)}</div> : <div className="mb-3 overflow-x-auto rounded-xl bg-[#161c27] p-4"><SchemaViewer schema={schema} serviceId="" componentScope="mcp-catalog" allowRemoteRefs={false} /></div>}
    {/* Field summaries omit constraints, so complete schemas remain one explicit click away. */}
    {fields.length > 0 && <details className="group overflow-hidden rounded-lg border border-slate-200"><summary className="flex cursor-pointer list-none items-center justify-between px-4 py-3 text-xs font-medium text-slate-500 hover:bg-slate-50 [&::-webkit-details-marker]:hidden">View {title.toLowerCase()} schema<ChevronDown className="h-3.5 w-3.5 transition-transform group-open:rotate-180" /></summary><div className="overflow-x-auto bg-[#161c27] p-4"><SchemaViewer schema={schema} serviceId="" componentScope="mcp-catalog" allowRemoteRefs={false} /></div></details>}
  </section>;
}

/** Presents each declared field as readable metadata without assuming it is a simple JSON Schema. */
function McpSchemaField({ name, field, required }: { name: string; field: unknown; required: boolean }) {
  // Boolean and malformed definitions stay available in the complete schema without unsafe property access.
  const node = field && typeof field === "object" ? field as Record<string, unknown> : {};
  // Union types remain intact; undeclared types are left unlabelled rather than guessed.
  const type = Array.isArray(node.type) ? node.type.join(" | ") : typeof node.type === "string" ? node.type : "";
  return <div className="p-4">
    <div className="flex flex-wrap items-center gap-x-2.5 gap-y-2"><code className="break-all text-sm font-medium text-slate-900">{name}</code>{/* Types appear only when the schema explicitly supplies one. */}{type && <span className="font-mono text-xs text-slate-500">{type}</span>}<span className="ml-auto text-xs text-slate-500">{required ? "Required" : "Optional"}</span></div>
    {/* Descriptions render as plain text so remote metadata cannot introduce markup or scripts. */}
    {typeof node.description === "string" && <p className="mt-2 whitespace-pre-wrap text-sm leading-6 text-slate-600">{node.description}</p>}
  </div>;
}
