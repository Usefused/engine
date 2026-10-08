export type McpCatalogKind = "tools" | "prompts" | "resources" | "resource_templates";
export interface McpCatalogItem {
  name: string;
  title?: string;
  description?: string;
  uri?: string;
  uriTemplate?: string;
  mimeType?: string;
  inputSchema?: Record<string, unknown>;
  outputSchema?: Record<string, unknown>;
  arguments?: Array<{ name: string; description?: string; required?: boolean }>;
  [key: string]: unknown;
}
export interface McpCatalog {
  protocol_version: string;
  server: { name: string; version: string; title?: string };
  supported: Record<McpCatalogKind, boolean>;
  tools: McpCatalogItem[];
  prompts: McpCatalogItem[];
  resources: McpCatalogItem[];
  resource_templates: McpCatalogItem[];
}
export interface McpDiscoveryInput { url: string; bucket_name?: string; secret_name?: string }
export interface McpCatalogSnapshot {
  id: string;
  url: string;
  bucket_name?: string;
  secret_name?: string;
  created_at: string;
  catalog: McpCatalog;
}
export interface McpCatalogPreview extends McpCatalogSnapshot {
  expires_at: string;
  changes: { added: number; changed: number; removed: number };
}
export const mcpCatalogSections: Array<{ kind: McpCatalogKind; label: string }> = [
  { kind: "tools", label: "Tools" }, { kind: "prompts", label: "Prompts" },
  { kind: "resources", label: "Resources" }, { kind: "resource_templates", label: "Resource templates" },
];

/** Uses provider identity rather than a display title to keep resource rows distinct. */
export function mcpItemKey(kind: McpCatalogKind, item: McpCatalogItem): string {
  // Resource identity belongs to its URI, including parameterized templates.
  if (kind === "resources") return item.uri ?? item.name;
  if (kind === "resource_templates") return item.uriTemplate ?? item.name;
  return item.name;
}

/** Searches only human-readable catalog metadata; schema internals stay in the detail view. */
export function filterMcpItems(items: McpCatalogItem[], query: string): McpCatalogItem[] {
  const term = query.trim().toLocaleLowerCase();
  return items.filter((item) => [item.name, item.title, item.description, item.uri, item.uriTemplate].join(" ").toLocaleLowerCase().includes(term));
}

/** Builds one exact version-scoped URL for all catalog actions. */
export function mcpCatalogPath(serviceID: string, versionID: string): string {
  return `/workspace/services/${encodeURIComponent(serviceID)}/versions/${encodeURIComponent(versionID)}/mcp-catalog`;
}
