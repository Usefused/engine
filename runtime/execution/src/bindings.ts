import type { SelectedOperation } from "./bundle";
import { renderPublicType } from "./client_schema";
import { operationAliases } from "./aliases";

// Keep one stable type pair per selected endpoint, including providers with punctuation in their names.
function operationTypes(operation: SelectedOperation, index: number): string {
  // Missing optional typing hints stay broad rather than inventing provider fields.
  const input = operation.inputSchema
    ? renderPublicType(operation.inputSchema, `Operation${index}Input`)
    : `export type Operation${index}Input = Record<string, unknown>;`;
  const output = operation.outputSchema
    ? renderPublicType(operation.outputSchema, `Operation${index}Output`)
    : `export type Operation${index}Output = unknown;`;
  return `${input}\n${output}`;
}

// Group only selected author keys so absent operations never become callable methods.
function serviceGroups(operations: readonly SelectedOperation[]): Map<string, Array<[SelectedOperation, number]>> {
  const groups = new Map<string, Array<[SelectedOperation, number]>>();
  for (const [index, operation] of operations.entries()) {
    // The first operation establishes its exact service namespace.
    if (!groups.has(operation.service)) groups.set(operation.service, []);
    groups.get(operation.service)!.push([operation, index]);
  }
  return groups;
}

// Emit the same selected methods as either declarations or Engine-backed runtime functions.
function serviceSource(service: string, entries: Array<[SelectedOperation, number]>, declaration: boolean): string {
  const methods = entries.map(([operation, index]) => {
    // Declarations expose reviewed types while execution always forwards the original provider keys.
    if (declaration) return `readonly ${JSON.stringify(operation.operation)}: (input: Operation${index}Input, options?: OperationOptions) => Promise<Operation${index}Output>;`;
    return `[${JSON.stringify(operation.operation)}]: (input: Operation${index}Input, options: OperationOptions = {}): Promise<Operation${index}Output> => transport.fetch({ ...options, service: ${JSON.stringify(service)}, operation: ${JSON.stringify(operation.operation)}, input: input as JsonObject }) as Promise<unknown> as Promise<Operation${index}Output>`;
  });
  // Computed keys keep even __proto__ an ordinary selected property at runtime.
  return declaration ? `readonly ${JSON.stringify(service)}: { ${methods.join(" ")} };` : `[${JSON.stringify(service)}]: Object.freeze({ ${methods.join(", ")} })`;
}

// Build safe dot-access aliases as references to the existing exact operation bindings.
function aliasedServices(operations: readonly SelectedOperation[], declaration: boolean): string {
  const groups = serviceGroups(operations);
  const services = operationAliases([...groups.keys()], true);
  return [...groups].map(([service, entries]) => {
    const aliases = operationAliases(entries.map(([operation]) => operation.operation), false);
    const methods = entries.map(([operation]) => `${JSON.stringify(aliases.get(operation.operation))}: ${declaration ? "ExactServices" : "services"}[${JSON.stringify(service)}][${JSON.stringify(operation.operation)}]`);
    return `${JSON.stringify(services.get(service))}: { ${methods.join(declaration ? "; " : ", ")} }`;
  }).join(declaration ? "; " : ", ");
}

// Keep SDK-style calls on the same fetch bridge, including selectors, pagination and connected users.
export function generateExecutionBindingsSource(operations: readonly SelectedOperation[]): string {
  const types = operations.map(operationTypes).join("\n");
  const exact = [...serviceGroups(operations)].map(([service, entries]) => serviceSource(service, entries, false)).join(", ");
  return `import { fused as runtime, type JsonObject, type WorkspaceOperationRequest } from "@fused/unified-app";
${types}
type OperationOptions = Pick<WorkspaceOperationRequest, "selector" | "pagination">;
// Each facade binds exact selected operations to one existing Engine transport.
function bindOperations(transport: Pick<typeof runtime, "fetch">) {
  const services = Object.freeze({ ${exact} });
  return { services, methods: { ${aliasedServices(operations, false)} } };
}
const bound = bindOperations(runtime);
export const services = bound.services;
export const fused = Object.freeze({ ...runtime, ...bound.methods,
  // A connected-user facade reuses the canonical reference validation and selector merge.
  forUserRef(ref: string) { const transport = runtime.forUserRef(ref); return Object.freeze({ ...transport, ...bindOperations(transport).methods }); },
  // Exact service keys continue to own per-service connected-user routing.
  forServiceUserRefs(refs: Readonly<Record<string, string>>) { const transport = runtime.forServiceUserRefs(refs); return Object.freeze({ ...transport, ...bindOperations(transport).methods }); }
});`;
}

// Emit the same callable surface for local TypeScript checks without changing authorization manifests.
export function generateExecutionBindingsDeclaration(operations: readonly SelectedOperation[]): string {
  const types = operations.map(operationTypes).join("\n");
  const exact = [...serviceGroups(operations)].map(([service, entries]) => serviceSource(service, entries, true)).join(" ");
  return `declare module "@fused/operations" {
import { fused as runtime, type WorkspaceOperationRequest } from "@fused/unified-app";
${types}
export type OperationOptions = Pick<WorkspaceOperationRequest, "selector" | "pagination">;
type ExactServices = { ${exact} };
type SelectedServices = { ${aliasedServices(operations, true)} };
type BoundFused = ReturnType<typeof runtime.forUserRef> & SelectedServices;
export const services: ExactServices;
export const fused: Omit<typeof runtime, "forUserRef" | "forServiceUserRefs"> & SelectedServices & {
  forUserRef(ref: string): BoundFused;
  forServiceUserRefs(refs: Readonly<Record<string, string>>): BoundFused;
};
}\n`;
}
