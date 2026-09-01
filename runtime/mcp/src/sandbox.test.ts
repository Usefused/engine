import { afterEach, describe, expect, it, vi } from "vitest";
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

/** Decodes one admitted execute error while pinning its outer delivery classification. */
function errorValue(outcome: ExecuteOutcome): Record<string, unknown> {
  // Recovery assertions are meaningful only for the public error envelope.
  expect(outcome.isError).toBe(true);
  return JSON.parse(outcome.text) as Record<string, unknown>;
}

afterEach(() => {
  // Host transport stubs must never leak into another sandbox invocation test.
  vi.unstubAllGlobals();
});

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
  // The transport receives invocation-owned cancellation without exposing it to script params.
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
    expect(callImpl).toHaveBeenCalledWith(testCallOptions, "test.op", { x: 1 }, expect.any(AbortSignal), undefined);
  });

  /** Physical pagination options reach the Engine separately while malformed controls stop before transport. */
  it("forwards only strict physical pagination options", async () => {
    const callImpl = vi.fn().mockResolvedValue({ messages: [{ id: "latest" }] });
    const session = new SessionState();
    const accepted = await runExecute(
      'return await call("gmail.users.messages.list", {userId:"me",maxResults:1}, {pagination:{maxPages:1}});',
      testCallOptions,
      session,
      undefined,
      callImpl,
    );
    expect(accepted.isError).toBe(false);
    expect(callImpl).toHaveBeenCalledWith(
      testCallOptions,
      "gmail.users.messages.list",
      { userId: "me", maxResults: 1 },
      expect.any(AbortSignal),
      { pagination: { maxPages: 1 } },
    );

    const engineBound = await runExecute(
      'return await call("test.op", {}, {pagination:{maxPages:1001}});',
      testCallOptions,
      session,
      undefined,
      callImpl,
    );
    // The runtime owns shape admission; the Engine remains the single source for operation and global upper bounds.
    expect(engineBound.isError).toBe(false);
    expect(callImpl).toHaveBeenLastCalledWith(
      testCallOptions,
      "test.op",
      {},
      expect.any(AbortSignal),
      { pagination: { maxPages: 1001 } },
    );

    const invalid = [
      "null",
      "{}",
      '{pagination:{maxPages:0}}',
      '{pagination:{maxPages:1.5}}',
      '{pagination:{max_pages:1}}',
      '{pagination:{maxPages:1},unknown:true}',
    ];
    for (const options of invalid) {
      // Every rejected shape must fail before another Engine bridge call begins.
      const outcome = await runExecute(`return await call("test.op", {}, ${options});`, testCallOptions, session, undefined, callImpl);
      const failure = JSON.parse(outcome.text);
      expect(failure.code).toMatch(/MCP_CALL_(OPTIONS|PAGINATION)_INVALID/);
      expect(failure.message).toContain("search_docs exact operationId detail");
      expect(failure).toMatchObject({ recovery_action: "correct_execute_arguments", execute_request: "correct_arguments", provider_execution: "not_started", automatic_replay: false });
    }
    expect(callImpl).toHaveBeenCalledTimes(2);
  });

  /** Engine-owned structured rejections survive outer formatting when the first bridge call proves it did not dispatch. */
  it("preserves single first-call physical pagination corrections", async () => {
    const cases = [
      { code: "mcp_pagination_not_supported", operationId: "gmail.users.messages.get", message: 'operation "gmail.users.messages.get" is not paginated; use call("gmail.users.messages.get", params)' },
      { code: "mcp_physical_pagination_not_allowed_for_unified", operationId: "release.provision", message: 'operation "release.provision" is Unified; use call("release.provision", params)' },
    ];
    for (const test of cases) {
      vi.stubGlobal("fetch", vi.fn().mockResolvedValue({
        ok: false,
        status: 400,
        json: async () => ({
          code: test.code,
          error: `${test.code}: ${test.message}`,
          recovery_action: "correct_execute_arguments",
          execute_request: "correct_arguments",
          provider_execution: "not_started",
          automatic_replay: false,
        }),
      }));
      const outcome = await runExecute(
        `return await call(${JSON.stringify(test.operationId)}, {}, {pagination:{maxPages:1}});`,
        testCallOptions,
        new SessionState(),
      );
      const failure = errorValue(outcome);
      expect(failure).toMatchObject({ code: test.code, recovery_action: "correct_execute_arguments", execute_request: "correct_arguments", provider_execution: "not_started", automatic_replay: false });
      expect(failure.message).toContain(`call(${JSON.stringify(test.operationId)}, params)`);
    }
  });

  /** A first-call missing user selector remains actionable in the final model-visible execute error. */
  it("preserves the MCP end-user connection correction", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({
      ok: false,
      status: 400,
      json: async () => ({
        code: "MCP_END_USER_REF_REQUIRED",
        error: "MCP_END_USER_REF_REQUIRED: This operation requires connected OAuth/OIDC credentials. Configure X-Fused-End-User-Ref on the MCP client connection for the intended user, then retry.",
        recovery_action: "correct_execute_arguments",
        execute_request: "correct_arguments",
        provider_execution: "not_started",
        automatic_replay: false,
      }),
    }));

    const failure = errorValue(await runExecute('return await call("jira.searchProjects", {});', testCallOptions, new SessionState()));
    expect(failure).toMatchObject({
      code: "MCP_END_USER_REF_REQUIRED",
      recovery_action: "correct_execute_arguments",
      execute_request: "correct_arguments",
      provider_execution: "not_started",
      automatic_replay: false,
    });
    expect(failure.message).toContain("Configure X-Fused-End-User-Ref on the MCP client connection");
  });

  /** A first-call static credential miss remains actionable without claiming any provider execution. */
  it("preserves the credential setup action in the final execute error", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({
      ok: false,
      status: 409,
      json: async () => ({
        code: "bucket_credentials_missing",
        error: "bucket_credentials_missing: provider credentials are not configured; run: fused-cli secret set service-id --bucket bucket-id --interactive",
        recovery_action: "complete_authentication",
        execute_request: "retry_after_auth",
        provider_execution: "not_started",
        automatic_replay: false,
      }),
    }));

    const failure = errorValue(await runExecute('return await call("resend.sendEmail", {});', testCallOptions, new SessionState()));
    // A single bridge-proven rejection survives the outer runtime without being downgraded to an unknown provider outcome.
    expect(failure).toMatchObject({
      code: "bucket_credentials_missing",
      recovery_action: "complete_authentication",
      execute_request: "retry_after_auth",
      provider_execution: "not_started",
      automatic_replay: false,
    });
    expect(failure.message).toContain("fused-cli secret set service-id --bucket bucket-id --interactive");
  });

  /** A first-call auth challenge carries a browser URL and safe exact-tool replay decision outside model text. */
  it("preserves browser authentication for an isolated call", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({
      ok: false, status: 409,
      json: async () => ({
        code: "connection_required", error: "Provider authentication is required to continue.",
        recovery_action: "complete_authentication", execute_request: "retry_after_auth",
        provider_execution: "not_started", automatic_replay: false,
        auth_action: { action: "connect", url: "https://provider.example.com/oauth", elicitation_id: "opaque-1", expires_at: "2026-08-29T17:00:00Z" },
      }),
    }));
    const outcome = await runExecute('return await call("gmail.list", {});', testCallOptions, new SessionState());
    expect(outcome.authAction).toMatchObject({ action: "connect", url: "https://provider.example.com/oauth", recovery: { provider_execution: "not_started", execute_request: "retry_after_auth" } });
  });

  /** Earlier provider work keeps the browser link while forbidding complete-script replay. */
  it("downgrades browser authentication replay after a sibling call", async () => {
    const fetchMock = vi.fn().mockImplementation(async (_url: string, init: RequestInit) => {
      const operationId = JSON.parse(String(init.body)).operation_id;
      // Only the second bridge call requires authentication in this aggregate script.
      if (operationId === "auth") {
        return { ok: false, status: 409, json: async () => ({
          code: "reconnect_required", error: "Provider authentication is required to continue.",
          recovery_action: "complete_authentication", execute_request: "retry_after_auth",
          provider_execution: "not_started", automatic_replay: false,
          auth_action: { action: "reconnect", url: "https://provider.example.com/oauth", elicitation_id: "opaque-2", expires_at: "2026-08-29T17:00:00Z" },
        }) };
      }
      return { ok: true, status: 200, json: async () => ({ result: { ok: true } }) };
    });
    vi.stubGlobal("fetch", fetchMock);
    const outcome = await runExecute('await call("first", {}); return await call("auth", {});', testCallOptions, new SessionState());
    expect(outcome.authAction).toMatchObject({ action: "reconnect", recovery: { recovery_action: "complete_authentication", provider_execution: "unknown", execute_request: "do_not_replay" } });
  });

  /** Any earlier or concurrent admitted call makes a later local option correction unsafe as an aggregate outcome. */
  it("downgrades local pagination corrections after another call starts", async () => {
    const scripts = [
      'await call("valid", {}); return await call("invalid", {}, null);',
      'return await Promise.all([call("valid", {}), call("invalid", {}, null)]);',
    ];
    for (const script of scripts) {
      const callImpl = vi.fn().mockResolvedValue({ ok: true });
      const failure = errorValue(await runExecute(script, testCallOptions, new SessionState(), undefined, callImpl));
      expect(failure).toMatchObject({ code: "MCP_CALL_OPTIONS_INVALID", recovery_action: "do_not_replay", execute_request: "do_not_replay", provider_execution: "unknown", automatic_replay: false });
      expect(callImpl).toHaveBeenCalledTimes(1);
    }
  });

  /** Structured not-started metadata is call-scoped and cannot erase an earlier or concurrent bridge attempt. */
  it("downgrades Engine pagination corrections when another bridge call starts", async () => {
    const fetchMock = vi.fn().mockImplementation(async (_url: string, init: RequestInit) => {
      const operationId = JSON.parse(String(init.body)).operation_id;
      // Only the invalid operation returns the Engine-proven pre-provider rejection under test.
      if (operationId === "invalid") {
        return {
          ok: false,
          status: 400,
          json: async () => ({
            code: "mcp_pagination_not_supported",
            error: "mcp_pagination_not_supported: invalid operation is not paginated",
            recovery_action: "correct_execute_arguments",
            execute_request: "correct_arguments",
            provider_execution: "not_started",
            automatic_replay: false,
          }),
        };
      }
      return { ok: true, status: 200, json: async () => ({ result: { ok: true } }) };
    });
    vi.stubGlobal("fetch", fetchMock);
    const scripts = [
      'await call("valid", {}); return await call("invalid", {}, {pagination:{maxPages:1}});',
      'return await Promise.all([call("valid", {}), call("invalid", {}, {pagination:{maxPages:1}})]);',
    ];
    for (const script of scripts) {
      const failure = errorValue(await runExecute(script, testCallOptions, new SessionState()));
      expect(failure).toMatchObject({ code: "mcp_pagination_not_supported", recovery_action: "do_not_replay", execute_request: "do_not_replay", provider_execution: "unknown", automatic_replay: false });
    }
  });

  /** session.get fails explicitly on ignored options before reading or re-retaining a root result. */
  it("rejects extra session.get arguments with navigation guidance", async () => {
    const session = new SessionState();
    const retained = session.deliver({ messages: Array.from({ length: 100 }, (_, id) => ({ id, body: "x".repeat(200) })) }, 1024);
    const reference = JSON.parse(retained.text).result_ref;

    const outcome = await runExecute(`return session.get(${JSON.stringify(reference)}, {path:"/messages"});`, testCallOptions, session);

    expect(outcome.isError).toBe(true);
    expect(outcome.text).toContain("MCP_SESSION_GET_ARGUMENTS_INVALID");
    expect(outcome.text).toContain("session.page");
    expect(outcome.access).toEqual({ retained_reads: 0, unavailable_reads: 0 });
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

  // Model-visible recovery stays compact while trusted delivery metadata remains separate.
  it("a script error is returned as a clean error, not thrown", async () => {
    const outcome = await runExecute(
      'throw new Error("boom");',
      testCallOptions,
      new SessionState(),
    );
    expect(outcome).toMatchObject({ isError: true, delivery: "error" });
    expect(JSON.parse(outcome.text)).toMatchObject({ message: "boom", recovery_action: "do_not_replay", execute_request: "do_not_replay", provider_execution: "unknown" });
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

    expect(outcome).toMatchObject({ isError: true, delivery: "error" });
    expect(JSON.parse(outcome.text)).toMatchObject({ message: "safe text", recovery_action: "do_not_replay", provider_execution: "unknown" });
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
