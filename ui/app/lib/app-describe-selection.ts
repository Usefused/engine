import { api, type Service, type ServiceVersion, type IntegrationObject, type WebhookObject } from "./api";
import type { AppServicePin } from "./app-describe-contract";

export interface DescribedServiceData {
  service: Service;
  integrations: IntegrationObject[];
  webhooks: WebhookObject[];
  serviceVersions: ServiceVersion[];
}

/** Hydrates exact pins into the existing picker's row IDs without activating workspace dependencies. */
export async function loadDescribedSelection(services: Record<string, AppServicePin>) {
  const entries = await Promise.all(Object.entries(services).map(async ([key, pin]) => {
    const result = await api.graphql<{ service: Service | null; serviceVersions: ServiceVersion[]; serviceOperations: Array<IntegrationObject & { resource_name?: string }> }>(`query DescribeSelectionContract($id: String!, $version: String!) {
      service(id: $id, version: $version) {
        id name slug canonical_ref provider { name handle } description current_service_version base_url
        servers { url description environment is_default }
        endpoint_count webhook_count event_extraction_path incoming_webhook_config { auth_type }
        resources { id name }
        webhooks { id name description method }
      }
      serviceVersions(serviceId: $id) { id name header_value status }
      serviceOperations(serviceId: $id, version: $version) { id service_id name description method path resource_name }
    }`, { id: pin.service_id, version: pin.version });
    const version = result.serviceVersions.find((candidate) => candidate.id === pin.service_version_id && candidate.name === pin.version);
    // A vanished or retired snapshot must not be substituted by the current version.
    if (!result.service || !version || version.status === "deprecated") throw new Error(`The selected version of ${key} is unavailable.`);
    const operations = result.serviceOperations.filter((operation) => pin.operations.includes(operation.name));
    const webhooks = (result.service.webhooks ?? []).filter((event) => pin.webhooks?.includes(event.name));
    // Exact names from discovery must still exist in the immutable contract before entering manual review.
    if (operations.length !== pin.operations.length || webhooks.length !== (pin.webhooks?.length ?? 0)) throw new Error(`Selected operations or events for ${key} are unavailable.`);
    // A complete operation catalogue needs no lazy resource placeholders; retaining both would show duplicate groups.
    // Contracts without resource metadata retain the existing picker's path-derived grouping.
    const integrations = result.serviceOperations.map((operation) => ({ ...operation, resource: operation.resource_name || "" }));
    const data: DescribedServiceData = { service: { ...result.service, resources: [] }, integrations, webhooks: result.service.webhooks ?? [], serviceVersions: result.serviceVersions };
    return { pin, data, operationIDs: new Set(operations.map((operation) => operation.id)), webhookIDs: new Set(webhooks.map((event) => event.id)) };
  }));
  return {
    data: entries.map((entry) => entry.data),
    selections: Object.fromEntries(entries.map(({ pin, operationIDs }) => [pin.service_id, operationIDs])),
    webhookSelections: Object.fromEntries(entries.map(({ pin, webhookIDs }) => [pin.service_id, webhookIDs])),
    selectAllServices: new Set(entries.filter(({ pin }) => pin.select_all).map(({ pin }) => pin.service_id)),
    versionSelections: Object.fromEntries(entries.map(({ pin }) => [pin.service_id, pin.service_version_id])),
    loadedServices: Object.fromEntries(entries.map(({ pin }) => [pin.service_id, true])),
  };
}
