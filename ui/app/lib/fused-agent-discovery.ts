import { api } from "./api";
import { readAgentContracts } from "./fused-agent-contracts";

/** Rejects unbounded searches before sending an authenticated catalogue request. */
function validateSearch(query: string, offset: number) {
  // Search terms and paging must stay small even when the caller is a model.
  if (typeof query !== "string" || query.length > 512 || !Number.isSafeInteger(offset) || offset < 0 || offset > 10000) throw new Error("Use a search of at most 512 characters and an offset between 0 and 10,000.");
}

/** Discovers visible catalogue identities without exposing connection configuration or credentials. */
export async function searchAgentServices(query: string, offset = 0) {
  validateSearch(query, offset);
  const result = await api.graphql<{ searchServices: unknown[] }>(`query FusedAgentSearchServices($q: String!) {
    searchServices(q: $q) { id name slug canonical_ref provider { name handle } }
  }`, { q: query });
  const items = result.searchServices.slice(offset, offset + 20);
  return { items, total: result.searchServices.length, next_offset: offset + items.length < result.searchServices.length ? offset + items.length : null };
}

/** Resolves explicit service versions before browsing endpoints so a typo cannot broaden the search. */
async function serviceVersions(serviceID: string) {
  // IDs must come from discovery; reject missing identities instead of querying an unscoped catalogue.
  if (typeof serviceID !== "string" || !/^[0-9a-f-]{36}$/i.test(serviceID)) throw new Error("Use a service ID returned by search_services or the current editor.");
  const result = await api.graphql<{ serviceVersions: Array<{ id: string; name: string; status: string }> }>(`query FusedAgentServiceVersions($id: String!) {
    serviceVersions(serviceId: $id) { id name status }
  }`, { id: serviceID });
  return result.serviceVersions;
}

/** Lists versions first, then returns a bounded page of endpoints from one exact observed contract. */
export async function searchAgentOperations(serviceID: string, version = "", query = "", offset = 0) {
  validateSearch(query, offset);
  const versions = await serviceVersions(serviceID);
  // Browsing without a version helps the agent choose explicitly, never silently upgrading an existing app.
  if (!version) return { versions: versions.slice(offset, offset + 20), next_offset: offset + 20 < versions.length ? offset + 20 : null, next: "Choose the app's pinned version, or an explicit version from this list." };
  const pin = versions.find(item => item.name === version || item.id === version);
  // Registry endpoint search can otherwise fall back to an unfiltered catalogue for an unknown version.
  if (!pin) throw new Error("Service version not found. Browse versions before searching endpoints.");
  const result = await api.graphql<{ searchEndpoints: unknown[] }>(`query FusedAgentSearchOperations($id: String!, $version: String!, $q: String!, $offset: Int!) {
    searchEndpoints(serviceId: $id, version: $version, q: $q, limit: 20, offset: $offset) { name description method path deprecated }
  }`, { id: serviceID, version: pin.name, q: query, offset });
  return { version: pin.name, items: result.searchEndpoints, next_offset: result.searchEndpoints.length === 20 ? offset + 20 : null };
}

/** Reads one discovered endpoint's exact contract without adding it to an app or executing it. */
export async function readAgentServiceContract(serviceID: string, version: string, operation: string, path = "", offset = 0) {
  validateSearch(path, offset);
  // Contract reads remain explicit and bounded; the Registry verifies the operation belongs to this version.
  if (!version || !operation || operation.length > 256) throw new Error("Choose an exact version and operation from endpoint discovery.");
  const versions = await serviceVersions(serviceID);
  const pin = versions.find(item => item.name === version || item.id === version);
  // Missing pins must not resolve to latest or another provider's contract.
  if (!pin) throw new Error("Service version not found.");
  return readAgentContracts({ service: { service_id: serviceID, service_version_id: pin.id, version: pin.name, operations: [operation] } }, path, offset);
}
