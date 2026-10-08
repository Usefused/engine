import { createRoot } from "react-dom/client";
import McpCatalogTab from "./app/components/mcp/McpCatalogTab";

// The browser fixture mounts the production component while using isolated Engine test endpoints.
function Preview() {
  return <main className="mx-auto max-w-5xl space-y-6 p-8"><p className="text-xs text-slate-500">Fused · Isolated browser test</p><h1 className="text-3xl font-semibold text-slate-900">Fixture service</h1><div className="rounded-lg bg-slate-100 p-3 text-sm font-medium text-slate-900">Service details / MCP</div><McpCatalogTab serviceID="10000000-0000-4000-8000-000000000001" versionID="20000000-0000-4000-8000-000000000001" canRead={true} canManage={true} /></main>;
}
createRoot(document.getElementById("root")!).render(<Preview />);
