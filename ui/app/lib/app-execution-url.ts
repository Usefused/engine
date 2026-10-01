/** Uses only an explicitly configured Engine origin; the UI host may be a separate development or proxy server. */
export function appExecutionURL(appID: string, publicURL = ""): string {
  const path = `/v1/apps/${encodeURIComponent(appID)}/executions`;
  try {
    const base = new URL(publicURL);
    // Do not propagate credentials or routing parameters into a copied endpoint.
    if (!["https:", "http:"].includes(base.protocol) || base.username || base.password || base.search || base.hash) return path;
    return `${publicURL.replace(/\/+$/, "")}${path}`;
  } catch { return path; }
}
