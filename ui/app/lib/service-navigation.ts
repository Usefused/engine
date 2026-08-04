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
