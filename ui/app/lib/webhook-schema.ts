import type { JsonSchemaNode } from "~/components/SchemaViewer";

// webhookSchemaPending distinguishes an absent or generic body from a declared
// shape, without resolving references or claiming runtime validation results.
export function webhookSchemaPending(schema?: JsonSchemaNode | null): boolean {
  // Missing bodies still need a visible explanation rather than a blank panel.
  if (!schema) return true;
  // References and composed/array schemas are real contracts even without properties.
  const declaredShape = [schema.$ref, schema.items, schema.oneOf?.length, schema.anyOf?.length, schema.allOf?.length].some(Boolean);
  if (declaredShape) return false;
  // Scalar event bodies must not be mistaken for unobserved objects.
  if (["array", "string", "number", "integer", "boolean", "null"].includes(String(schema.type))) return false;
  return Object.keys(schema.properties ?? {}).length === 0;
}
