import { loginPathForLocation } from "./safe-navigation.ts";

const SAFE_METHODS = new Set(["GET", "HEAD", "OPTIONS"]);

export function credentialedRequestInit(
  init: RequestInit = {},
  csrfToken: string | null = null
): RequestInit {
  const headers = new Headers(init.headers);
  if (!headers.has("Content-Type")) headers.set("Content-Type", "application/json");
  const method = (init.method || "GET").toUpperCase();
  if (!SAFE_METHODS.has(method) && csrfToken) headers.set("X-Fused-CSRF", csrfToken);
  return { ...init, credentials: "include", headers };
}

// credentialedResponseLoginPath preserves the current same-origin destination for shared 401 handling.
export function credentialedResponseLoginPath(status: number, pathname: string, search: string): string | null {
  if (status !== 401) return null;
  // Authentication routes and the public root must not recursively redirect themselves.
  if (pathname === "/login" || pathname === "/cli-login" || pathname === "/") return null;
  return loginPathForLocation(pathname, search);
}
