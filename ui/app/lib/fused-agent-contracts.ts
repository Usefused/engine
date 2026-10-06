import { api } from "./api";
import type { AppServicePin } from "./app-describe-contract";

/** Reads bounded pages from the same immutable, authorized contracts used by Registry drafting. */
export async function readAgentContracts(services: Record<string, AppServicePin>, path = "", offset = 0) {
  const selections = Object.entries(services).flatMap(([service, pin]) => [
    ...pin.operations.map((operation) => ({ service, service_id: pin.service_id, version: pin.version, operation })),
    ...(pin.webhooks ?? []).map((event) => ({ service, service_id: pin.service_id, version: pin.version, event })),
  ]);
  const result = await api.graphql<{ agentOperationContracts: string }>(`query FusedAgentContracts($selections: String!, $path: String!, $offset: Int!) {
    agentOperationContracts(selections: $selections, path: $path, offset: $offset)
  }`, { selections: JSON.stringify(selections), path, offset });
  return JSON.parse(result.agentOperationContracts);
}
