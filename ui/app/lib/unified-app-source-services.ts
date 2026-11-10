import { api } from "./api";
import type { UnifiedServicePin } from "./unified-app-contract";
import type { DescribeServiceCandidate } from "./app-describe-contract";

/** Resolves newly authored aliases only when an exact reference identifies one provider. */
async function sourceServiceIdentity(key: string): Promise<string> {
  const result = await api.graphql<{ serviceCandidatesByRefs: Array<{ candidates: DescribeServiceCandidate[] }> }>(`query UnifiedSourceServiceCandidates($refs: [String!]!, $limitPerRef: Int!) {
    serviceCandidatesByRefs(refs: $refs, limitPerRef: $limitPerRef) { candidates { id name slug provider { handle } } }
  }`, { refs: [key], limitPerRef: 20 });
  const exact = (result.serviceCandidatesByRefs[0]?.candidates ?? []).filter(/** Only an exact qualified reference, slug or name may select a provider from source text. */ (item) => [item.name, item.slug, `@${item.provider?.handle}/${item.slug}`].includes(key));
  // Ambiguity must be resolved in authored text instead of silently choosing another publisher.
  if (exact.length !== 1) throw new Error(`Use an unambiguous @publisher/service reference for “${key}”.`);
  return exact[0].id;
}

/** Resolves exact versions without changing a retained alias's provider or activating workspace dependencies. */
export async function resolveUnifiedSourceService(key: string, version: string, retainedID?: string): Promise<UnifiedServicePin> {
  // Existing aliases remain bound to their reviewed provider even when their version changes.
  const serviceID = retainedID ?? await sourceServiceIdentity(key);
  const versions = await api.graphql<{ serviceVersions: Array<{ id: string; name: string; status: string }> }>(`query UnifiedSourceServiceVersions($id: String!) {
    serviceVersions(serviceId: $id) { id name status }
  }`, { id: serviceID });
  const pin = versions.serviceVersions.find(/** Match the requested immutable version, never a latest-version fallback. */ (item) => item.name === version && item.status !== "deprecated");
  // A missing version cannot be substituted without changing reviewed operation scope.
  if (!pin) throw new Error(`Version “${version}” is unavailable for ${key}.`);
  return { service_id: serviceID, service_version_id: pin.id, version, operations: [] };
}
