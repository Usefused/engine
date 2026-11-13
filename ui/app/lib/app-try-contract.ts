import { resolveSchemaPointer, schemaPointerTokens } from "./schema-reference.ts";
import type { RuntimeBucket } from "./unified-app-runtime-buckets.ts";

export type TryOperation = { name: string; input: Record<string, unknown> | boolean; buckets: RuntimeBucket[]; selector: boolean };
export type TryRequirement = { service_id: string; service: string; bucket_id: string; bucket_name: string; auth_name: string; auth_type: string; credentials: "ready" | "missing" | "unknown" | "managed"; connection: "not_required" | "user_required" | "ready" | "missing" | "reconnect_required" | "unknown" };
export type TryReadiness = { requirements: TryRequirement[]; runtime_buckets: RuntimeBucket[]; note?: string };

/** Reads own JSON properties without permitting prototype traversal. */
function at(value: unknown, ...path: string[]): unknown {
  return resolveSchemaPointer(value, path).value;
}

/** Resolves only local envelope references, with a finite cycle boundary. */
function resolve(document: unknown, value: unknown): unknown {
  const seen = new Set<string>();
  while (typeof at(value, "$ref") === "string") {
    const ref = at(value, "$ref") as string;
    const path = schemaPointerTokens(ref);
    // Remote, broken and recursive envelopes cannot become executable forms.
    if (!path || seen.has(ref) || seen.size >= 32) throw new Error("The operation contract could not be resolved.");
    seen.add(ref);
    value = resolveSchemaPointer(document, path).value;
  }
  return value;
}

/** Keeps component references rooted in the exported document without rewriting authored local definitions. */
function inputSchema(document: unknown, input: unknown): TryOperation["input"] {
  // Boolean contracts are valid JSON Schema and must retain their exact meaning.
  if (typeof input === "boolean") return input;
  // Absent or malformed input contracts must not become permissive empty schemas.
  if (!input || typeof input !== "object" || Array.isArray(input)) throw new Error("The operation input schema is unavailable.");
  return { ...input, components: at(document, "components") };
}

/** Derives the operation picker and forms from the exact immutable execution contract. */
export function appTryOperations(document: unknown, appID: string, unified: boolean): TryOperation[] {
  // Refuse stale or foreign schemas before offering an executable payload.
  if (at(document, "x-fused-app-id") !== appID) throw new Error("The app version changed. Reopen Try to load its current contract.");
  const request = at(document, "paths", "/v1/apps/{app_id}/executions", "post", "requestBody", "content", "application/json", "schema");
  const variants = at(request, "oneOf");
  // The Engine export always identifies callable requests through explicit variants.
  if (!Array.isArray(variants) || variants.length === 0) throw new Error("No callable operations are available.");
  const operations = variants.map(/** Projects only runtime envelope controls, preserving the input contract. */ (variant): TryOperation => {
    const branch = resolve(document, variant);
    const name = at(branch, "properties", "operation", "const");
    // Each form must identify an exact operation, never infer one from payload shape.
    if (typeof name !== "string" || !name) throw new Error("The operation identity is unavailable.");
    const ids = at(branch, "properties", "bucket_id", "enum");
    // Only the bounded deployment allowlist can populate alternate bucket choices.
    if (ids !== undefined && (!Array.isArray(ids) || ids.length > 32 || ids.some(/** Prevents malformed identities entering routing controls. */ id => typeof id !== "string"))) throw new Error("The permitted bucket scope is invalid.");
    const buckets = ((ids ?? []) as string[]).map(/** Associates an immutable identity with its optional display label. */ id => {
      const label = at(branch, "properties", "bucket_id", "x-fused-bucket-names", id);
      // The opaque ID remains authoritative when a display label is absent.
      return { id, name: typeof label === "string" ? label : id };
    });
    return { name, input: inputSchema(document, at(branch, "properties", "input")), buckets, selector: at(branch, "properties", "selector") !== undefined };
  });
  // Unified Apps expose their authored input instead of their internal provider calls.
  const selected = unified ? operations.filter(/** Hides provider dependencies behind the authored entry point. */ operation => operation.name === "execute") : operations;
  // Empty or duplicate operation identities cannot safely select a request form.
  if (!selected.length || new Set(selected.map(/** Checks runtime identity uniqueness. */ operation => operation.name)).size !== selected.length) throw new Error("The operation contract is missing or ambiguous.");
  return selected;
}

/** Builds one runtime request while keeping routing controls outside the authored input. */
export function appTryRequest(operation: TryOperation, input: unknown, bucketID: string, versionID: string, unified: boolean, selector: Record<string, string>) {
  // A stale UI selection must never broaden the deployed bucket scope.
  if (bucketID && !operation.buckets.some(/** Matches only an advertised immutable bucket identity. */ bucket => bucket.id === bucketID)) throw new Error("Choose a permitted bucket.");
  // Omitted routing controls preserve configured defaults; authored calls pin their displayed version.
  return { operation: operation.name, input, ...(bucketID ? { bucket_id: bucketID } : {}), ...(unified ? { expected_app_id: versionID } : {}), ...(operation.selector && Object.values(selector).some(Boolean) ? { selector: Object.fromEntries(Object.entries(selector).filter(/** Blank optional selectors must not override configured routing. */ ([, value]) => value !== "")) } : {}) };
}
