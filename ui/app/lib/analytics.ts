declare global {
  interface Window {
    gtag?: (...args: unknown[]) => void;
  }
}

export function track(eventName: string, params?: Record<string, string | number | boolean>) {
  if (typeof window === "undefined" || !window.gtag) return;
  window.gtag("event", eventName, params);
}

export function useAnalytics() {
  return { track };
}
