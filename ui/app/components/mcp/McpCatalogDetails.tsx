import { useEffect, useRef } from "react";
import { createPortal } from "react-dom";
import { OperationDetailsHeader, OperationDisclosure } from "~/components/OperationDetailsParts";
import { SchemaViewer } from "~/components/SchemaViewer";
import { mcpItemKey, type McpCatalogItem, type McpCatalogKind } from "~/lib/mcp-catalog";

export const catalogTypeBadges: Record<McpCatalogKind, { label: string; color: string }> = {
  tools: { label: "TOOL", color: "bg-blue-100 text-blue-700 border-blue-200" },
  prompts: { label: "PROMPT", color: "bg-purple-100 text-purple-700 border-purple-200" },
  resources: { label: "RESOURCE", color: "bg-green-100 text-green-700 border-green-200" },
  resource_templates: { label: "TEMPLATE", color: "bg-orange-100 text-orange-700 border-orange-200" },
};

/** Presents imported MCP definitions in the same inspector shell as individual operations. */
export function McpCatalogDetails({ kind, item, onClose }: { kind: McpCatalogKind; item: McpCatalogItem; onClose: () => void }) {
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
  const badge = catalogTypeBadges[kind];
  // Portaling avoids list layout and ancestor transforms changing the fixed inspector's viewport.
  return createPortal(<div className="contents">
    <div className="fixed inset-0 z-40 bg-slate-900/20 transition-opacity" onClick={onClose} />
    <div ref={panel} role="dialog" aria-modal="true" aria-label={`${badge.label} details: ${item.name}`} tabIndex={-1} data-fused-detail-sidebar className="fixed inset-y-0 right-0 z-50 flex w-full flex-col overflow-y-auto overflow-x-hidden border-l border-slate-200 bg-white shadow-2xl md:w-[600px]">
      <OperationDetailsHeader badge={badge.label} badgeClass={badge.color} identity={mcpItemKey(kind, item)} copyLabel="Copy MCP identifier" closeLabel="Close MCP details" onClose={onClose} />
      <div className="flex flex-1 flex-col space-y-8 p-4 sm:p-6">
        <div><h2 className="text-lg font-semibold text-slate-900">{item.title || item.name}</h2>{/* Provider text stays inert and complete in the detail view. */}{item.description && <p className="mt-2 whitespace-pre-wrap text-sm text-slate-600">{item.description}</p>}</div>
        <McpResourceFacts item={item} />
        <McpPromptArguments argumentsList={item.arguments} />
        <McpSchema title="Input schema" schema={item.inputSchema} />
        <McpSchema title="Output schema" schema={item.outputSchema} />
        <OperationDisclosure label="Full definition"><pre className="overflow-auto bg-slate-950 p-4 text-xs text-slate-200">{JSON.stringify(item, null, 2)}</pre></OperationDisclosure>
      </div>
    </div>
  </div>, document.body);
}

/** Keeps keyboard users inside the inspector until they explicitly dismiss it. */
function inspectorKeyboard(event: KeyboardEvent, panel: HTMLElement | null, close: () => void) {
  // Escape closes only the active inspection, without changing the selected catalog filter.
  if (event.key === "Escape") { event.preventDefault(); close(); return; }
  // Ordinary key presses remain available to schema disclosures and copy controls.
  if (event.key !== "Tab" || !panel) return;
  const controls = Array.from(panel.querySelectorAll<HTMLElement>('button:not(:disabled), a[href], [tabindex="0"]')).filter((node) => node.getClientRects().length > 0);
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
  return <dl className="space-y-3 text-sm">{facts.map(([label, value]) => <div key={label}><dt className="text-xs font-medium text-slate-500">{label}</dt><dd className="mt-1 break-all font-mono text-slate-800">{value}</dd></div>)}</dl>;
}

/** Displays prompt inputs without rendering or executing the remote prompt. */
function McpPromptArguments({ argumentsList }: { argumentsList?: McpCatalogItem["arguments"] }) {
  // An absent or empty declaration must not invent prompt inputs.
  if (!argumentsList?.length) return null;
  return <section><h3 className="mb-3 border-b pb-2 text-sm font-semibold">Arguments</h3><div className="divide-y divide-slate-200 overflow-hidden rounded-lg border border-slate-200">{argumentsList.map((argument) => <div key={argument.name} className="p-4"><div className="flex justify-between gap-3"><code className="break-all text-sm text-slate-800">{argument.name}</code>{/* Requirement labels come only from the provider's declaration. */}<span className="text-xs text-slate-500">{argument.required ? "Required" : "Optional"}</span></div>{/* Arguments may omit descriptive copy. */}{argument.description && <p className="mt-2 whitespace-pre-wrap text-sm text-slate-500">{argument.description}</p>}</div>)}</div></section>;
}

/** Reuses endpoint schema presentation without resolving provider-controlled references over the network. */
function McpSchema({ title, schema }: { title: string; schema?: Record<string, unknown> }) {
  // A schema is displayed only when the MCP server declared it.
  if (!schema) return null;
  return <section><h3 className="mb-3 border-b pb-2 text-sm font-semibold">{title}</h3><OperationDisclosure label="Schema"><div className="bg-[#161c27] p-4"><SchemaViewer schema={schema} serviceId="" componentScope="mcp-catalog" allowRemoteRefs={false} /></div></OperationDisclosure></section>;
}
