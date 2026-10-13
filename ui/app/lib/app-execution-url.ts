/** Prefers Unified App names while retaining exact SDK IDs and an explicitly configured Engine origin. */
export function appExecutionURL(appID: string, publicURL = "", appName = ""): string {
  const name = appName.trim();
  // UUID-shaped names are interpreted as IDs by Engine, so use the known family ID for those names.
  const uuid = /^(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|\{[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\}|urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$/i;
  const reference = name && !uuid.test(name) ? name : appID;
  const path = `/v1/apps/${encodeURIComponent(reference)}/executions`;
  try {
    const base = new URL(publicURL);
    // Do not propagate credentials or routing parameters into a copied endpoint.
    if (!["https:", "http:"].includes(base.protocol) || base.username || base.password || base.search || base.hash) return path;
    return `${publicURL.replace(/\/+$/, "")}${path}`;
  // Without a usable public origin, copy a path callers can join to their Engine URL.
  } catch { return path; }
}
