const FALLBACK_PATH = "/integrations";
const INTERNAL_ORIGIN = "https://engine.local";

// Login return paths are caller-controlled query values. Resolving against a
// fixed origin rejects protocol-relative, absolute, backslash, and scheme-based
// redirects while preserving ordinary internal query strings and fragments.
export function safeInternalPath(candidate: string | null, fallback = FALLBACK_PATH): string {
  if (!candidate || !candidate.startsWith("/") || candidate.startsWith("//") || candidate.includes("\\")) {
    return fallback;
  }
  try {
    const resolved = new URL(candidate, INTERNAL_ORIGIN);
    if (resolved.origin !== INTERNAL_ORIGIN) return fallback;
    return `${resolved.pathname}${resolved.search}${resolved.hash}`;
  } catch {
    return fallback;
  }
}
