import { randomUUID } from "node:crypto";
import { EXECUTE_RESULT_OUTPUT_POLICY, JsonOutputPolicy, SerializedJsonOutput, serializeBoundedJson } from "./outputLimits.js";
import { previewResult } from "./resultPreview.js";
import { discoverCollections, CollectionDiscovery } from "./resultCollections.js";
import { executeOutputBudget, EXECUTE_INLINE_BYTES, EXECUTE_VISIBLE_OUTPUT_POLICY } from "./resultBudget.js";
import { inspectionScript, pageScript, recoveryForError, retainedContinuation, SESSION_RESULT_TTL_MS } from "./sessionContract.js";
import { pageResultCanProgress } from "./resultPaging.js";
export { EXECUTE_INLINE_BYTES, EXECUTE_VISIBLE_OUTPUT_POLICY } from "./resultBudget.js";

/** Errors cannot be navigated like successful data and must remain small on every client. */
export const EXECUTE_ERROR_OUTPUT_POLICY: JsonOutputPolicy = Object.freeze({
  maxBytes: 8 * 1024,
  limitCode: "MCP_EXECUTE_ERROR_OUTPUT_LIMIT_EXCEEDED",
  subject: "execute error",
});
export const RETAINED_RESULT_MAX_BYTES = 4 * 1024 * 1024;
export const RETAINED_RESULT_MAX_ENTRIES = 16;
export const RETAINED_RESULT_TTL_MS = SESSION_RESULT_TTL_MS;
const RESULT_REF_PREFIX = "fused-result:";

/** Keeps an immutable JSON snapshot, not script-realm objects or executable hooks. */
interface RetainedResult {
  text: string;
  bytes: number;
  expiresAt: number;
}

/** Separates envelope admission from session-memory mutation. */
interface RetainedCandidate {
  reference: string;
  entry: RetainedResult;
  existing: boolean;
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
      return { ...outputWithRecovery(output), delivery: "error" };
    }
    // Ordinary small results preserve their exact public return shape.
    if (Buffer.byteLength(output.text, "utf8") <= maxBytes) {
      return { ...output, delivery: "inline" };
    }
    const candidate = this.candidate(output.text);
    const snapshot = JSON.parse(candidate.entry.text);
    const visible = storedResultOutput(snapshot, candidate, maxBytes);
    // An undeliverable reference must never reserve session memory the model cannot recover.
    if (visible.isError) return { ...outputWithRecovery(visible), delivery: "error" };
    // Existing immutable snapshots already own their admitted byte charge and expiry.
    if (!candidate.existing) this.commit(candidate);
    return { ...visible, delivery: "stored" };
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

  /** Finds an immutable duplicate or prepares one uncommitted snapshot candidate. */
  private candidate(text: string): RetainedCandidate {
    this.expire();
    // Returning an existing large result again must not evict its own original reference.
    for (const [reference, entry] of this.entries) {
      // Exact JSON equality is safe here because the bounded snapshot has no executable hooks.
      if (entry.text === text) return { reference, entry, existing: true };
    }
    const entry = { text, bytes: Buffer.byteLength(text, "utf8"), expiresAt: Date.now() + RETAINED_RESULT_TTL_MS };
    return { reference: `${RESULT_REF_PREFIX}${randomUUID()}`, entry, existing: false };
  }

  /** Commits only a candidate whose model-visible recovery envelope already passed admission. */
  private commit(candidate: RetainedCandidate): void {
    // Each input already passed the per-result ceiling, so bounded FIFO eviction always makes room.
    while (this.entries.size >= RETAINED_RESULT_MAX_ENTRIES || this.bytes + candidate.entry.bytes > RETAINED_RESULT_MAX_BYTES) {
      this.remove(this.entries.keys().next().value!);
    }
    this.entries.set(candidate.reference, candidate.entry);
    this.bytes += candidate.entry.bytes;
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

/** Builds a stored envelope and falls back to compact inspection before rejecting its reference. */
function storedResultOutput(snapshot: unknown, candidate: RetainedCandidate, maxBytes: number): SerializedJsonOutput {
  const discovery = discoverCollections(snapshot);
  const primaryScript = retainedReadScript(snapshot, candidate.reference, discovery, maxBytes);
  let output = serializeStoredEnvelope(snapshot, candidate, discovery, primaryScript, maxBytes);
  // A path that fits a page can still make the surrounding stored envelope too large at the minimum budget.
  if (output.isError) {
    const fallback = inspectionScript(candidate.reference, snapshot, maxBytes);
    output = serializeStoredEnvelope(snapshot, candidate, discovery, fallback, maxBytes);
  }
  return output;
}

/** Chooses a whole-row or single-field page only when every exact continuation can advance. */
function retainedReadScript(snapshot: unknown, reference: string, discovery: CollectionDiscovery, maxBytes: number): string {
  for (const collection of discovery.collections) {
    const projections: Array<string[] | undefined> = [undefined, ...collection.fields.map((field) => [field])];
    for (const fields of projections) {
      const options = { path: collection.path, fields, offset: 0 };
      // Proven all-row progress prevents the supplied continuation from failing on a later oversized row.
      if (pageResultCanProgress(snapshot, reference, options, maxBytes)) {
        return pageScript(reference, collection.path, fields, 0);
      }
    }
  }
  return inspectionScript(reference, snapshot, maxBytes);
}

/** Serializes one recovery plan after trimming only optional structural metadata. */
function serializeStoredEnvelope(snapshot: unknown, candidate: RetainedCandidate, discovery: CollectionDiscovery, script: string, maxBytes: number): SerializedJsonOutput {
  const envelope = {
    code: "MCP_RESULT_STORED",
    complete: false,
    result_ref: candidate.reference,
    result_bytes: candidate.entry.bytes,
    expires_at: new Date(candidate.entry.expiresAt).toISOString(),
    output_budget_bytes: maxBytes,
    preview: previewResult(snapshot),
    ...structuredClone(discovery),
    ...retainedContinuation(script, maxBytes),
  };
  fitNavigation(envelope, maxBytes);
  return serializeBoundedJson(envelope, { ...EXECUTE_VISIBLE_OUTPUT_POLICY, maxBytes });
}

/** Adds the closed projection recovery contract to trusted serializer failures without echoing rejected data. */
function outputWithRecovery(output: SerializedJsonOutput): SerializedJsonOutput {
  const failure = JSON.parse(output.text) as Record<string, unknown>;
  const code = typeof failure.code === "string" ? failure.code : "MCP_OUTPUT_SERIALIZATION_FAILED";
  return { text: JSON.stringify({ ...failure, ...recoveryForError(`${code}:`) }), isError: true };
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
