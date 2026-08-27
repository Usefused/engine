import { afterEach, expect, it, vi } from "vitest";
import { discoverCollections } from "./resultCollections.js";
import { pageResult, ResultPage } from "./resultPaging.js";
import { EXECUTE_INLINE_BYTES, EXECUTE_VISIBLE_OUTPUT_POLICY, RETAINED_RESULT_TTL_MS, RetainedResults } from "./retainedResults.js";
import { executeOutputBudget } from "./resultBudget.js";
import { DEFAULT_EXECUTE_LIMITS, runExecute, SessionState } from "./sandbox.js";

const reference = "fused-result:synthetic";
const callOptions = { sessionId: "synthetic", enginePort: "1" };

// Fake time must not affect unrelated VM deadlines.
afterEach(() => vi.useRealTimers());

/** Builds synthetic variable-width rows to test projection without loading personal or provider data. */
function transactions(count = 120) {
  // Deliberate escapes and multibyte characters distinguish UTF-8 size from JavaScript string length.
  return Array.from({ length: count }, (_, id) => ({ id, amount: id + 0.5, currency: "GBP", merchant: "商店😀\"\\".repeat(id % 5 + 1), raw: "PRIVATE_SENTINEL".repeat(40) }));
}

/** Requires successful stored navigation and returns only its opaque runtime reference to the test. */
function retain(session: SessionState, value: unknown): string {
  const output = session.deliver(value, 1024);
  expect(output.delivery).toBe("stored");
  return JSON.parse(output.text).result_ref;
}

// A configurable byte policy must reject invalid values before any provider side effect occurs.
it("validates budgets and preserves small or explicitly enlarged inline results", async () => {
  expect(executeOutputBudget()).toBe(16384);
  expect(executeOutputBudget(65536)).toBe(65536);
  for (const budget of [0, 1023, 65537, NaN, Infinity, 1024.5, "4096", null]) {
    expect(() => executeOutputBudget(budget)).toThrow("MCP_OUTPUT_BUDGET_INVALID");
  }
  const callImpl = vi.fn();
  const failed = await runExecute('return await call("fixture.read", {})', callOptions, new SessionState(), { ...DEFAULT_EXECUTE_LIMITS, outputBudgetBytes: 65537 }, callImpl);
  expect(failed.text).toContain("MCP_OUTPUT_BUDGET_INVALID");
  expect(callImpl).not.toHaveBeenCalled();
  const result = new RetainedResults();
  expect(result.deliver("x".repeat(12000)).delivery).toBe("inline");
  expect(result.deliver("x".repeat(32000), 65536).delivery).toBe("inline");
});

// Every row must appear exactly once, with actual serialized bytes deciding the page boundary.
it("packs maximal whole-row pages without gaps, duplicates, or retained page envelopes", async () => {
  const rows = transactions();
  const session = new SessionState();
  const ref = retain(session, { transactions: rows });
  const received: unknown[] = [];
  let offset = 0;
  let script = `return session.page(${JSON.stringify(ref)}, {path:"/transactions", fields:["id","merchant","amount","currency"],offset:0});`;
  for (let attempt = 0; attempt < rows.length; attempt++) {
    const output = await runExecute(script, callOptions, session, { ...DEFAULT_EXECUTE_LIMITS, outputBudgetBytes: 1024 });
    expect(output.delivery).toBe("inline");
    expect(output.isError).toBe(false);
    expect(output.access.retained_reads).toBe(1);
    expect(Buffer.byteLength(output.text)).toBeLessThanOrEqual(1024);
    const page = JSON.parse(output.text) as ResultPage;
    expect(page.offset).toBe(offset);
    expect(page.total).toBe(rows.length);
    expect(page.returned).toBe(page.items.length);
    expect(page.returned).toBeGreaterThan(0);
    expect(output.text).not.toContain("PRIVATE_SENTINEL");
    received.push(...page.items);
    // The final page terminates traversal; all others must advance an exact snapshot position.
    if (page.complete) {
      expect(page.provider_execution).toBe("complete");
      expect(page.automatic_replay).toBe(false);
      expect(page.nextOffset).toBeNull();
      expect(page.next_request).toBeUndefined();
      break;
    }
    expect(page).toMatchObject({ recovery_action: "continue_stored_result", execute_request: "use_next_request", provider_execution: "complete", automatic_replay: false });
    expect(page.nextOffset).toBe(offset + page.returned);
    expect(page.next_request).toMatchObject({ tool: "execute", arguments: { outputBudgetBytes: 1024 } });
    const next = rows[page.nextOffset!];
    // One more full row must exceed the budget; otherwise the runtime caused an avoidable round trip.
    const extended = { ...page, returned: page.returned + 1, nextOffset: page.nextOffset! + 1, complete: false, items: [...page.items, { id: next.id, merchant: next.merchant, amount: next.amount, currency: next.currency }] };
    expect(Buffer.byteLength(JSON.stringify(extended))).toBeGreaterThan(1024);
    offset = page.nextOffset!;
    // Execute the supplied continuation verbatim so selector and cursor reconstruction cannot hide a defect.
    script = page.next_request!.arguments.script;
  }
  // Field selection preserves exact values and order, excluding only the explicitly unselected payload.
  expect(received).toEqual(rows.map(({ id, merchant, amount, currency }) => ({ id, merchant, amount, currency })));
});

// Fitting all selected rows should yield one page even when the unprojected snapshot was large.
it("returns a complete projected collection in one retrieval and calls the provider only once", async () => {
  const session = new SessionState();
  const callImpl = vi.fn().mockResolvedValue({ transactions: transactions(80) });
  const first = await runExecute('return await call("fixture.read", {});', callOptions, session, undefined, callImpl);
  const envelope = JSON.parse(first.text);
  expect(envelope.collections[0]).toMatchObject({ path: "/transactions", count: 80, fields_complete: true });
  expect(envelope.collections[0].fields).toContain("amount");
  expect(first.text).not.toContain("PRIVATE_SENTINEL");
  const second = await runExecute(`return session.page(${JSON.stringify(envelope.result_ref)}, {path:"/transactions",fields:["id","amount"]});`, callOptions, session, undefined, callImpl);
  expect(second.delivery).toBe("inline");
  expect(JSON.parse(second.text)).toMatchObject({ total: 80, returned: 80, complete: true, nextOffset: null });
  expect(callImpl).toHaveBeenCalledTimes(1);
});

// RFC 6901 and own-key access must work identically for discovery and retrieval, including special names.
it("supports root arrays, escaped collection paths, literal fields, and sparse null values", () => {
  const root = JSON.parse('{"a/b":{"~items":[{"__proto__":"own","a.b":null},{"other":false}]}}');
  const description = discoverCollections(root).collections[0];
  expect(description.path).toBe("/a~1b/~0items");
  expect(description.fields_complete).toBe(true);
  expect(description.fields).toEqual(["__proto__", "a.b", "other"]);
  const page = pageResult(root, reference, { path: description.path, fields: ["__proto__", "a.b"] }, 1024);
  expect(JSON.parse(JSON.stringify(page.items))).toEqual(JSON.parse('[{"__proto__":"own","a.b":null},{}]'));
  expect(pageResult([1, null, false, [2]], reference, {}, 1024).items).toEqual([1, null, false, [2]]);
  expect(pageResult([], reference, { fields: ["id"] }, 1024)).toMatchObject({ returned: 0, total: 0, nextOffset: null, complete: true });
  expect(pageResult([1], reference, { offset: 1 }, 1024)).toMatchObject({ returned: 0, offset: 1, complete: true });
});

// Generic nested traversal must work with Unified all-settled results without provider-specific normalization.
it("advertises collections nested inside result arrays without exposing scalar values", () => {
  const root = { results: [{ target: "PRIVATE_SENTINEL", data: { transactions: [{ id: "PRIVATE_SENTINEL", amount: 1 }] } }] };
  const discovered = discoverCollections(root);
  expect(discovered.collections).toContainEqual({ path: "/results/0/data/transactions", count: 1, fields: ["id", "amount"], fields_complete: true });
  expect(JSON.stringify(discovered)).not.toContain("PRIVATE_SENTINEL");
  expect(pageResult(root, reference, { path: "/results/0/data/transactions", fields: ["amount"] }, 1024).items).toEqual([{ amount: 1 }]);
});

// Discovery cannot claim complete schemas from a sample or silently hide wide/nested collections.
it("marks row, key, path, traversal, and envelope metadata limits incomplete", () => {
  const sampled = discoverCollections({ rows: [...Array(512).fill({ id: 1 }), { later: true }] }).collections[0];
  expect(sampled.fields).toEqual(["id"]);
  expect(sampled.fields_complete).toBe(false);
  // Wide synthetic keys force bounded inventories without requiring any scalar samples.
  const wide = Object.fromEntries(Array.from({ length: 40 }, (_, index) => [`field_${index}_${"k".repeat(100)}`, "PRIVATE_SENTINEL"]));
  const results = new RetainedResults();
  const output = results.deliver({ rows: [wide], padding: "PRIVATE_SENTINEL".repeat(2000) }, 1024);
  expect(output.delivery).toBe("stored");
  expect(Buffer.byteLength(output.text)).toBeLessThanOrEqual(1024);
  expect(output.text).not.toContain("PRIVATE_SENTINEL");
  expect(JSON.parse(output.text).collections[0].fields_complete).toBe(false);
  expect(discoverCollections({ rows: [{ ["k".repeat(129)]: 1 }] }).collections[0].fields_complete).toBe(false);
  // More than eight separate arrays makes catalogue truncation explicit rather than silently exhaustive.
  const many = Object.fromEntries(Array.from({ length: 40 }, (_, index) => [`list${index}`, []]));
  expect(discoverCollections(many).collections_complete).toBe(false);
  expect(discoverCollections({ ["k".repeat(2049)]: [] }).collections_complete).toBe(false);
});

// Bad selectors must fail with stable, value-free errors instead of returning misleading empty data.
it("rejects invalid options, missing paths, inherited keys, and unknown fields", () => {
  const root = { rows: [{ id: 1 }] };
  const options = [
    null, [], { limit: 10 }, { path: "rows" }, { path: "/rows/~2" }, { path: "/missing" },
    { path: "/rows/00" }, { path: "/constructor" }, { path: "/rows", offset: -1 },
    { path: "/rows", offset: 0.5 }, { path: "/rows", offset: 2 }, { path: "/rows", offset: Infinity },
    { path: "/rows", fields: [] }, { path: "/rows", fields: ["id", "id"] },
    { path: "/rows", fields: [1] }, { path: "/rows", fields: ["constructor"] },
    { path: "/rows", fields: ["missing"] }, { path: "/rows", fields: ["k".repeat(129)] },
  ];
  for (const option of options) {
    expect(() => pageResult(root, reference, option, 1024)).toThrow(/MCP_RESULT_PAGE_/);
  }
  expect(() => pageResult([{ id: 1 }, null], reference, { fields: ["id"] }, 1024)).toThrow("MCP_RESULT_PAGE_ROW_TYPE");
});

// A row larger than the whole budget cannot yield an empty page with an unchanged continuation.
it("fails oversized rows explicitly and allows recovery with narrower fields on the same reference", () => {
  const session = new SessionState();
  const ref = retain(session, { rows: [{ id: 1, raw: "x".repeat(20000) }] });
  expect(() => session.page(ref, { path: "/rows" }, 1024)).toThrow("MCP_RESULT_ROW_TOO_LARGE");
  expect(session.page(ref, { path: "/rows", fields: ["id"] }, 1024)).toMatchObject({ returned: 1, complete: true, items: [{ id: 1 }] });
  expect(() => pageResult({ ["k".repeat(1500)]: [] }, reference, { path: "/" + "k".repeat(1500) }, 1024)).toThrow("MCP_RESULT_PAGE_METADATA_TOO_LARGE");
});

// Paging uses the same absolute expiry and isolation as raw retained reads, never a mutable session.set value.
it("preserves expiry, cross-session denial, snapshot immutability, and reserved-reference behavior", () => {
  vi.useFakeTimers();
  const session = new SessionState();
  const ref = retain(session, transactions());
  const page = session.page(ref, { fields: ["id"] }) as ResultPage;
  (page.items[0] as { id: number }).id = 999;
  expect((session.page(ref, { fields: ["id"] }) as ResultPage).items[0]).toEqual({ id: 0 });
  expect(() => new SessionState().page(ref)).toThrow("MCP_RESULT_UNAVAILABLE");
  session.set("manual", transactions());
  expect(() => session.page("manual")).toThrow("MCP_RESULT_REFERENCE_INVALID");
  vi.advanceTimersByTime(RETAINED_RESULT_TTL_MS);
  expect(() => session.page(ref)).toThrow("MCP_RESULT_UNAVAILABLE");
});

// Adjacent byte boundaries include changing envelope digits and escaped UTF-8 rows, not just payload length.
it("fits exactly at the page byte boundary and stops one byte below it", () => {
  const rows = ["😀\"\\".repeat(200), "商店".repeat(200)];
  const whole = pageResult(rows, reference, {}, EXECUTE_VISIBLE_OUTPUT_POLICY.maxBytes);
  const size = Buffer.byteLength(JSON.stringify(whole));
  expect(pageResult(rows, reference, {}, size).returned).toBe(2);
  expect(pageResult(rows, reference, {}, size - 1).returned).toBe(1);
  expect(EXECUTE_INLINE_BYTES).toBe(16 * 1024);
});

// Terminal metadata shrinks when a six-digit continuation becomes null; a greedy first-overflow stop loses rows.
it("considers a fitting terminal tail when an intermediate prefix narrowly exceeds the budget", () => {
  const rows = Array(400000).fill(0);
  const options = { offset: 399500 };
  const whole = pageResult(rows, reference, options, 65536);
  const size = Buffer.byteLength(JSON.stringify(whole));
  expect(size).toBeGreaterThanOrEqual(1024);
  expect(pageResult(rows, reference, options, size)).toEqual(whole);
});
