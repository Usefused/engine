const TITLES: Record<string, string> = {
  "/": "Fused",
  "/login": "Sign in - Fused",
  "/integrations": "Services - Fused",
  "/integrations/sdks": "Apps - Fused",
  "/integrations/mcp": "MCP servers - Fused",
  "/integrations/access/people": "People - Fused",
  "/integrations/access/teams": "Teams - Fused",
  "/integrations/buckets": "Credentials - Fused",
	"/integrations/activity": "Activity - Fused",
  "/integrations/settings": "Settings - Fused",
};

export function routeTitle(pathname: string, search = ""): string {
  const normalizedPath = pathname.length > 1 ? pathname.replace(/\/$/, "") : pathname;
  if (normalizedPath === "/integrations/builder") {
    return new URLSearchParams(search).get("tab") === "mcp"
      ? "Create MCP server - Fused"
      : "Create app - Fused";
  }
  if (TITLES[normalizedPath]) return TITLES[normalizedPath];
  if (/^\/integrations\/mcp\/[^/]+\/analytics$/.test(normalizedPath)) {
    return "MCP server activity - Fused";
  }
  if (/^\/integrations\/mcp\/[^/]+$/.test(normalizedPath)) {
    return "MCP server details - Fused";
  }
  if (/^\/integrations\/sdks\/[^/]+$/.test(normalizedPath)) {
    return "App details - Fused";
  }
  if (/^\/integrations\/[^/]+(?:\/[^/]+)?$/.test(normalizedPath)) {
    return "Service details - Fused";
  }
  return "Fused";
}
