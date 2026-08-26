import { describe, expect, it, vi } from "vitest";
import { CallClientOptions } from "./callClient.js";
import { runExecute, SessionState, ExecuteLimits, ExecuteOutcome } from "./sandbox.js";
import { EXECUTE_RESULT_OUTPUT_POLICY } from "./outputLimits.js";

const testCallOptions: CallClientOptions = { sessionId: "sess-1", enginePort: "1234" };

/** Decodes only admitted output so tests exercise the same trusted text consumed by the MCP handler. */
function resultValue(outcome: ExecuteOutcome): unknown {
  // An execution error must not accidentally satisfy a successful-result assertion.
  expect(outcome.isError).toBe(false);
  return JSON.parse(outcome.text);
}

describe("runExecute -- sandbox allowlist (security-critical)", () => {
  it("cannot reach fetch", async () => {
    const outcome = await runExecute(
      "return typeof fetch;",
      testCallOptions,
      new SessionState(),
    );
    expect(resultValue(outcome)).toBe("undefined");
  });

  it("cannot reach process", async () => {
    const outcome = await runExecute(
      "return typeof process;",
      testCallOptions,
      new SessionState(),
    );
    expect(resultValue(outcome)).toBe("undefined");
  });

  it("cannot reach require", async () => {
    const outcome = await runExecute(
      "return typeof require;",
      testCallOptions,
      new SessionState(),
    );
    expect(resultValue(outcome)).toBe("undefined");
  });

  it("cannot reach Node's Buffer, global, or globalThis host objects", async () => {
    const outcome = await runExecute(
      "return [typeof Buffer, typeof global].join(',');",
      testCallOptions,
      new SessionState(),
    );
    expect(resultValue(outcome)).toBe("undefined,undefined");
  });

  it("standard JS built-ins (not injected, inherent to any realm) are still usable", async () => {
    const outcome = await runExecute(
      "return JSON.stringify({ a: Math.max(1, 2), b: [1,2,3].map(x => x * 2) });",
      testCallOptions,
      new SessionState(),
    );
    expect(resultValue(outcome)).toBe('{"a":2,"b":[2,4,6]}');
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
    expect(resultValue(outcome)).toEqual({ ok: true });
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
    expect(outcome.isError).toBe(true);
    expect(outcome.text).toMatch(/call\(\) limit exceeded/);
    expect(callImpl).toHaveBeenCalledTimes(2);
  });

  // Delivery metadata stays separate from the public error text.
  it("a script error is returned as a clean error, not thrown", async () => {
    const outcome = await runExecute(
      'throw new Error("boom");',
      testCallOptions,
      new SessionState(),
    );
    expect(outcome).toMatchObject({ isError: true, text: "boom", delivery: "error" });
  });
});

describe("runExecute -- session state", () => {
  it("persists values across separate invocations sharing one SessionState", async () => {
    const session = new SessionState();
    await runExecute('session.set("k", 42); return null;', testCallOptions, session);
    const outcome = await runExecute('return session.get("k");', testCallOptions, session);
    expect(resultValue(outcome)).toBe(42);
  });
});

describe("runExecute -- realm freshness", () => {
  it("does not leak global mutations from one invocation into the next", async () => {
    await runExecute("globalThis.polluted = true; return null;", testCallOptions, new SessionState());
    const outcome = await runExecute("return typeof globalThis.polluted;", testCallOptions, new SessionState());
    expect(resultValue(outcome)).toBe("undefined");
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
    expect(outcome.isError).toBe(true);
    expect(outcome.text).toMatch(/timed out/);
  }, 2000);
});

describe("runExecute -- bounded output", () => {
  // Errors cannot be retrieved by reference and need their own small inline ceiling.
  it("bounds thrown error messages independently of stored results", async () => {
    const outcome = await runExecute(
      `throw new Error("x".repeat(${EXECUTE_RESULT_OUTPUT_POLICY.maxBytes + 1}));`,
      testCallOptions,
      new SessionState(),
    );

    expect(outcome.isError).toBe(true);
    expect(JSON.parse(outcome.text).code).toBe("MCP_EXECUTE_ERROR_OUTPUT_LIMIT_EXCEEDED");
    expect(Buffer.byteLength(outcome.text, "utf8")).toBeLessThan(1024);
  });

  it("rejects large results before returning raw objects to the MCP handler", async () => {
    const outcome = await runExecute(
      `return "x".repeat(${EXECUTE_RESULT_OUTPUT_POLICY.maxBytes + 1});`,
      testCallOptions,
      new SessionState(),
    );

    expect(JSON.parse(outcome.text).code).toBe("MCP_EXECUTE_RESULT_LIMIT_EXCEEDED");
    expect(outcome).not.toHaveProperty("result");
  });

  it("keeps result getters and toJSON hooks inside the execute deadline", async () => {
    const limits: ExecuteLimits = { timeoutMs: 25, maxCalls: 10 };
    const scripts = [
      "return { get value() { while (true) {} } };",
      "return { toJSON() { while (true) {} } };",
    ];
    for (const script of scripts) {
      const outcome = await runExecute(script, testCallOptions, new SessionState(), limits);
      expect(outcome.isError).toBe(true);
      expect(JSON.parse(outcome.text).code).toBe("MCP_EXECUTE_TIMEOUT");
    }
  }, 2000);

  it("keeps thrown message getters and toString hooks inside the execute deadline", async () => {
    const limits: ExecuteLimits = { timeoutMs: 25, maxCalls: 10 };
    const scripts = [
      "throw { get message() { while (true) {} } };",
      "throw { toString() { while (true) {} } };",
    ];
    for (const script of scripts) {
      const outcome = await runExecute(script, testCallOptions, new SessionState(), limits);
      expect(outcome.isError).toBe(true);
      expect(JSON.parse(outcome.text).code).toBe("MCP_EXECUTE_TIMEOUT");
    }
  }, 2000);

  it("uses trusted JSON despite script-side serializer replacement", async () => {
    const outcome = await runExecute(
      "JSON.stringify = () => 'spoofed'; return { admitted: true };",
      testCallOptions,
      new SessionState(),
    );

    expect(resultValue(outcome)).toEqual({ admitted: true });
  });

  // Error formatting must not re-evaluate stateful user code when adding delivery metadata.
  it("reads a thrown message getter only once before admitting its text", async () => {
    const outcome = await runExecute(
      "let reads = 0; throw { get message() { return ++reads === 1 ? 'safe text' : { unexpected: true }; } };",
      testCallOptions,
      new SessionState(),
    );

    expect(outcome).toMatchObject({ text: "safe text", isError: true, delivery: "error" });
  });

  it("returns stable serialization failures for circular and BigInt output", async () => {
    const scripts = ["const value = {}; value.self = value; return value;", "return 1n;"];
    for (const script of scripts) {
      const outcome = await runExecute(script, testCallOptions, new SessionState());
      expect(outcome.isError).toBe(true);
      expect(JSON.parse(outcome.text).code).toBe("MCP_OUTPUT_SERIALIZATION_FAILED");
    }
  });
});
