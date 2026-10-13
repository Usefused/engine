import { resolveSchemaPointer, schemaPointerTokens } from "./schema-reference.ts";

export type InputValueType = "string" | "number" | "boolean" | "null" | "object" | "array" | "json";
export type InputField = { name: string; type: InputValueType; required: boolean; description?: string; choices?: unknown[] };

/** Resolves local references only; complex or recursive schemas retain their full JSON editor. */
function resolve(value: unknown, root: unknown, seen = new Set<string>()): Record<string, unknown> | null {
  // Boolean and malformed schemas do not declare a predictable field shape.
  if (!value || typeof value !== "object" || Array.isArray(value)) return null;
  const schema = value as Record<string, unknown>;
  if (typeof schema.$ref !== "string") return schema;
  const tokens = schemaPointerTokens(schema.$ref);
  // Cycles and external references never trigger network reads or unbounded rendering.
  if (!tokens || seen.has(schema.$ref) || seen.size >= 32) return null;
  const target = resolve(resolveSchemaPointer(root, tokens).value, root, new Set([...seen, schema.$ref]));
  // Sibling constraints remain intact, including descriptions attached to a reference.
  return target ? { ...target, ...schema, $ref: undefined } : null;
}

/** Extracts declared fields without inventing required values or choosing an ambiguous union branch. */
export function unifiedInputFields(schema: unknown): InputField[] | null {
  const root = resolve(schema, schema);
  // Conditional/composed roots cannot safely expose one fixed set of fields.
  if (!root || root.oneOf || root.anyOf || root.allOf || root.if || (root.type !== "object" && !root.properties)) return null;
  const properties = root.properties ?? {};
  if (!properties || typeof properties !== "object" || Array.isArray(properties) || Object.keys(properties).length > 100) return null;
  const required = new Set(Array.isArray(root.required) ? root.required : []);
  return Object.entries(properties).map(([name, property]) => {
    const field = resolve(property, schema);
    const declared = field?.type;
    // Integers use numeric input; the full contract still enforces integral values and other constraints.
    const type = declared === "integer" ? "number" : declared;
    const simple = typeof type === "string" && ["string", "number", "boolean", "null", "object", "array"].includes(type);
    // Unions retain explicit JSON values, rather than silently selecting a preferred type.
    const editorType = simple && !field?.oneOf && !field?.anyOf && !field?.allOf ? type as InputValueType : "json";
    return { name, type: editorType, required: required.has(name), description: typeof field?.description === "string" ? field.description : undefined, choices: Array.isArray(field?.enum) ? field.enum : undefined };
  });
}
