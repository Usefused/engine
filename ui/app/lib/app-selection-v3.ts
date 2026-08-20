export const APP_SELECTION_DEFINITION_SCHEMA_VERSION = 3;

export type AppSelectionPayload = {
  service_id: string;
  service_version_id?: string | null;
  definition_schema_version: number;
  endpoint_ids: string[];
  webhook_ids: string[];
  select_all: boolean;
  webhook_select_all: boolean;
};

export type AppSelectionV3 = Omit<
  AppSelectionPayload,
  "service_version_id" | "definition_schema_version"
> & {
  service_version_id: string;
  definition_schema_version: typeof APP_SELECTION_DEFINITION_SCHEMA_VERSION;
};

/** Converts GraphQL payloads into the v3 selection invariant used by the UI. */
export function requireAppSelectionV3(selection: AppSelectionPayload): AppSelectionV3 {
  // Rendering an older definition as current would hide a server/UI contract
  // mismatch, so reject it at the response boundary with a stable message.
  if (selection.definition_schema_version !== APP_SELECTION_DEFINITION_SCHEMA_VERSION) {
    throw new Error("This app uses an unsupported selection schema version.");
  }
  // The GraphQL field remains nullable for historical rows, while every v3
  // selection must pin an immutable service version before the UI consumes it.
  if (!selection.service_version_id) {
    throw new Error("This app selection is missing its service version.");
  }
  return {
    ...selection,
    service_version_id: selection.service_version_id,
    definition_schema_version: APP_SELECTION_DEFINITION_SCHEMA_VERSION,
  };
}

/** Validates the complete app selection list before detail rendering begins. */
export function requireAppSelectionsV3(selections: AppSelectionPayload[]): AppSelectionV3[] {
  return selections.map(requireAppSelectionV3);
}
