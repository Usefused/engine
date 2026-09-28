type Schema = Record<string, unknown>;

interface SchemaContext {
  aliases: Map<string, string>;
  rootName: string;
}

// Keep generated type identifiers independent of authored definition names.
function definitionAliases(schema: Schema, prefix: string): Map<string, string> {
  const definitions = record(schema.$defs);
  return new Map(Object.keys(definitions).sort().map((name, index) => [name, `${prefix}Def${index}`]));
}

// Read JSON Schema maps without treating arrays as named definitions.
function record(value: unknown): Schema {
  // Unsupported schema branches stay typed as unknown instead of becoming unsafe any.
  if (!value || typeof value !== "object" || Array.isArray(value)) return {};
  return value as Schema;
}

// Resolve only local Zod JSON Schema definitions embedded in this manifest.
function referenceType(reference: string, context: SchemaContext): string {
  // Zod uses the document root as the recursive anchor for self-references.
  if (reference === "#") return context.rootName;
  // External references cannot be compiled without a separate admitted schema.
  if (!reference.startsWith("#/$defs/")) return "unknown";
  const name = reference.slice("#/$defs/".length).replace(/~1/g, "/").replace(/~0/g, "~");
  return context.aliases.get(name) ?? "unknown";
}

// Preserve JSON Schema scalar literals as exact TypeScript literal types.
function literalType(value: unknown): string {
  // Composite enum values have no stable scalar search or transport type here.
  if (value === null || typeof value === "string" || typeof value === "number" || typeof value === "boolean") {
    return JSON.stringify(value);
  }
  return "unknown";
}

// Render one JSON Schema combinator without duplicating the object/array renderer.
function variantType(value: unknown, context: SchemaContext, separator: string): string {
  // Empty or malformed variants cannot assert a narrower public type.
  if (!Array.isArray(value) || value.length === 0) return "unknown";
  return value.map((part) => renderSchemaType(record(part), context)).join(separator);
}

// Preserve required fields and additional-property policy from Zod's public schema.
function objectType(schema: Schema, context: SchemaContext): string {
  const properties = record(schema.properties);
  const required = new Set(Array.isArray(schema.required) ? schema.required : []);
  const fields = Object.keys(properties).sort().map((name) => {
    // Optional fields remain optional in generated callers.
    const suffix = required.has(name) ? "" : "?";
    return `${JSON.stringify(name)}${suffix}: ${renderSchemaType(record(properties[name]), context)};`;
  });
  // A dictionary schema admits values beyond named properties.
  if (schema.additionalProperties === true) fields.push("[key: string]: unknown;");
  // Typed records can retain their value type when no named field conflicts with it.
  if (record(schema.additionalProperties) === schema.additionalProperties && fields.length === 0) {
    return `Record<string, ${renderSchemaType(record(schema.additionalProperties), context)}>`;
  }
  // Mixed named and additional fields use unknown because TypeScript index signatures include named keys.
  if (record(schema.additionalProperties) === schema.additionalProperties) fields.push("[key: string]: unknown;");
  return `{ ${fields.join(" ")} }`;
}

// Map JSON Schema primitives and containers to their TypeScript equivalents.
function typeByKind(schema: Schema, context: SchemaContext): string {
  // A union of JSON primitive kinds remains a union in generated source.
  if (Array.isArray(schema.type)) {
    return schema.type.map((kind) => typeByKind({ ...schema, type: kind }, context)).join(" | ");
  }
  // Arrays retain the item's declared shape.
  if (schema.type === "array") return `Array<${renderSchemaType(record(schema.items), context)}>`;
  // Object field and dictionary policies are handled in one place.
  if (schema.type === "object") return objectType(schema, context);
  const primitive: Record<string, string> = { string: "string", number: "number", integer: "number", boolean: "boolean", null: "null" };
  return primitive[String(schema.type)] ?? "unknown";
}

// Render supported Zod-derived JSON Schema without inventing unsupported constraints.
export function renderSchemaType(schema: Schema, context: SchemaContext): string {
  // Local definitions preserve recursive types through named aliases.
  if (typeof schema.$ref === "string") return referenceType(schema.$ref, context);
  // A const must remain narrower than its underlying primitive type.
  if (Object.prototype.hasOwnProperty.call(schema, "const")) return literalType(schema.const);
  // Enum alternatives become a finite union of literal values.
  if (Array.isArray(schema.enum)) return schema.enum.map(literalType).join(" | ") || "unknown";
  // Alternatives and intersections preserve public shape composition.
  if (Array.isArray(schema.anyOf)) return variantType(schema.anyOf, context, " | ");
  if (Array.isArray(schema.oneOf)) return variantType(schema.oneOf, context, " | ");
  if (Array.isArray(schema.allOf)) return variantType(schema.allOf, context, " & ");
  return typeByKind(schema, context);
}

// Emit one root type plus all local aliases needed by recursive references.
export function renderPublicType(schema: Schema, name: string): string {
  const aliases = definitionAliases(schema, name);
  const context = { aliases, rootName: name };
  const definitions = record(schema.$defs);
  const declarations = [...aliases].map(([key, alias]) => `type ${alias} = ${renderSchemaType(record(definitions[key]), context)};`);
  declarations.push(`export type ${name} = ${renderSchemaType(schema, context)};`);
  return declarations.join("\n");
}
