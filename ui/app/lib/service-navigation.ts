import { openAuthenticatedTab } from "./session.ts";

type ServiceLinkClick = Pick<
  MouseEvent,
  "defaultPrevented" | "button" | "metaKey" | "ctrlKey" | "shiftKey" | "altKey" | "preventDefault"
>;

// openServiceLink respects row-action cancellation before opening an authenticated detail tab.
export function openServiceLink(event: ServiceLinkClick, href: string): void {
  // Action buttons cancel navigation but still bubble so click tracking can observe their actions.
  if (event.defaultPrevented) {
    return;
  }
  // Modified and non-primary clicks retain the browser's native tab/window behavior.
  if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) {
    return;
  }
  // Preserve the ordinary link fallback when the browser cannot open a new tab.
  if (openAuthenticatedTab(href)) {
    event.preventDefault();
  }
}

export function serviceDetailPath(
  serviceId: string,
  serviceSlug?: string | null
): string {
  const slug = serviceSlug || serviceId;
  if (slug.startsWith("@")) {
    const [provider, serviceName] = slug.slice(1).split("/");
    if (provider && serviceName) {
      return `/integrations/${encodeURIComponent(
        provider
      )}/${encodeURIComponent(serviceName)}`;
    }
  }
  return `/integrations/${encodeURIComponent(slug)}`;
}
