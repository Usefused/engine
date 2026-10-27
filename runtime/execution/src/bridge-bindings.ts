import { renderPublicType } from "./client_schema";

export interface SelectedBridge {
  group?: string;
  alias: string;
  connection_id: string;
  operation_id: string;
  kind: string;
  input_schema: Record<string, unknown>;
  output_schema: Record<string, unknown>;
}

type Branch = Map<string, Branch | number>;

// Validate reviewed identities before emitting computed property names.
function groupedPath(binding: SelectedBridge): string[] {
  if (!/^[a-z][a-z0-9_]{0,63}$/.test(binding.group ?? "")) throw new Error("Invalid Bridge group");
  if (!binding.connection_id || !binding.operation_id || !["read", "command"].includes(binding.kind)) throw new Error("Invalid grouped Bridge binding");
  const path = [binding.group!, binding.connection_id, ...binding.operation_id.split(".")];
  if (path.length > 18 || path.some(part => !part)) throw new Error("Invalid Bridge method path");
  return path;
}

// An operation cannot replace an existing method or a namespace of methods.
function insertPath(root: Branch, path: string[], index: number): void {
  let node = root;
  for (const [position, part] of path.entries()) {
    const prior = node.get(part);
    if (position === path.length - 1) {
      if (prior !== undefined) throw new Error("Conflicting Bridge method paths");
      node.set(part, index);
    } else {
      if (typeof prior === "number") throw new Error("Conflicting Bridge method paths");
      if (!prior) node.set(part, new Map());
      node = node.get(part) as Branch;
    }
  }
}

// Create a finite tree from reviewed selectors, never from runtime caller input.
function bridgeTree(bindings: readonly SelectedBridge[]): Branch {
  if (bindings.length > 100) throw new Error("At most 100 Bridge capabilities may be selected");
  const root: Branch = new Map();
  const aliases = new Set<string>();
  for (const [index, binding] of bindings.entries()) {
    if (!/^[a-z][a-z0-9_]{0,63}$/.test(binding.alias) || aliases.has(binding.alias)) throw new Error("Invalid or duplicate Bridge alias");
    aliases.add(binding.alias);
    if (binding.group) insertPath(root, groupedPath(binding), index);
  }
  return root;
}

function renderTree(node: Branch, bindings: readonly SelectedBridge[], declaration: boolean): string {
  const entries = [...node].map(([name, child]) => {
    const key = JSON.stringify(name);
    const value = typeof child === "number"
      ? declaration
        ? `(input: Bridge${child}Input) => Promise<Bridge${child}Output>`
        : `(input: Bridge${child}Input): Promise<Bridge${child}Output> => runtime.callBridge(${JSON.stringify(bindings[child].alias)}, input as JsonValue) as Promise<unknown> as Promise<Bridge${child}Output>`
      : renderTree(child, bindings, declaration);
    return declaration ? `readonly ${key}: ${value}` : `[${key}]: ${value}`;
  });
  return declaration ? `{ ${entries.join("; ")} }` : `Object.freeze({ ${entries.join(", ")} })`;
}

// A closed empty input accepts only {}, whereas TypeScript's bare {} accepts any non-null value.
function bridgeInputType(schema: Record<string, unknown>, index: number): string {
  const properties = schema.properties;
  if (schema.type === "object" && schema.additionalProperties === false && (!properties || Object.keys(properties as object).length === 0)) {
    return `export type Bridge${index}Input = Record<string, never>;`;
  }
  return renderPublicType(schema, `Bridge${index}Input`);
}

// Shared generation keeps declared TypeScript and emitted runtime routes in agreement.
export function generateBridgeBindings(bindings: readonly SelectedBridge[] = [], declaration = false): string {
  const tree = bridgeTree(bindings);
  const types = bindings.flatMap((binding,index) => binding.group ? [
    bridgeInputType(binding.input_schema, index),
    renderPublicType(binding.output_schema, `Bridge${index}Output`),
  ] : []).join("\n");
  return `${types}\nexport const bridges${declaration ? ": " : " = "}${renderTree(tree,bindings,declaration)};`;
}
