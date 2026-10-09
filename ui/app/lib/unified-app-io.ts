import { resolveSchemaPointer, schemaPointerTokens } from "./schema-reference.ts";

export type AppSchema = Record<string, unknown> | boolean;
export type UnifiedAppContract = { input: AppSchema; output: AppSchema };

/** Accepts boolean schemas as well as object schemas without inventing an empty contract. */
function isSchema(value: unknown): value is AppSchema {
  return typeof value === "boolean" || (value !== null && typeof value === "object" && !Array.isArray(value));
}

/** Reads an exact contract location without following external references or prototype properties. */
function at(document: unknown, ...tokens: string[]): unknown {
  return resolveSchemaPointer(document, tokens).value;
}

/** Resolves the single branch returned by Engine's execute-only OpenAPI export. */
function branch(document: unknown, schema: unknown): unknown {
  const variants = at(schema, "oneOf");
  // More than one variant means this is not the singular authored contract requested by the page.
  if (!Array.isArray(variants) || variants.length !== 1) throw new Error("The app's input and output contract is unavailable.");
  let value: unknown = variants[0];
  const visited = new Set<string>();
  while (typeof at(value, "$ref") === "string") {
    const reference = at(value, "$ref") as string;
    const tokens = schemaPointerTokens(reference);
    // Broken, remote, or cyclic envelope references must not appear as a successful empty schema.
    if (!tokens || visited.has(reference)) throw new Error("The app's schema reference could not be resolved.");
    visited.add(reference);
    value = resolveSchemaPointer(document, tokens).value;
  }
  return value;
}

/** Extracts authored input/output, excluding Engine receipt metadata and provider-operation schemas. */
export function unifiedAppContract(document: unknown): UnifiedAppContract {
  const operation = at(document, "paths", "/v1/apps/{app_id}/executions", "post");
  const request = branch(document, at(operation, "requestBody", "content", "application/json", "schema"));
  const response = branch(document, at(operation, "responses", "200", "content", "application/json", "schema"));
  const input = at(request, "properties", "input");
  const output = at(response, "properties", "output");
  // A physical operation must never be mislabeled as the app's authored execute interface.
  if (at(request, "properties", "operation", "const") !== "execute" || !isSchema(input) || !isSchema(output)) {
    throw new Error("The app's input and output contract is unavailable.");
  }
  return { input, output };
}
