export const APP_SELECTION_DEFINITION_SCHEMA_VERSION = 3;

export type AppSelectionPayload = {
  service_id: string;
  service_version_id?: string | null;
  definition_schema_version: number;
  endpoint_ids: string[];
  operation_names?: string[];
  webhook_ids: string[];
  webhook_names?: string[];
  select_all: boolean;
  webhook_select_all: boolean;
};

export type AppSelectionDisplayRow = {
  id: string;
  name: string;
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

/** Builds stable display rows from the semantic names stored with an immutable app selection. */
export function appSelectionDisplayRows(ids: string[], names: string[] | undefined): AppSelectionDisplayRow[] {
  const labels = names ?? [];
  // Semantic names are the immutable authored selection and do not need to be
  // positionally paired with Registry endpoint identities for presentation.
  if (labels.length > 0) {
    return labels.map((name, index) => ({ id: `semantic:${index}:${name}`, name }));
  }
  // The UUID fallback keeps historical ID-only rows visible without consulting
  // mutable Registry state to relabel an already-applied app version.
  return ids.map((id) => ({ id, name: id }));
}
