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
