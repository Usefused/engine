import { afterEach, describe, expect, it, vi } from "vitest";
import { bridgeRecoveryForError, callClientOptionsFromEnv, remoteCall } from "./callClient.js";

describe("callClientOptionsFromEnv", () => {
  const originalEnv = { ...process.env };

  afterEach(() => {
    process.env = { ...originalEnv };
  });

  it("reads FUSED_SESSION_ID and FUSED_ENGINE_PORT", () => {
    process.env.FUSED_SESSION_ID = "sess-123";
    process.env.FUSED_ENGINE_PORT = "8081";
    expect(callClientOptionsFromEnv()).toEqual({ sessionId: "sess-123", enginePort: "8081" });
  });

  it("throws if FUSED_SESSION_ID is missing", () => {
    delete process.env.FUSED_SESSION_ID;
    process.env.FUSED_ENGINE_PORT = "8081";
    expect(() => callClientOptionsFromEnv()).toThrow(/FUSED_SESSION_ID/);
  });

  it("throws if FUSED_ENGINE_PORT is missing", () => {
    process.env.FUSED_SESSION_ID = "sess-123";
    delete process.env.FUSED_ENGINE_PORT;
    expect(() => callClientOptionsFromEnv()).toThrow(/FUSED_ENGINE_PORT/);
  });
});

describe("remoteCall", () => {
  const options = { sessionId: "sess-1", enginePort: "1234" };

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  /** Omitted execution controls retain the established provider-only request envelope. */
  it("sends operation_id/params and the session ID as a bearer token, never a credential", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ result: { hello: "world" } }),
    });
    vi.stubGlobal("fetch", fetchMock);

    const result = await remoteCall(options, "test.op", { a: 1 });

    expect(result).toEqual({ hello: "world" });
    expect(fetchMock).toHaveBeenCalledWith(
      "http://localhost:1234/mcp/call",
      expect.objectContaining({
        method: "POST",
        headers: expect.objectContaining({ Authorization: "Bearer sess-1" }),
        body: JSON.stringify({ operation_id: "test.op", params: { a: 1 } }),
      }),
    );
  });

  /** Caller pagination stays outside provider params and omission preserves the legacy envelope. */
  it("serializes an optional physical pagination intent separately", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ result: { ok: true } }),
    });
    vi.stubGlobal("fetch", fetchMock);

    await remoteCall(options, "gmail.users.messages.list", { userId: "me", maxResults: 1 }, undefined, { pagination: { maxPages: 1 } });

    expect(fetchMock).toHaveBeenCalledWith(
      "http://localhost:1234/mcp/call",
      expect.objectContaining({
        body: JSON.stringify({
          operation_id: "gmail.users.messages.list",
          params: { userId: "me", maxResults: 1 },
          pagination: { maxPages: 1 },
        }),
      }),
    );
  });

  it("throws a clean error on a non-OK response", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: false,
        status: 400,
        json: async () => ({ error: "validation failed: missing field x" }),
      }),
    );

    await expect(remoteCall(options, "test.op", {})).rejects.toThrow(/validation failed/);
  });

  it("throws a clean error when the body carries an error even with HTTP 200", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({ error: "vendor rejected the request" }),
      }),
    );

    await expect(remoteCall(options, "test.op", {})).rejects.toThrow(/vendor rejected/);
  });

  /** Typed bridge failures preserve the stable code used to derive compact model recovery without exposing identity. */
  it("preserves a stable unavailable-session code", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: false,
        status: 404,
        json: async () => ({
          error: "MCP bridge session is unavailable",
          code: "MCP_BRIDGE_SESSION_UNAVAILABLE",
          recovery_action: "reinitialize_connection",
          execute_request: "reformat_if_session_state_used",
          provider_execution: "not_started",
          automatic_replay: false,
        }),
      }),
    );

    await expect(remoteCall(options, "test.op", {})).rejects.toThrow("MCP_BRIDGE_SESSION_UNAVAILABLE: MCP bridge session is unavailable");
  });

  /** Typed pagination failures retain one Engine-owned prefix for outer execute recovery. */
  it("preserves a specific pagination code without duplicating its message prefix", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: false,
        status: 400,
        json: async () => ({
          code: "mcp_pagination_not_supported",
          error: 'mcp_pagination_not_supported: operation "gmail.users.messages.get" is not paginated',
          recovery_action: "correct_execute_arguments",
          execute_request: "correct_arguments",
          provider_execution: "not_started",
          automatic_replay: false,
        }),
      }),
    );

    let failure: unknown;
    try {
      await remoteCall(options, "gmail.users.messages.get", {});
    } catch (error) {
      // Capturing the exact host error verifies identity-bound metadata as well as public prose.
      failure = error;
    }
    expect(failure).toBeInstanceOf(Error);
    expect((failure as Error).message).toBe('mcp_pagination_not_supported: operation "gmail.users.messages.get" is not paginated');
    expect((failure as Error).message).not.toContain("mcp_pagination_not_supported: mcp_pagination_not_supported:");
    expect(bridgeRecoveryForError(failure)).toEqual({ recovery_action: "correct_execute_arguments", execute_request: "correct_arguments", provider_execution: "not_started", automatic_replay: false });
  });

  /** Missing dynamic user context keeps Engine's exact connection correction attached to the host-owned error. */
  it("preserves the end-user connection selector recovery", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
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
      }),
    );

    let failure: unknown;
    try {
      await remoteCall(options, "jira.searchProjects", {});
    } catch (error) {
      // Retaining the exact error object verifies that untrusted lookalikes cannot claim bridge recovery.
      failure = error;
    }
    expect((failure as Error).message).toContain("Configure X-Fused-End-User-Ref on the MCP client connection");
    expect(bridgeRecoveryForError(failure)).toEqual({ recovery_action: "correct_execute_arguments", execute_request: "correct_arguments", provider_execution: "not_started", automatic_replay: false });
  });

  /** Invalid bridge recovery remains unusable even when its error code and prose are otherwise stable. */
  it("rejects incomplete structured recovery metadata", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({
      ok: false,
      status: 400,
      json: async () => ({
        code: "mcp_pagination_not_supported",
        error: "mcp_pagination_not_supported: omit pagination",
        recovery_action: "correct_execute_arguments",
        execute_request: "correct_arguments",
        provider_execution: "not_started",
      }),
    }));
    let failure: unknown;
    try {
      await remoteCall(options, "gmail.users.messages.get", {});
    } catch (error) {
      // The thrown error is retained only to inspect its trusted identity metadata.
      failure = error;
    }
    expect(bridgeRecoveryForError(failure)).toBeUndefined();
  });

  /** Invalid bridge bodies keep the mutation outcome explicit and never trigger another fetch. */
  it("classifies invalid JSON without retrying", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: false, status: 502, json: async () => { throw new SyntaxError("PRIVATE_PROXY_BODY"); } });
    vi.stubGlobal("fetch", fetchMock);

    await expect(remoteCall(options, "test.op", {})).rejects.toThrow("MCP_BRIDGE_RESPONSE_INVALID");
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  /** Network rejection is outcome-unknown unless the invocation's own abort signal owns cancellation. */
  it("classifies network rejection without leaking raw transport errors or retrying", async () => {
    const fetchMock = vi.fn().mockRejectedValue(new Error("PRIVATE_DESTINATION"));
    vi.stubGlobal("fetch", fetchMock);

    const call = remoteCall(options, "test.op", {});
    await expect(call).rejects.toThrow("MCP_BRIDGE_UNAVAILABLE");
    await expect(call).rejects.not.toThrow("PRIVATE_DESTINATION");
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});
