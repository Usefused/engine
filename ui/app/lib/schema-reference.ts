export type ResolvedSchemaReference = { schema: unknown; document: unknown };

export type SchemaReferencePlan =
  | { kind: "local"; schema: unknown; document: unknown }
  | { kind: "component"; name: string; suffix: string[] }
  | { kind: "unavailable"; reason: string };

type PointerResult = { found: boolean; value?: unknown };

// schemaPointerTokens accepts only same-document JSON Pointer fragments, never
// arbitrary URLs, and decodes URI escaping before RFC 6901 token escaping.
export function schemaPointerTokens(reference: string): string[] | null {
  // A root cycle is a legitimate local reference rather than a component name.
  if (reference === "#") return [];
  // External references and named anchors require separate supported semantics.
  if (!reference.startsWith("#/")) return null;
  let pointer: string;
  try {
    pointer = decodeURIComponent(reference.slice(2));
  } catch {
    // Malformed percent escapes cannot alias a differently named saved schema.
    return null;
  }
  const tokens = pointer.split("/");
  // Invalid tilde escapes must not be silently normalized into another identity.
  if (tokens.some((token) => /~(?:[^01]|$)/.test(token))) return null;
  return tokens.map((token) => token.replace(/~1/g, "/").replace(/~0/g, "~"));
}

// resolveSchemaPointer traverses own JSON members only and preserves false,
// null, and array positions instead of mistaking them for absent definitions.
export function resolveSchemaPointer(document: unknown, tokens: string[]): PointerResult {
  let value = document;
  for (const token of tokens) {
    // Prototype properties are not JSON data and must never satisfy a reference.
    if (value === null || typeof value !== "object" || !Object.hasOwn(value, token)) return { found: false };
    // Arrays use canonical indexes rather than arbitrary JS object properties.
    if (Array.isArray(value) && !/^(0|[1-9][0-9]*)$/.test(token)) return { found: false };
    value = (value as Record<string, unknown>)[token];
  }
  return { found: true, value };
}

// schemaComponentReference extracts the saved definition's complete decoded
// name while retaining a subschema pointer for inspection after the fetch.
function schemaComponentReference(tokens: string[]): { name: string; suffix: string[]; owner: string[] } | null {
  // Shared canonical definitions and original OpenAPI components have exact namespaces.
  const shared = tokens[0] === "$defs";
  // No other local pointer namespace may trigger a Registry component request.
  if (!shared && (tokens[0] !== "components" || tokens[1] !== "schemas")) return null;
  const index = shared ? 1 : 2;
  const name = tokens[index];
  // Empty component identities are invalid even though empty JSON property names are legal.
  if (!name) return null;
  return { name, suffix: tokens.slice(index + 1), owner: tokens.slice(0, index + 1) };
}

// planSchemaReference gives the current raw document precedence over shared
// definitions and never turns a missing local subschema into a remote fallback.
export function planSchemaReference(document: unknown, reference: string): SchemaReferencePlan {
  const tokens = schemaPointerTokens(reference);
  // Unsupported reference syntax remains visible but cannot initiate a fetch.
  if (tokens === null) return { kind: "unavailable", reason: "Only local schema references are supported." };
  const local = resolveSchemaPointer(document, tokens);
  // Boolean schemas are real found values, including the restrictive false schema.
  if (local.found) return { kind: "local", schema: local.value, document };
  const component = schemaComponentReference(tokens);
  // Ordinary local pointers cannot be guessed into a provider component name.
  if (!component) return { kind: "unavailable", reason: "Local schema reference was not found." };
  // A local definition owns its complete subtree, even when the requested child is absent.
  if (resolveSchemaPointer(document, component.owner).found) return { kind: "unavailable", reason: "Local schema reference was not found." };
  return { kind: "component", name: component.name, suffix: component.suffix };
}

// schemaReferenceLabel displays the exact decoded definition name rather than
// the final path segment, which may actually name a property or array index.
export function schemaReferenceLabel(reference: string): string {
  const tokens = schemaPointerTokens(reference);
  // Unsupported references retain their literal text for honest diagnostics.
  if (tokens === null) return reference;
  return schemaComponentReference(tokens)?.name ?? tokens.at(-1) ?? reference;
}

// resolvePlannedSchemaReference preserves the fetched definition as the local
// root so a nested reference does not accidentally resolve against its caller.
export async function resolvePlannedSchemaReference(plan: SchemaReferencePlan, fetchComponent: (name: string) => Promise<unknown>): Promise<ResolvedSchemaReference> {
  // Local references need no request and remain usable when remote expansion is disabled.
  if (plan.kind === "local") return { schema: plan.schema, document: plan.document };
  // Invalid reference syntax must fail before a network callback is invoked.
  if (plan.kind === "unavailable") throw new Error(plan.reason);
  const document = await fetchComponent(plan.name);
  const result = resolveSchemaPointer(document, plan.suffix);
  // A missing requested subschema cannot be shown as its containing definition instead.
  if (!result.found) throw new Error("Schema reference was not found in the selected definition.");
  return { schema: result.value, document };
}
