import { resolveSchemaPointer, schemaPointerTokens } from "./schema-reference.ts";

export type SchemaExample = { json: string; error?: undefined } | { json?: undefined; error: string };
type Schema = Record<string, unknown>;

/** Generates editable JSON values from a saved schema, without mixing schema keywords into the payload. */
export function schemaExample(schema: unknown): SchemaExample {
  try {
    return { json: JSON.stringify(exampleValue(schema, schema, new Set(), 0, { remaining: 1000 }), null, 2) };
  } catch {
    // Unsupported or recursive contracts remain inspectable instead of offering a misleading partial payload.
    return { error: "An example could not be generated for this schema. Use Fields or JSON Schema to inspect it." };
  }
}

/** Builds bounded placeholders, favoring explicit authored examples and fixed values over inferred types. */
function exampleValue(value: unknown, root: unknown, references: Set<string>, depth: number, budget: { remaining: number }): unknown {
  // A shared node budget also bounds broad schemas that stay below the depth limit.
  if (--budget.remaining < 0) throw new Error("Example too large");
  // Deep or impossible schemas must not hang the details page or produce an apparently complete example.
  if (depth > 12 || value === false || value === null || typeof value !== "object" || Array.isArray(value)) {
    // An unrestricted boolean schema has a usable JSON object placeholder.
    if (value === true) return {};
    throw new Error("Unsupported schema");
  }
  const schema = value as Schema;
  // const is authoritative even when the fixed value is false, zero, or null.
  if (Object.hasOwn(schema, "const")) return schema.const;
  // Provider-authored examples/defaults preserve domain-specific shapes without fabricating identifiers.
  if (Object.hasOwn(schema, "example")) return schema.example;
  if (Array.isArray(schema.examples) && schema.examples.length) return schema.examples[0];
  if (Object.hasOwn(schema, "default")) return schema.default;
  if (Array.isArray(schema.enum) && schema.enum.length) return schema.enum[0];
  // Local definitions are resolved against the authored input/output root; external references never cause a fetch.
  if (typeof schema.$ref === "string") {
    const tokens = schemaPointerTokens(schema.$ref);
    // A repeated reference on one branch signals recursion; siblings may safely reuse the same definition.
    if (!tokens || references.has(schema.$ref)) throw new Error("Unresolved reference");
    const target = resolveSchemaPointer(root, tokens);
    if (!target.found) throw new Error("Missing definition");
    return exampleValue(target.value, root, new Set([...references, schema.$ref]), depth + 1, budget);
  }
  const alternatives = schema.oneOf ?? schema.anyOf;
  // Prefer a concrete union member over null so the user sees fields they can actually fill in.
  if (Array.isArray(alternatives)) {
    const concrete = alternatives.filter((item) => item && typeof item === "object" && item.type !== "null");
    const choice = concrete[0] ?? alternatives[0];
    return exampleValue(choice, root, references, depth + 1, budget);
  }
  // Intersections are only combined when all branches describe object values; conflicting fields remain unsupported.
  if (Array.isArray(schema.allOf)) {
    const merged: Record<string, unknown> = {};
    for (const part of schema.allOf) {
      const item = exampleValue(part, root, references, depth + 1, budget);
      if (!item || typeof item !== "object" || Array.isArray(item)) throw new Error("Unsupported intersection");
      for (const [key, entry] of Object.entries(item)) {
        if (Object.hasOwn(merged, key) && JSON.stringify(merged[key]) !== JSON.stringify(entry)) throw new Error("Conflicting intersection");
        Object.defineProperty(merged, key, { value: entry, enumerable: true, configurable: true });
      }
    }
    return merged;
  }
  const type = Array.isArray(schema.type) ? schema.type.find((item) => item !== "null") ?? "null" : schema.type;
  // Object properties become payload keys, never a copied properties/required/schema envelope.
  if (type === "object" || schema.properties) {
    const properties = schema.properties;
    if (!properties || typeof properties !== "object" || Array.isArray(properties)) return {};
    const entries = Object.entries(properties);
    if (entries.length > 100) throw new Error("Example too large");
    return Object.fromEntries(entries.map(([key, child]) => [key, exampleValue(child, root, references, depth + 1, budget)]));
  }
  // One representative item makes nested arrays editable; explicit min/max bounds determine empty or repeated items.
  if (type === "array" || schema.items || schema.prefixItems) {
    const minimum = typeof schema.minItems === "number" ? schema.minItems : 0;
    const count = Math.max(minimum, schema.maxItems === 0 ? 0 : 1);
    if (count > 20 || !Number.isInteger(count)) throw new Error("Example too large");
    const tuple = Array.isArray(schema.prefixItems) ? schema.prefixItems : [];
    // Tuple declarations must respect the same example-size bound as ordinary arrays.
    if (tuple.length > 20) throw new Error("Example too large");
    return Array.from({ length: Math.max(count, tuple.length) }, (_, index) => exampleValue(tuple[index] ?? schema.items ?? {}, root, references, depth + 1, budget));
  }
  // Scalar placeholders stay JSON values rather than type-descriptor objects.
  if (type === "boolean") return false;
  if (type === "null") return null;
  if (type === "integer" || type === "number") {
    const step = typeof schema.multipleOf === "number" && schema.multipleOf > 0 ? schema.multipleOf : 1;
    const minimum = typeof schema.minimum === "number" ? schema.minimum : typeof schema.exclusiveMinimum === "number" ? schema.exclusiveMinimum + step : 0;
    let number = Math.ceil(minimum / step) * step;
    // Negative-only ranges should not receive the usual zero placeholder.
    if (typeof schema.maximum === "number" && number > schema.maximum) number = Math.floor(schema.maximum / step) * step;
    if (typeof schema.exclusiveMaximum === "number" && number >= schema.exclusiveMaximum) number = Math.ceil(schema.exclusiveMaximum / step) * step - step;
    return number;
  }
  if (type === "string") {
    const formats: Record<string, string> = { email: "customer@example.com", uri: "https://example.com", url: "https://example.com", uuid: "00000000-0000-4000-8000-000000000000", date: "2026-01-01", "date-time": "2026-01-01T00:00:00Z" };
    const placeholder = formats[String(schema.format)] ?? "string";
    const minimum = typeof schema.minLength === "number" ? schema.minLength : 0;
    // Bound generated text independently of untrusted schema lengths.
    if (minimum > 1000) throw new Error("Example too large");
    const text = placeholder.padEnd(minimum, "x");
    return typeof schema.maxLength === "number" ? text.slice(0, schema.maxLength) : text;
  }
  // An empty schema permits arbitrary JSON; unfamiliar typed constructs should not silently turn into objects.
  if (Object.keys(schema).every((key) => ["title", "description", "$schema", "$defs"].includes(key))) return {};
  throw new Error("Unsupported schema");
}
