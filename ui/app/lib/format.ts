export function formatServiceName(name: string): string {
  return name.replace(/_/g, " ").replace(/\b\w/g, (c) => c.toUpperCase());
}

export function stripLinks(html: string | undefined | null): string {
  if (!html) return "";
  return html.replace(/<a\b[^>]*>/gi, "").replace(/<\/a>/gi, "");
}
