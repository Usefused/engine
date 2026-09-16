import { z } from "zod";
import { toJsonSchemaCompat } from "@modelcontextprotocol/sdk/server/zod-json-schema-compat.js";
import type { Fixture, FixtureOperation, FixtureUnifiedOperation, FixtureSchemaContract } from "./fixture.js";
import { searchDocs, type DocumentationSection } from "./searchDocs.js";
import { serializeBoundedJson, DOCUMENTATION_OUTPUT_POLICY } from "./outputLimits.js";
import { SEARCH_DOCS_DEFINITION, EXECUTE_DEFINITION } from "./toolDefinitions.js";

/** Carries only the already-authorized, schema-admitted Go catalogue into trusted search code. */
interface Catalogue {
  operations?: FixtureOperation[] | null;
  unified_operations?: { operations: FixtureUnifiedOperation[] };
  schema_definitions?: Record<string, Record<string, FixtureSchemaContract>>;
}

/** Projects the same registered definitions as the sessionful SDK without starting a server. */
export function listTools() {
  return { tools: Object.entries({ search_docs: SEARCH_DOCS_DEFINITION, execute: EXECUTE_DEFINITION }).map(([name, definition]) => ({
    ...definition, name, inputSchema: toJsonSchemaCompat(z.object(definition.inputSchema), { strictUnions: true, pipeStrategy: "input" }),
  })) };
}

/** Searches a request-local catalogue with exactly the compatibility runtime's validation and output policy. */
export function search(catalogue: Catalogue, arguments_: unknown) {
  const args = z.object(SEARCH_DOCS_DEFINITION.inputSchema).safeParse(arguments_);
  // Invalid inputs must fail before search work and never leak schema or request values in diagnostics.
  if (!args.success) {
    return { content: [{ type: "text", text: "Invalid search_docs arguments" }], isError: true };
  }
  const operations = catalogue.operations ?? [];
  const unifiedOperations = catalogue.unified_operations?.operations ?? [];
  const physical = new Map(operations.map(operation => [operation.operation_id, operation]));
  const unified = new Map(unifiedOperations.map(operation => [operation.name, operation]));
  // Go has already validated and scoped this catalogue; these indexes carry no execution state or credentials.
  const fixture = { operations, unifiedOperations, schemaDefinitions: catalogue.schema_definitions ?? {},
    resolve: (id: string) => physical.get(id), resolveUnified: (id: string) => unified.get(id),
  } as Fixture;
  const result = searchDocs(fixture, { ...args.data, section: args.data.section as DocumentationSection | undefined });
  const output = serializeBoundedJson(result, DOCUMENTATION_OUTPUT_POLICY);
  return { content: [{ type: "text", text: output.text }], isError: output.isError };
}
