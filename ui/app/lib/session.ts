const LEGACY_KEY = "fused_api_key";
const CSRF_COOKIE_NAMES = ["__Host-fused_csrf", "fused_csrf_dev"];

export function purgeLegacyBrowserCredential(): void {
  if (typeof window === "undefined") return;
  try {
    window.sessionStorage.removeItem(LEGACY_KEY);
  } catch {
    // Browser policy may disable storage; there is no in-memory credential
    // fallback because browser authentication is cookie-only now.
  }
}

export function getCSRFToken(): string | null {
  if (typeof document === "undefined") return null;
  const values = new Map<string, string>();
  for (const part of document.cookie.split(";")) {
    const [rawName, ...rawValue] = part.trim().split("=");
    if (CSRF_COOKIE_NAMES.includes(rawName)) values.set(rawName, rawValue.join("="));
  }
  for (const name of CSRF_COOKIE_NAMES) {
    const value = values.get(name);
    if (value === undefined) continue;
    try {
      return decodeURIComponent(value);
    } catch {
      // Ignore a malformed stale cookie and try the active alternative.
    }
  }
  return null;
}

export function openAuthenticatedTab(path: string): boolean {
  if (
    typeof window === "undefined" ||
    !path.startsWith("/") ||
    path.startsWith("//") ||
    path.startsWith("/\\")
  ) {
    return false;
  }

  // Opening about:blank keeps a usable WindowProxy; passing noopener as a
  // feature makes compliant browsers return null even when the tab opened.
  const newTab = window.open("about:blank", "_blank");
  if (!newTab) return false;
  try {
    newTab.opener = null;
  } catch {
    newTab.close();
    return false;
  }
  newTab.location.replace(path);
  return true;
}
