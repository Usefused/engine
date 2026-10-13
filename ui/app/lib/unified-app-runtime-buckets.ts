export type RuntimeBucket = { id: string; name: string };

/** Validates a finite authored scope before YAML or form edits enter a deployment plan. */
export function runtimeBucketNames(value: unknown): string[] {
  // Omission is the backwards-compatible, disabled default.
  if (value === undefined) return [];
  if (!Array.isArray(value) || value.length > 32 || value.some((name) => typeof name !== "string" || !name || name.trim() !== name || name === "*") || new Set(value).size !== value.length) throw new Error("runtime_buckets must contain at most 32 distinct bucket names without wildcards.");
  return value as string[];
}

/** Keeps runtime routing outside authored input and omits the selector for static executions. */
export function unifiedInvocationBody(input: unknown, bucketID: string, buckets: RuntimeBucket[]) {
  // The displayed scope is a client-side guard; Engine independently rechecks live authority.
  if (bucketID && !buckets.some((bucket) => bucket.id === bucketID)) throw new Error("Choose a permitted runtime bucket.");
  return { operation: "execute", input, ...(bucketID ? { bucket_id: bucketID } : {}) };
}
