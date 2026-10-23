import { expect, it, vi } from "vitest";
import { runExecuteRequest } from "./executeRequest.js";
import { SessionState } from "./sandbox.js";
import { listTools } from "./metadata.js";

const options = { sessionId: "test", enginePort: "1" };

// Both discovery surfaces must retain exactly two tools and advertise the same direct execution fields.
it("advertises direct execution and scripts without adding a tool", () => {
  const tools = listTools().tools;
  expect(tools.map(tool => tool.name)).toEqual(["search_docs", "execute"]);
  const schema = tools[1].inputSchema as { properties: Record<string, unknown>; required?: string[] };
  expect(Object.keys(schema.properties)).toEqual(expect.arrayContaining(["operationId", "params", "script", "select", "saveAs"]));
  expect(schema.required ?? []).not.toContain("script");
});

// Projection must not alter provider inputs or discard the full result needed for subsequent inspection.
it("calls once, projects fields, and reads saved data without another provider call", async () => {
  const session = new SessionState();
  const call = vi.fn().mockResolvedValue([{ title: "Post", status: "published", content: "Full article" }]);
  const output = await runExecuteRequest({ operationId: "posts", params: { status: "all" }, select: ["title"], saveAs: "posts" }, options, session, undefined, call);
  expect(output.isError).toBe(false);
  expect(JSON.parse(output.text)).toEqual([{ title: "Post" }]);
  expect(call).toHaveBeenCalledTimes(1);
  expect(call.mock.calls[0].slice(1, 3)).toEqual(["posts", { status: "all" }]);
  const read = await runExecuteRequest({ script: 'return session.get("posts")[0].content;' }, options, session, undefined, call);
  expect(JSON.parse(read.text)).toBe("Full article");
  expect(call).toHaveBeenCalledTimes(1);
});

// Every ambiguous request must be rejected before provider mutations or saved-state changes can occur.
it.each([
  {}, { operationId: "posts", script: "return 1;" }, { script: "return 1;", params: {} },
  { script: "return 1;", select: ["id"] }, { operationId: "posts", params: [] },
  { operationId: "posts", select: [] }, { operationId: "posts", saveAs: "fused-result:fake" },
  { operationId: "posts", unknown: true },
])("rejects invalid or conflicting modes without a provider call: %j", async input => {
  const call = vi.fn();
  const output = await runExecuteRequest(input, options, new SessionState(), undefined, call);
  expect(output.isError).toBe(true);
  expect(JSON.parse(output.text)).toMatchObject({ code: "MCP_EXECUTE_ARGUMENTS_INVALID", provider_execution: "not_started", recovery_action: "correct_execute_arguments" });
  expect(call).not.toHaveBeenCalled();
});

// Quoted values and prototype-like property names remain data across generated-script serialization.
it("keeps quoted input literal and strips prototype keys", async () => {
  const id = 'posts"); throw new Error("injected"); //';
  const params = JSON.parse('{"__proto__":{"polluted":true},"text":"\\\"; throw 1;//"}');
  const call = vi.fn().mockResolvedValue({ ok: true });
  const result = await runExecuteRequest({ operationId: id, params }, options, new SessionState(), undefined, call);
  expect(result.isError).toBe(false);
  expect(call.mock.calls[0][1]).toBe(id);
  expect(call.mock.calls[0][2].text).toBe(params.text);
  // Zod strips prototype keys at admission; ordinary quoted provider values remain literal.
  expect(Object.hasOwn(call.mock.calls[0][2], "__proto__")).toBe(false);
});

// A rejected transport is still uncertain; direct syntax must never weaken mutation replay protection.
it("preserves unknown outcomes without retrying", async () => {
  const call = vi.fn().mockRejectedValue(new Error("connection lost"));
  const output = await runExecuteRequest({ operationId: "publishPost", params: { id: "post-1" } }, options, new SessionState(), undefined, call);
  expect(JSON.parse(output.text)).toMatchObject({ provider_execution: "unknown", recovery_action: "do_not_replay", automatic_replay: false });
  expect(call).toHaveBeenCalledTimes(1);
});

// Successful calls followed by a script error have completed provider work, but remain unsafe to replay.
it("reports completed provider work after a script throws data", async () => {
  const call = vi.fn().mockResolvedValue({ id: "post-1" });
  const output = await runExecuteRequest({ script: 'const post = await call("posts", {}); throw new Error("DATA " + JSON.stringify(post));' }, options, new SessionState(), undefined, call);
  expect(JSON.parse(output.text)).toMatchObject({ provider_execution: "complete", recovery_action: "do_not_replay", automatic_replay: false });
  expect(call).toHaveBeenCalledTimes(1);
});

// A later unknown call prevents a previous successful response from being treated as the aggregate outcome.
it("does not hide an unknown sibling outcome", async () => {
  const call = vi.fn().mockResolvedValueOnce({ id: "post-1" }).mockRejectedValueOnce(new Error("connection lost"));
  const output = await runExecuteRequest({ script: 'await call("readPost", {}); await call("publishPost", {});' }, options, new SessionState(), undefined, call);
  expect(JSON.parse(output.text)).toMatchObject({ provider_execution: "unknown", recovery_action: "do_not_replay" });
  expect(call).toHaveBeenCalledTimes(2);
});

// Large direct results use the existing retained-result continuation rather than inviting another provider call.
it("retrieves oversized results through the supplied continuation", async () => {
  const session = new SessionState();
  const call = vi.fn().mockResolvedValue(Array.from({ length: 1000 }, (_, id) => ({ id, title: "Post ".repeat(50) })));
  const output = await runExecuteRequest({ operationId: "posts" }, options, session, undefined, call);
  expect(output.delivery).toBe("stored");
  const continuation = JSON.parse(output.text).next_request;
  expect(continuation.tool).toBe("execute");
  const page = await runExecuteRequest(continuation.arguments, options, session, undefined, call);
  expect(page.isError).toBe(false);
  expect(call).toHaveBeenCalledTimes(1);
});
