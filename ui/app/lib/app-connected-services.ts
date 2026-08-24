import {
  requireAppSelectionsV3,
  type AppSelectionPayload,
  type AppSelectionV3,
} from "./app-selection-v3.ts";

export interface AppServiceSummary {
  service_id: string;
  service_slug: string;
  service_name: string;
  version?: string;
  select_all: boolean;
  endpoint_count: number;
  webhook_count: number;
}

export type AppConnectedServiceSelection = AppSelectionV3 & {
  service_name?: string;
  service_slug?: string;
  service_version_name?: string;
  endpoint_count?: number;
  webhook_count?: number;
};

/** Joins batched Engine-local labels to the immutable selection contract. */
export function appConnectedServiceSelections(
  selections: AppSelectionPayload[],
  services: AppServiceSummary[],
): AppConnectedServiceSelection[] {
  // One map keeps the join linear and avoids any per-selection data fetches.
  const serviceById = new Map(services.map((service) => [service.service_id, service]));
  return requireAppSelectionsV3(selections).map((selection) => {
    const service = serviceById.get(selection.service_id);
    return {
      ...selection,
      service_slug: service?.service_slug,
      service_name: service?.service_name,
      service_version_name: service?.version,
      endpoint_count: service?.endpoint_count,
      webhook_count: service?.webhook_count,
    };
  });
}
