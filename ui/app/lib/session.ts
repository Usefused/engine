import Cookies from "js-cookie";

const KEY = "fused_api_key";

export function getApiKey(): string | null {
  if (typeof window === "undefined") return null;
  return Cookies.get(KEY) || null;
}

export function setApiKey(key: string): void {
  Cookies.set(KEY, key, { expires: 30, path: "/" });
}

export function clearApiKey(): void {
  Cookies.remove(KEY, { path: "/" });
}

export function isAuthenticated(): boolean {
  return !!getApiKey();
}

export function logoutAndRedirect(): never {
  clearApiKey();
  if (typeof window !== "undefined") {
    window.location.href = "/login";
  }
  throw new Error("Redirecting to login");
}
