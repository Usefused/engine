import { useState } from "react";
import { createRoot } from "react-dom/client";
import McpCatalogTab from "../../app/components/mcp/McpCatalogTab";
import { ImportedMcpPicker, type ImportedMcpSelection } from "../../app/components/apps/ImportedMcpPicker";
import { api } from "../../app/lib/api";
import type { McpCatalogSnapshot } from "../../app/lib/mcp-catalog";

const snapshot: McpCatalogSnapshot = {
 id: "preview-revision", url: "https://example.com/mcp", created_at: "2026-10-10T12:00:00Z",
 catalog: { protocol_version: "2025-03-26", server: {name:"Fixture",version:"1"},
 supported: {tools:true,prompts:true,resources:false,resource_templates:false},
 tools:[{name:"search_documents",description:"Find service documentation.",inputSchema:{type:"object"}}],
 prompts:[{name:"summarize",description:"Summarize a document."}],resources:[],resource_templates:[] }
};
let reads = 0;
/** Synthetic catalog reads make disabled-state network activity visible without contacting a provider. */
api.mcpCatalog.get = async () => { reads += 1; return {catalog:snapshot}; };

/** Models an auth-protected provider without transmitting credentials or contacting the network. */
api.mcpCatalog.discover = async (_service, _version, input) => {
 // Credential-free input must remain true for both URL edits and saved-catalog refreshes.
 if (Object.keys(input).some((key) => key !== "url")) throw new Error("Fixture received unexpected credentials");
 throw new Error("This server requires authentication to discover its capabilities. Authenticated discovery isn't supported yet.");
};

/** Exercises the production import and selection components with a reversible local flag. */
function Preview() {
 const [enabled, setEnabled] = useState(false);
 const [selection, setSelection] = useState<ImportedMcpSelection>();
 (window as Window & {__FUSED_ENV?:{IMPORTED_MCP_ENABLED:boolean}}).__FUSED_ENV = {IMPORTED_MCP_ENABLED:enabled};
 /** A local toggle simulates reloading the shell after an operator changes deployment settings. */
 function toggle() { setEnabled(!enabled); }
 /** Store only synthetic capability selection so the fixture has no app mutations. */
 function select(_service: string, value?: ImportedMcpSelection) { setSelection(value); }
 return <main className="mx-auto max-w-5xl space-y-6 p-6 text-slate-900">
  <h1 className="text-2xl font-semibold">Imported MCP feature flag</h1>
  <p>Local fixture using production components. No provider or workspace writes.</p>
  <button className="rounded-lg bg-black px-4 py-2 text-white" onClick={toggle}>{enabled ? "Disable imported MCP" : "Enable imported MCP"}</button>
  <p>Flag: {String(enabled)} · Catalog reads at last render: {reads}</p>
  <section className="rounded-xl border p-4"><h2 className="mb-3 font-semibold">Service MCP catalog</h2><McpCatalogTab serviceID="fixture" versionID="1" canRead canManage /></section>
  <section className="rounded-xl border p-4"><h2 className="mb-3 font-semibold">App capability selection</h2><ImportedMcpPicker serviceID="fixture" versionID="1" value={selection} onChange={select} /></section>
 </main>;
}
createRoot(document.getElementById("root")!).render(<Preview />);
