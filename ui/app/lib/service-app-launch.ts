import type { AppBuildSelector, AppBuildSelectorPage } from "./app-builder-contract";

export type ServiceAppSource = { id: string; name: string };

/** Carries exact service identity into manual creation without preselecting any operations. */
export function serviceAppHref(destination: string, service?: ServiceAppSource): string {
  // Catalogue creation has no service context to carry forward.
  if (!service) return destination;
  const [pathname, search] = destination.split("?");
  const params = new URLSearchParams(search);
  params.set("serviceId", service.id);
  params.set("serviceName", service.name);
  params.set("mode", "manual");
  return `${pathname}?${params}`;
}

/** Resolves a deep link through owner-aware Engine selectors before any Registry metadata is loaded. */
export async function findServiceAppSource(
  serviceID: string,
  readPage: (limit: number, offset: number) => Promise<AppBuildSelectorPage>,
): Promise<AppBuildSelector> {
  let offset = 0;
  for (let pageNumber = 0; pageNumber < 100; pageNumber++) {
    const page = await readPage(100, offset);
    const selected = page.items.find((item) => item.resource_id === serviceID);
    // Names only narrow the search; duplicate names never substitute for the requested identity.
    if (selected) return selected;
    offset += page.items.length;
    // Exhaustion or a stalled page cannot authorize a service that was not returned.
    if (offset >= page.total || page.items.length === 0) break;
  }
  throw new Error("This service is not available for the selected app owner. Choose another service or owner.");
}
