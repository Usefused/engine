import { describe, expect, it, vi } from "vitest";
import { CallClientOptions } from "./callClient.js";
import { runExecute, SessionState, ExecuteLimits } from "./sandbox.js";

const testCallOptions: CallClientOptions = { sessionId: "sess-1", enginePort: "1234" };

describe("runExecute -- sandbox allowlist (security-critical)", () => {
  it("cannot reach fetch", async () => {
    const outcome = await runExecute(
      "return typeof fetch;",
      testCallOptions,
      new SessionState(),
    );
    expect(outcome.result).toBe("undefined");
  });

  it("cannot reach process", async () => {
    const outcome = await runExecute(
      "return typeof process;",
      testCallOptions,
      new SessionState(),
    );
    expect(outcome.result).toBe("undefined");
  });

  it("cannot reach require", async () => {
    const outcome = await runExecute(
      "return typeof require;",
      testCallOptions,
      new SessionState(),
    );
    expect(outcome.result).toBe("undefined");
  });

  it("cannot reach Node's Buffer, global, or globalThis host objects", async () => {
    const outcome = await runExecute(
      "return [typeof Buffer, typeof global].join(',');",
      testCallOptions,
      new SessionState(),
    );
    expect(outcome.result).toBe("undefined,undefined");
  });

  it("standard JS built-ins (not injected, inherent to any realm) are still usable", async () => {
    const outcome = await runExecute(
      "return JSON.stringify({ a: Math.max(1, 2), b: [1,2,3].map(x => x * 2) });",
      testCallOptions,
      new SessionState(),
    );
    expect(outcome.result).toBe('{"a":2,"b":[2,4,6]}');
  });
});

describe("runExecute -- call() wiring", () => {
  it("routes call() through the injected transport, not a real network call", async () => {
    const callImpl = vi.fn().mockResolvedValue({ ok: true });
    const outcome = await runExecute(
      'return await call("test.op", { x: 1 });',
      testCallOptions,
      new SessionState(),
      undefined,
      callImpl,
    );
    expect(outcome.error).toBeUndefined();
    expect(outcome.result).toEqual({ ok: true });
    expect(callImpl).toHaveBeenCalledWith(testCallOptions, "test.op", { x: 1 });
  });

  it("enforces the call-count cap", async () => {
    const callImpl = vi.fn().mockResolvedValue({ ok: true });
    const limits: ExecuteLimits = { timeoutMs: 5000, maxCalls: 2 };
    const outcome = await runExecute(
      'await call("a"); await call("b"); await call("c"); return "done";',
      testCallOptions,
      new SessionState(),
      limits,
      callImpl,
    );
    expect(outcome.error).toMatch(/call\(\) limit exceeded/);
    expect(callImpl).toHaveBeenCalledTimes(2);
  });

  it("a script error is returned as a clean error, not thrown", async () => {
    const outcome = await runExecute(
      'throw new Error("boom");',
      testCallOptions,
      new SessionState(),
    );
    expect(outcome.error).toBe("boom");
  });
});

describe("runExecute -- session state", () => {
  it("persists values across separate invocations sharing one SessionState", async () => {
    const session = new SessionState();
    await runExecute('session.set("k", 42); return null;', testCallOptions, session);
    const outcome = await runExecute('return session.get("k");', testCallOptions, session);
    expect(outcome.result).toBe(42);
  });
});

describe("runExecute -- realm freshness", () => {
  it("does not leak global mutations from one invocation into the next", async () => {
    await runExecute("globalThis.polluted = true; return null;", testCallOptions, new SessionState());
    const outcome = await runExecute("return typeof globalThis.polluted;", testCallOptions, new SessionState());
    expect(outcome.result).toBe("undefined");
  });
});

describe("runExecute -- wall-clock timeout", () => {
  it("times out a long-running async script well before it would naturally resolve", async () => {
    const limits: ExecuteLimits = { timeoutMs: 50, maxCalls: 10 };
    const outcome = await runExecute(
      'await new Promise((resolve) => setTimeout(resolve, 5000)); return "too slow";',
      testCallOptions,
      new SessionState(),
      limits,
    );
    expect(outcome.error).toMatch(/timed out/);
  }, 2000);
});
