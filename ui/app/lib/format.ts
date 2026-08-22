export function formatServiceName(name: string): string {
  return name.replace(/_/g, " ").replace(/\b\w/g, (c) => c.toUpperCase());
}

// Versions are provider-defined identifiers, not numbers. Preserve them
// exactly instead of assuming they need a leading "v".
export function formatVersion(version: string): string {
  return version;
}

export function stripLinks(html: string | undefined | null): string {
  if (!html) return "";
  return html.replace(/<a\b[^>]*>/gi, "").replace(/<\/a>/gi, "");
}
