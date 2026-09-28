import type { SelectedOperation } from "./bundle";
import { renderPublicType } from "./client_schema";

// Keep one stable alias pair per selected endpoint even when provider names contain punctuation.
function operationTypes(operation: SelectedOperation, index: number): string {
  const input = operation.inputSchema
    ? renderPublicType(operation.inputSchema, `Operation${index}Input`)
    : `export type Operation${index}Input = Record<string, unknown>;`;
  const output = operation.outputSchema
    ? renderPublicType(operation.outputSchema, `Operation${index}Output`)
    : `export type Operation${index}Output = unknown;`;
  return `${input}\n${output}`;
}

// Group only selected author keys so absent operations are not exposed by generated bindings.
function serviceGroups(operations: readonly SelectedOperation[]): Map<string, Array<[SelectedOperation, number]>> {
  const groups = new Map<string, Array<[SelectedOperation, number]>>();
  for (const [index, operation] of operations.entries()) {
    // The first operation establishes the service group used by later methods.
    if (!groups.has(operation.service)) groups.set(operation.service, []);
    groups.get(operation.service)!.push([operation, index]);
  }
  return groups;
}

// Render the in-bundle methods through fused.fetch so Engine remains the sole admission authority.
export function generateExecutionBindingsSource(operations: readonly SelectedOperation[]): string {
  const types = operations.map(operationTypes).join("\n");
  const groups = [...serviceGroups(operations)].map(([service, entries]) => {
    const methods = entries.map(([operation, index]) => `${JSON.stringify(operation.operation)}: (input: Operation${index}Input): Promise<Operation${index}Output> => fused.fetch({ service: ${JSON.stringify(service)}, operation: ${JSON.stringify(operation.operation)}, input: input as JsonObject }) as Promise<unknown> as Promise<Operation${index}Output>`);
    return `${JSON.stringify(service)}: { ${methods.join(", ")} }`;
  });
  return `import { fused, type JsonObject } from "@fused/execution";\n${types}\nexport const services = { ${groups.join(", ")} };`;
}

// Emit author-facing declarations from reviewed schema hints without adding those hints to the manifest.
export function generateExecutionBindingsDeclaration(operations: readonly SelectedOperation[]): string {
  const types = operations.map(operationTypes).join("\n");
  const groups = [...serviceGroups(operations)].map(([service, entries]) => {
    const methods = entries.map(([operation, index]) => `readonly ${JSON.stringify(operation.operation)}: (input: Operation${index}Input) => Promise<Operation${index}Output>;`);
    return `readonly ${JSON.stringify(service)}: { ${methods.join(" ")} };`;
  });
  return `declare module "@fused/operations" {\n${types}\nexport const services: { ${groups.join(" ")} };\n}\n`;
}
