/** Keeps absent paths distinct from JSON null without exposing traversed values. */
export interface PointerResolution {
  value?: unknown;
  error?: string;
}

export const MAX_JSON_POINTER_BYTES = 2048;
const MAX_SCHEMA_PATH_DEPTH = 32;

/** Validates and resolves a bounded RFC 6901 JSON Pointer using own properties only. */
export function resolveJsonPointer(root: unknown, pointer: string): PointerResolution {
  // Bounds prevent adversarial pointer parsing from dominating a local discovery call.
  if (Buffer.byteLength(pointer, "utf8") > MAX_JSON_POINTER_BYTES) {
    return { error: "schemaPath exceeds the 2048-byte limit" };
  }
  // The empty pointer names the whole selected section by RFC 6901.
  if (pointer === "") {
    return { value: root };
  }
  // Non-empty pointers must be absolute to remain deterministic across callers.
  if (!pointer.startsWith("/")) {
    return { error: "schemaPath must be an RFC 6901 JSON Pointer" };
  }
  const encodedTokens = pointer.slice(1).split("/");
  // A depth bound keeps nested traversal predictable even for tiny malicious inputs.
  if (encodedTokens.length > MAX_SCHEMA_PATH_DEPTH) {
    return { error: "schemaPath exceeds the 32-segment limit" };
  }
  let current = root;
  for (const encoded of encodedTokens) {
    const token = decodePointerToken(encoded);
    // Malformed tilde escapes are rejected rather than interpreted inconsistently.
    if (token === undefined) {
      return { error: "schemaPath contains an invalid escape" };
    }
    const next = ownValue(current, token);
    // Missing paths are explicit so callers do not confuse absent data with JSON null.
    if (!next.found) {
      return { error: "schemaPath does not exist in the selected section" };
    }
    current = next.value;
  }
  return { value: current };
}

/** Decodes one JSON Pointer token only when every tilde starts a defined escape. */
function decodePointerToken(token: string): string | undefined {
  // RFC 6901 defines only ~0 and ~1, so any other escape is ambiguous.
  if (/~(?:[^01]|$)/.test(token)) {
    return undefined;
  }
  return token.replace(/~1/g, "/").replace(/~0/g, "~");
}

/** Reads an own object property or canonical array index without prototype traversal. */
function ownValue(value: unknown, token: string): { found: boolean; value?: unknown } {
  // Array traversal accepts canonical non-negative indices only.
  if (Array.isArray(value)) {
    // Reject aliases such as leading-zero indices so every accepted path has one interpretation.
    if (!/^(0|[1-9]\d*)$/.test(token)) {
      return { found: false };
    }
    const index = Number(token);
    return index < value.length ? { found: true, value: value[index] } : { found: false };
  }
  // Own-property checks allow legitimate schema keys without exposing prototypes.
  if (value !== null && typeof value === "object" && Object.prototype.hasOwnProperty.call(value, token)) {
    return { found: true, value: (value as Record<string, unknown>)[token] };
  }
  return { found: false };
}

/** Appends an escaped token to an existing RFC 6901 pointer. */
export function appendPointer(base: string, token: string): string {
  const escaped = token.replace(/~/g, "~0").replace(/\//g, "~1");
  return `${base}/${escaped}`;
}
