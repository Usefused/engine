const TITLES: Record<string, string> = {
  "/": "Fused",
  "/login": "Sign in - Fused",
  "/privacy-policy": "Privacy Policy - Fused",
  "/terms-of-service": "Terms of Service - Fused",
  "/integrations": "Services - Fused",
  "/integrations/sdks": "Apps - Fused",
  "/integrations/workflows": "Workflows - Fused",
  "/integrations/workflows/install": "Add workflows - Fused",
  "/integrations/mcp": "Apps - Fused",
  "/integrations/access/people": "People - Fused",
  "/integrations/access/teams": "Teams - Fused",
  "/integrations/buckets": "Credentials - Fused",
	"/integrations/activity": "Activity - Fused",
  "/integrations/settings": "Settings - Fused",
};

/** Returns the adapter-specific builder title from its explicit query selection. */
function builderTitle(search: string): string {
  const mode = new URLSearchParams(search).get("tab");
  // Builder titles expose MCP without changing the route hierarchy.
  if (mode === "mcp") return "Create MCP server - Fused";
  // Direct REST is an explicit package-free choice with its own builder context.
  if (mode === "api") return "Create REST API - Fused";
  // SDK is explicit too; an absent or invalid choice remains the neutral chooser.
  if (mode === "sdk") return "Create SDK - Fused";
  return "Create app - Fused";
}

/** Resolves a concise browser title for every primary Engine UI route. */
export function routeTitle(pathname: string, search = ""): string {
  const normalizedPath = pathname.length > 1 ? pathname.replace(/\/$/, "") : pathname;
  if (normalizedPath === "/integrations/builder") {
    return builderTitle(search);
  }
  if (TITLES[normalizedPath]) return TITLES[normalizedPath];
  // Workflow detail pages are Engine-owned catalogue pages.
  if (normalizedPath.startsWith("/integrations/workflows/")) return "Workflow details - Fused";
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
