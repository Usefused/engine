type Schema = { type?: unknown; properties?: object; additionalProperties?: unknown; required?: unknown } | boolean | null | undefined;

/** Only a closed, structurally empty object proves that an operation accepts no arguments. */
export function bridgeHasNoInput(schema: Schema): boolean {
  // Unknown, composed, and referenced schemas must not be mistaken for zero-argument contracts.
  if (!schema || typeof schema !== "object") return false;
  const simpleKeys = new Set(["type", "properties", "additionalProperties", "title", "description", "required"]);
  return schema.type === "object" && schema.additionalProperties === false &&
    Object.keys(schema.properties ?? {}).length === 0 &&
    (!Array.isArray(schema.required) || schema.required.length === 0) &&
    Object.keys(schema).every(/** Other validation keywords may impose additional input requirements. */ key => simpleKeys.has(key));
}

/** Distinguish missing contracts, empty inputs, and open objects without claiming that results are empty. */
export function bridgeSchemaSummary(schema: Schema, label: "Input" | "Output"): string | null {
  // Absent contracts carry no evidence about permitted input or returned values.
  if (schema == null) return "The Bridge has not supplied a schema for this operation.";
  // A closed empty object is intentional, not unfinished inference.
  if (bridgeHasNoInput(schema)) return label === "Input" ? "No input required. Pass {} when calling this operation." : "The declared output is an empty object ({}).";
  // A bare object allows fields but does not describe them; do not invent their names or values.
  if (typeof schema === "object" && schema.type === "object" && Object.keys(schema).every(/** Metadata alone does not define response fields. */ key => ["type", "title", "description"].includes(key))) return "The Bridge declares an object but has not described its fields. This does not mean the data is empty.";
  return null;
}

/** Keep the code alias and configuration pin identical for the selected profile and connection. */
export function bridgeUsage(pairing: string, profile: string, hash: string, connection: string, operation: string, contract: { input_schema?: Schema }): { binding: string; code: string } {
  const alias = "local_operation";
  const binding = JSON.stringify({ bridges: { [alias]: { pairing_id: pairing, profile_id: profile, manifest_hash: hash, connection_id: connection, operation_id: operation } } }, null, 2);
  // Only a proven zero-argument contract gets an empty-object example; other calls use authored input.
  const input = bridgeHasNoInput(contract.input_schema) ? "{}" : "input";
  const code = `import { fused } from "@fused/operations";

// Inside execute({ input }); bind the alias in your app configuration.
const result = await fused.callBridge(
  "${alias}",
  ${input},
);`;
  return { binding, code };
}

/** Group a profile's filtered catalogue by exact connection name without merging repeated operation IDs. */
export function groupBridgeOperations<T extends { connection_id: string; operation_id: string }>(operations: T[], query: string): { connection: string; operations: T[] }[] {
  const search = query.trim().toLowerCase();
  const groups = new Map<string, T[]>();
  // Map keys safely preserve arbitrary connection names, including names that resemble object properties.
  for (const operation of operations) {
    // Matching a connection includes all its operations; an operation match keeps its connection heading.
    if (!`${operation.connection_id} ${operation.operation_id}`.toLowerCase().includes(search)) continue;
    const group = groups.get(operation.connection_id) ?? [];
    group.push(operation);
    groups.set(operation.connection_id, group);
  }
  return Array.from(groups, /** Preserve the accepted operation order within each named connection. */ ([connection, entries]) => ({ connection, operations: entries }));
}
