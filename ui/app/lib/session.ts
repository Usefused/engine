const KEY = "fused_api_key";
let inMemoryKey: string | null = null;

export function getApiKey(): string | null {
  if (typeof window === "undefined") return null;
  try {
    return window.sessionStorage.getItem(KEY) || inMemoryKey;
  } catch {
    return inMemoryKey;
  }
}

export function setApiKey(key: string): void {
  inMemoryKey = key;
  try {
    window.sessionStorage.setItem(KEY, key);
  } catch {
    // The in-memory fallback keeps the current tab usable when browser policy
    // disables storage without extending credential lifetime beyond this page.
  }
}

export function clearApiKey(): void {
  inMemoryKey = null;
  if (typeof window === "undefined") return;
  try {
    window.sessionStorage.removeItem(KEY);
  } catch {
    // Clearing the in-memory copy is sufficient when storage is unavailable.
  }
}

export function isAuthenticated(): boolean {
  return !!getApiKey();
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

  const newTab = window.open("about:blank", "_blank");
  if (!newTab) return false;

  const key = getApiKey();
  if (key) {
    try {
      newTab.sessionStorage.setItem(KEY, key);
    } catch {
      // The destination remains usable for public routes when storage is
      // unavailable. Authenticated routes will show the normal sign-in flow.
    }
  }

  try {
    newTab.opener = null;
  } catch {
    // Some browsers expose opener as read-only. Navigation remains same-origin.
  }
  newTab.location.replace(path);
  return true;
}

export function logoutAndRedirect(): never {
  clearApiKey();
  if (typeof window !== "undefined") {
    window.location.href = "/login";
  }
  throw new Error("Redirecting to login");
}
