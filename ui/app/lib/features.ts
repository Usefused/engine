/** Mirrors the Engine opt-in for presentation; backend checks remain authoritative. */
export function importedMcpEnabled(): boolean {
  // Server rendering and older Engine shells default to hiding the experimental surface.
  if (typeof window === "undefined") return false;
  return (window as Window & { __FUSED_ENV?: { IMPORTED_MCP_ENABLED?: boolean } }).__FUSED_ENV?.IMPORTED_MCP_ENABLED === true;
}
