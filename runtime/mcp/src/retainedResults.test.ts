import { afterEach, expect, it, vi } from "vitest";
import { EXECUTE_RESULT_OUTPUT_POLICY } from "./outputLimits.js";
import { EXECUTE_INLINE_BYTES, EXECUTE_VISIBLE_OUTPUT_POLICY, RETAINED_RESULT_MAX_ENTRIES, RETAINED_RESULT_TTL_MS, RetainedResults } from "./retainedResults.js";
import { runExecute, SessionState } from "./sandbox.js";

/** Gives assertions the runtime-produced envelope without printing stored data. */
function store(results: RetainedResults, value: unknown) {
  const output = results.deliver(value);
  expect(output.delivery).toBe("stored");
  expect(Buffer.byteLength(output.text)).toBeLessThan(EXECUTE_VISIBLE_OUTPUT_POLICY.maxBytes);
  return JSON.parse(output.text);
}

// Fake time must not leak into unrelated sandbox deadline tests.
afterEach(() => vi.useRealTimers());

// UTF-8 admission must remain exact even when character count understates byte size.
it("preserves inline JSON at the byte boundary and retains the next byte", () => {
  const results = new RetainedResults();
  const inline = "x".repeat(EXECUTE_INLINE_BYTES - 2);
  expect(results.deliver(inline).text).toBe(JSON.stringify(inline));
  expect(results.deliver(inline).delivery).toBe("inline");
  expect(store(results, inline + "x").result_bytes).toBe(EXECUTE_INLINE_BYTES + 1);
  expect(results.deliver("😀".repeat(EXECUTE_INLINE_BYTES / 4)).delivery).toBe("stored");
});

// Structural previews must not reveal any personal scalar value, even in a representative item.
it("returns incomplete fields and collection counts without scalar samples", () => {
  const results = new RetainedResults();
  const value = { items: Array(200).fill({ subject: "PRIVATE_SENTINEL", text: "x".repeat(100) }), total: 200 };
  const envelope = store(results, value);
  expect(envelope.complete).toBe(false);
  expect(envelope.preview.fields[0]).toMatchObject({ name: "items", preview: { type: "array", count: 200, complete: false } });
  expect(JSON.stringify(envelope)).not.toContain("PRIVATE_SENTINEL");
  expect(envelope).toMatchObject({ recovery_action: "continue_stored_result", execute_request: "use_next_request", provider_execution: "complete", automatic_replay: false });
  expect(envelope.session).toEqual({ scope: "current_mcp_connection", same_session_required: true });
  expect(envelope.next_request).toMatchObject({ tool: "execute", arguments: { outputBudgetBytes: EXECUTE_INLINE_BYTES } });
  expect(envelope.next_request.arguments.script).toBe(`return session.page(${JSON.stringify(envelope.result_ref)}, {path:"/items",offset:0});`);
  expect(envelope.next_request.arguments.script).not.toContain("call(");
  expect(results.get(envelope.result_ref)).toEqual(value);
});

// Bounded snapshots are independent of caller mutations and repeated whole-result returns.
it("deduplicates snapshots and rejects references from another session", () => {
  const results = new RetainedResults();
  const value = { body: "x".repeat(120000) };
  const envelope = store(results, value);
  const retrieved = results.get(envelope.result_ref) as typeof value;
  retrieved.body = "changed";
  expect(results.get(envelope.result_ref)).toEqual(value);
  expect(store(results, value).result_ref).toBe(envelope.result_ref);
  expect(() => new RetainedResults().get(envelope.result_ref)).toThrow("MCP_RESULT_UNAVAILABLE");
});

// Absolute TTL prevents reads and deduplication from keeping private snapshots indefinitely.
it("expires results after five minutes even when they are read", () => {
  vi.useFakeTimers();
  const results = new RetainedResults();
  const value = "x".repeat(EXECUTE_INLINE_BYTES);
  const envelope = store(results, value);
  vi.advanceTimersByTime(RETAINED_RESULT_TTL_MS - 1);
  expect(results.get(envelope.result_ref)).toBe(value);
  expect(store(results, value).expires_at).toBe(envelope.expires_at);
  vi.advanceTimersByTime(1);
  expect(() => results.get(envelope.result_ref)).toThrow("MCP_RESULT_UNAVAILABLE");
});

// Count and byte caps are independent so many small snapshots cannot evade bounded storage.
it("evicts the oldest result at the entry ceiling", () => {
  const results = new RetainedResults();
  const first = store(results, "a".repeat(EXECUTE_INLINE_BYTES));
  for (let index = 0; index < RETAINED_RESULT_MAX_ENTRIES; index++) {
    store(results, "x".repeat(EXECUTE_INLINE_BYTES) + index);
  }
  expect(() => results.get(first.result_ref)).toThrow("MCP_RESULT_UNAVAILABLE");
});

// Four near-one-MiB results fit, but a fifth requires FIFO eviction regardless of entry count.
it("enforces the aggregate byte ceiling and original per-result ceiling", () => {
  const results = new RetainedResults();
  const first = store(results, "a".repeat(EXECUTE_RESULT_OUTPUT_POLICY.maxBytes - 2));
  for (let index = 0; index < 4; index++) {
    store(results, "x".repeat(EXECUTE_RESULT_OUTPUT_POLICY.maxBytes - 3) + index);
  }
  expect(() => results.get(first.result_ref)).toThrow("MCP_RESULT_UNAVAILABLE");
  const rejected = results.deliver("x".repeat(EXECUTE_RESULT_OUTPUT_POLICY.maxBytes));
  expect(rejected.delivery).toBe("error");
  expect(JSON.parse(rejected.text).code).toBe("MCP_EXECUTE_RESULT_LIMIT_EXCEEDED");
  expect(rejected.text).not.toContain("result_ref");
});

// Hostile field names must not make the automatic navigation response larger than the budget.
it("bounds deep previews and omits oversized keys without corrupting stored data", () => {
  const results = new RetainedResults();
  const key = "k".repeat(9000);
  const value = { [key]: { nested: { body: "x".repeat(9000) } } };
  const envelope = store(results, value);
  expect(envelope.preview).toMatchObject({ count: 1, fields: [], complete: false });
  expect(results.get(envelope.result_ref)).toEqual(value);
});

// Minimum-budget navigation must retain an executable same-session request even when no row projection can fit.
it("preserves executable recovery for oversized rows at the minimum output budget", async () => {
  const session = new SessionState();
  const options = { sessionId: "synthetic", enginePort: "1" };
  const callImpl = vi.fn();
  const stored = await runExecute('return {rows:Array(100).fill({["k".repeat(128)]:"PRIVATE_SENTINEL".repeat(100)})}', options, session, { timeoutMs: 30_000, maxCalls: 10, outputBudgetBytes: 1024 }, callImpl);
  const envelope = JSON.parse(stored.text);
  expect(Buffer.byteLength(JSON.stringify(envelope))).toBeLessThanOrEqual(1024);
  expect(envelope.code).toBe("MCP_RESULT_STORED");
  expect(envelope).toMatchObject({ recovery_action: "continue_stored_result", execute_request: "use_next_request", provider_execution: "complete", automatic_replay: false });
  expect(envelope.session).toMatchObject({ same_session_required: true });
  expect(envelope.next_request.tool).toBe("execute");
  const next = await runExecute(envelope.next_request.arguments.script, options, session, { timeoutMs: 30_000, maxCalls: 10, outputBudgetBytes: 1024 }, callImpl);
  expect(next.isError).toBe(false);
  expect(JSON.parse(next.text)).toMatchObject({ recovery_action: "adjust_result_projection", execute_request: "adjust_projection", provider_execution: "complete", type: "object" });
  expect(callImpl).not.toHaveBeenCalled();
  expect(JSON.stringify(envelope)).not.toContain("PRIVATE_SENTINEL");
});

// Compact type-specific continuations keep legal root values recoverable at the public minimum budget.
it("delivers and executes minimum-budget continuations for root strings and objects", async () => {
  const options = { sessionId: "synthetic", enginePort: "1" };
  for (const script of ['return "x".repeat(20000)', 'return {body:"x".repeat(20000)}']) {
    const session = new SessionState();
    const stored = await runExecute(script, options, session, { timeoutMs: 30_000, maxCalls: 10, outputBudgetBytes: 1024 });
    const envelope = JSON.parse(stored.text);
    // Every admitted stored result must expose a usable reference rather than a visible-output error.
    expect(stored.delivery).toBe("stored");
    expect(envelope.result_ref).toMatch(/^fused-result:/);
    const next = await runExecute(envelope.next_request.arguments.script, options, session, { timeoutMs: 30_000, maxCalls: 10, outputBudgetBytes: 1024 });
    expect(next.isError).toBe(false);
    expect(JSON.parse(next.text)).toMatchObject({ recovery_action: "adjust_result_projection", execute_request: "adjust_projection", provider_execution: "complete" });
  }
});

// Long provider-authored paths fall back before their exact text can crowd the retained reference out of the envelope.
it("keeps long-path retained results recoverable at the minimum budget", async () => {
  const session = new SessionState();
  const options = { sessionId: "synthetic", enginePort: "1" };
  const stored = await runExecute('return {["k".repeat(256)]:Array(20).fill({id:1,body:"x".repeat(100)})}', options, session, { timeoutMs: 30_000, maxCalls: 10, outputBudgetBytes: 1024 });
  const envelope = JSON.parse(stored.text);
  expect(stored.delivery).toBe("stored");
  expect(envelope.result_ref).toMatch(/^fused-result:/);
  const next = await runExecute(envelope.next_request.arguments.script, options, session, { timeoutMs: 30_000, maxCalls: 10, outputBudgetBytes: 1024 });
  expect(next.isError).toBe(false);
  expect(JSON.parse(next.text)).toMatchObject({ provider_execution: "complete" });
});

// Retrieval is a pure session read, so repeated selection cannot repeat a provider side effect.
it("executes once and retrieves slices through execute without another provider call", async () => {
  const session = new SessionState();
  const options = { sessionId: "synthetic", enginePort: "1" };
  const callImpl = vi.fn().mockResolvedValue({ items: Array(2000).fill({ title: "fixture", padding: "x".repeat(80) }) });
  const output = await runExecute('return await call("fixture.read", {});', options, session, undefined, callImpl);
  const envelope = JSON.parse(output.text);
  expect(output.delivery).toBe("stored");
  const reference = JSON.stringify(envelope.result_ref);
  const selected = await runExecute(`return session.get(${reference}).items.slice(0, 2).map(item => item.title);`, options, session, undefined, callImpl);
  expect(JSON.parse(selected.text)).toEqual(["fixture", "fixture"]);
  expect(selected.access).toEqual({ retained_reads: 1, unavailable_reads: 0 });
  const whole = await runExecute(`return session.get(${reference});`, options, session, undefined, callImpl);
  expect(JSON.parse(whole.text).result_ref).toBe(envelope.result_ref);
  const missing = await runExecute(`return session.get(${reference});`, options, new SessionState(), undefined, callImpl);
  expect(missing.text).toContain("MCP_RESULT_UNAVAILABLE");
  expect(missing.access).toEqual({ retained_reads: 1, unavailable_reads: 1 });
  expect(callImpl).toHaveBeenCalledTimes(1);
});

// Only trusted serialization may observe getters; previewing the parsed snapshot must not run them again.
it("serializes result getters once and protects the reserved reference namespace", async () => {
  const session = new SessionState();
  const options = { sessionId: "synthetic", enginePort: "1" };
  const outcome = await runExecute('let reads=0; return { get body() { session.set("reads", ++reads); return "x".repeat(20000); } };', options, session);
  expect(outcome.delivery).toBe("stored");
  expect(session.get("reads")).toBe(1);
  const reference = JSON.parse(outcome.text).result_ref;
  expect(() => session.set(reference, "replacement")).toThrow("MCP_RESULT_REFERENCE_RESERVED");
});
