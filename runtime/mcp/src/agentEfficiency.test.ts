import { readFileSync } from "node:fs";
import { getEncoding } from "js-tiktoken";
import { expect, it, vi } from "vitest";
import { Fixture, FixtureOperation } from "./fixture.js";
import { searchDocs } from "./searchDocs.js";
import { listTools } from "./metadata.js";
import { runExecuteRequest } from "./executeRequest.js";
import { SessionState } from "./sandbox.js";
import { MCP_SERVER_INSTRUCTIONS } from "./toolDescriptions.js";

const baseline = JSON.parse(readFileSync(new URL("../testdata/agent-efficiency-baseline.json", import.meta.url), "utf8"));
const tokenizer = getEncoding("o200k_base");
const options = { sessionId: "efficiency-test", enginePort: "1" };
const fixture = new Fixture(baseline.operations as FixtureOperation[], {}, { name: "efficiency", title: "Efficiency", version: "1", description: "Synthetic posts" });

/** Counts serialized public context, including tool schemas, with a pinned real tokenizer rather than character estimates. */
function tokens(value: unknown): number {
  return tokenizer.encode(JSON.stringify(value), [], []).length;
}

/** Includes startup instructions and tool schemas exactly once in each synthetic task transcript. */
function context(legacy: boolean, transcript: unknown[]) {
  // Frozen baseline output prevents a future implementation change from moving its own comparison target.
  return legacy ? { instructions: baseline.instructions, tools: baseline.tools, transcript }
    : { instructions: MCP_SERVER_INSTRUCTIONS, tools: listTools(), transcript };
}

// Leave a small allowance for workflow/reconciliation guidance while keeping fixed context below the released contract.
it("cuts fixed tool context by at least 24 percent against the released contract", () => {
  const previous = tokens(context(true, [])), current = tokens(context(false, []));
  expect(current).toBeLessThan(previous * 0.76);
  process.stdout.write(JSON.stringify({ scenario: "fixed context", tokenizer: "o200k_base", baseline_tokens: previous, current_tokens: current }) + "\n");
});

// A complete discovered schema lets an agent finish a read in two tool calls without schema confirmation or result-shape probes.
it("completes a discovered read in two calls with fewer tokens than the optimal old scripted path", async () => {
  const discovery = searchDocs(fixture, { query: "list posts" });
  expect(discovery).toMatchObject({ operations: [{ execution_ready: true, execute_example: { operationId: "posts", params: {} }, response_summary: { type: "array", fields: ["id", "title", "status", "content"] } }] });
  const args = { operationId: "posts", select: ["title", "status"], saveAs: "posts" };
  const provider = vi.fn().mockResolvedValue(baseline.posts);
  const output = await runExecuteRequest(args, options, new SessionState(), undefined, provider);
  const result = JSON.parse(output.text);
  expect(output.isError).toBe(false);
  expect(result).toEqual([{ title: "Unified Apps", status: "published" }, { title: "Billing support", status: "draft" }]);
  expect(provider).toHaveBeenCalledTimes(1);
  const currentTrace = [{ tool: "search_docs", arguments: { query: "list posts" }, result: discovery }, { tool: "execute", arguments: args, result }];
  const oldTrace = [{ tool: "search_docs", arguments: { query: "list posts" }, result: baseline.query }, {
    tool: "execute", arguments: { script: 'const posts = await call("posts", {}); session.set("posts", posts); return posts.map(({title,status}) => ({title,status}));' }, result,
  }];
  expect(currentTrace).toHaveLength(2);
  const before = tokens(context(true, oldTrace)), after = tokens(context(false, currentTrace));
  expect(after).toBeLessThan(before * 0.8);
  process.stdout.write(JSON.stringify({ scenario: "read titles", baseline_tool_calls: oldTrace.length, current_tool_calls: currentTrace.length, provider_calls: provider.mock.calls.length, baseline_tokens: before, current_tokens: after }) + "\n");
});

// Reproduce the observed shape-probe/data-exception pattern, then prove the direct path returns the same data with one fewer call.
it("eliminates the extra inspection call in the reported debug workflow", async () => {
  const provider = vi.fn().mockResolvedValue(baseline.posts);
  const session = new SessionState();
  const oldRequests = [
    { script: 'const r = await call("posts", {}); throw new Error("DEBUG " + JSON.stringify({t:typeof r,isArr:Array.isArray(r),keys:Object.keys(r ?? {}).slice(0,10),n:Array.isArray(r)?r.length:null}));' },
    { script: 'const r = await call("posts", {}); throw new Error("DATA " + JSON.stringify(r));' },
  ];
  const oldTrace: unknown[] = [{ tool: "search_docs", arguments: { operationId: "posts" }, result: baseline.exact }];
  for (const request of oldRequests) {
    const result = await runExecuteRequest(request, options, session, undefined, provider);
    expect(result.isError).toBe(true);
    oldTrace.push({ tool: "execute", arguments: request, result: JSON.parse(result.text) });
  }
  const oldProviderCalls = provider.mock.calls.length;
  provider.mockClear();
  const discovery = searchDocs(fixture, { operationId: "posts" });
  const result = await runExecuteRequest({ operationId: "posts" }, options, new SessionState(), undefined, provider);
  expect(result.isError).toBe(false);
  expect(JSON.parse(result.text)).toEqual(baseline.posts);
  const currentTrace = [{ tool: "search_docs", arguments: { operationId: "posts" }, result: discovery }, { tool: "execute", arguments: { operationId: "posts" }, result: JSON.parse(result.text) }];
  expect(currentTrace.length).toBeLessThan(oldTrace.length);
  expect(provider.mock.calls.length).toBeLessThan(oldProviderCalls);
  const before = tokens(context(true, oldTrace)), after = tokens(context(false, currentTrace));
  expect(after).toBeLessThan(before * 0.7);
  process.stdout.write(JSON.stringify({ scenario: "reported inspection workflow", baseline_tool_calls: oldTrace.length, current_tool_calls: currentTrace.length, baseline_provider_calls: oldProviderCalls, current_provider_calls: provider.mock.calls.length, baseline_tokens: before, current_tokens: after }) + "\n");
});

// Follow-up questions must reuse the original full data even when the initial answer showed only selected fields.
it("answers a follow-up from saved state without rediscovery or provider replay", async () => {
  const provider = vi.fn().mockResolvedValue(baseline.posts);
  const session = new SessionState();
  await runExecuteRequest({ operationId: "posts", select: ["title"], saveAs: "posts" }, options, session, undefined, provider);
  const result = await runExecuteRequest({ script: 'return session.get("posts").find(post => post.id === "post-1").content;' }, options, session, undefined, provider);
  expect(result.isError).toBe(false);
  expect(JSON.parse(result.text)).toBe("Full first article.");
  expect(provider).toHaveBeenCalledTimes(1);
});
