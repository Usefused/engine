import { randomUUID } from "node:crypto";
import { EXECUTE_RESULT_OUTPUT_POLICY, JsonOutputPolicy, SerializedJsonOutput, serializeBoundedJson } from "./outputLimits.js";
import { previewResult } from "./resultPreview.js";
import { discoverCollections, CollectionDiscovery } from "./resultCollections.js";
import { executeOutputBudget, EXECUTE_INLINE_BYTES, EXECUTE_VISIBLE_OUTPUT_POLICY } from "./resultBudget.js";
export { EXECUTE_INLINE_BYTES, EXECUTE_VISIBLE_OUTPUT_POLICY } from "./resultBudget.js";

/** Errors cannot be navigated like successful data and must remain small on every client. */
export const EXECUTE_ERROR_OUTPUT_POLICY: JsonOutputPolicy = Object.freeze({
  maxBytes: 8 * 1024,
  limitCode: "MCP_EXECUTE_ERROR_OUTPUT_LIMIT_EXCEEDED",
  subject: "execute error",
});
export const RETAINED_RESULT_MAX_BYTES = 4 * 1024 * 1024;
export const RETAINED_RESULT_MAX_ENTRIES = 16;
export const RETAINED_RESULT_TTL_MS = 5 * 60 * 1000;
const RESULT_REF_PREFIX = "fused-result:";

/** Keeps an immutable JSON snapshot, not script-realm objects or executable hooks. */
interface RetainedResult {
  text: string;
  bytes: number;
  expiresAt: number;
}

/** Carries only closed delivery metadata across the trusted handler boundary. */
export interface DeliveredResult extends SerializedJsonOutput {
  delivery: "inline" | "stored" | "error";
}

/** Recognizes a reserved session namespace without accepting arbitrary script objects. */
export function isResultReference(key: unknown): key is string {
  return typeof key === "string" && key.startsWith(RESULT_REF_PREFIX);
}

/** Owns bounded, absolute-expiry snapshots for one MCP process/session only. */
export class RetainedResults {
  private readonly entries = new Map<string, RetainedResult>();
  private bytes = 0;

  /** Uses one validated byte budget for inline results and navigation without changing full-value admission. */
  deliver(value: unknown, maxBytes = EXECUTE_INLINE_BYTES): DeliveredResult {
    executeOutputBudget(maxBytes);
    this.expire();
    const output = serializeBoundedJson(value, EXECUTE_RESULT_OUTPUT_POLICY);
    // Invalid or over-one-MiB values never enter retention, and never trigger provider re-execution.
    if (output.isError) {
      return { ...output, delivery: "error" };
    }
    // Ordinary small results preserve their exact public return shape.
    if (Buffer.byteLength(output.text, "utf8") <= maxBytes) {
      return { ...output, delivery: "inline" };
    }
    const [reference, entry] = this.retain(output.text);
    const snapshot = JSON.parse(entry.text);
    const envelope = {
      code: "MCP_RESULT_STORED",
      complete: false,
      result_ref: reference,
      result_bytes: entry.bytes,
      expires_at: new Date(entry.expiresAt).toISOString(),
      output_budget_bytes: maxBytes,
      preview: previewResult(snapshot),
      ...discoverCollections(snapshot),
      next: "Use execute with session.page(result_ref, {path, fields, offset}) for whole-row pages, or session.get(result_ref) for other projections. Metadata may be incomplete. Never repeat the provider call to retrieve this result.",
    };
    fitNavigation(envelope, maxBytes);
    const visible = serializeBoundedJson(envelope, { ...EXECUTE_VISIBLE_OUTPUT_POLICY, maxBytes });
    // A future preview change cannot silently send an oversized navigation response.
    return { ...visible, delivery: visible.isError ? "error" : "stored" };
  }

  /** Returns a fresh JSON snapshot so callers cannot mutate retained results or extend their lifetime. */
  get(reference: string): unknown {
    this.expire();
    const entry = this.entries.get(reference);
    // Expiry, eviction, and cross-session misses share one explicit non-retryable lookup failure.
    if (!entry) {
      throw new Error("MCP_RESULT_UNAVAILABLE: Result expired, was evicted, or belongs to another session. It cannot be recovered here; do not automatically repeat the operation.");
    }
    return JSON.parse(entry.text);
  }

  /** Deduplicates bounded snapshots and evicts oldest entries before reserving more session memory. */
  private retain(text: string): [string, RetainedResult] {
    this.expire();
    // Returning an existing large result again must not evict its own original reference.
    for (const pair of this.entries) {
      // Exact JSON equality is safe here because the bounded snapshot has no executable hooks.
      if (pair[1].text === text) {
        return pair;
      }
    }
    const entry = { text, bytes: Buffer.byteLength(text, "utf8"), expiresAt: Date.now() + RETAINED_RESULT_TTL_MS };
    // Each input already passed the per-result ceiling, so bounded FIFO eviction always makes room.
    while (this.entries.size >= RETAINED_RESULT_MAX_ENTRIES || this.bytes + entry.bytes > RETAINED_RESULT_MAX_BYTES) {
      this.remove(this.entries.keys().next().value!);
    }
    const reference = `${RESULT_REF_PREFIX}${randomUUID()}`;
    this.entries.set(reference, entry);
    this.bytes += entry.bytes;
    return [reference, entry];
  }

  /** Enforces absolute expiry even while client activity keeps the enclosing session alive. */
  private expire(): void {
    const now = Date.now();
    for (const [reference, entry] of this.entries) {
      // Reads never refresh expiry, preventing indefinite retention through repeated retrieval.
      if (entry.expiresAt <= now) {
        this.remove(reference);
      }
    }
  }

  /** Releases the exact admitted byte charge together with its snapshot. */
  private remove(reference: string): void {
    const entry = this.entries.get(reference)!;
    this.entries.delete(reference);
    this.bytes -= entry.bytes;
  }
}

/** Reduces only navigation metadata, never stored data, to honor even a client's smallest byte budget. */
function fitNavigation(envelope: CollectionDiscovery & { preview: ReturnType<typeof previewResult> }, maxBytes: number): void {
  // Discard sampled shape detail first, preserving collection paths and field names whenever possible.
  if (Buffer.byteLength(JSON.stringify(envelope)) > maxBytes) {
    envelope.preview = { type: envelope.preview.type, count: envelope.preview.count, complete: false };
  }
  // Every iteration removes a bounded field or collection, so fitting cannot loop without progress.
  while (Buffer.byteLength(JSON.stringify(envelope)) > maxBytes && envelope.collections.length > 0) {
    const last = envelope.collections[envelope.collections.length - 1];
    // Keep an exact path/count even when its full field inventory cannot fit.
    if (last.fields.length > 0) {
      last.fields.pop();
      last.fields_complete = false;
    } else {
      envelope.collections.pop();
      envelope.collections_complete = false;
    }
  }
}
