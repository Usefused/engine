import { afterEach, describe, expect, it, vi } from "vitest";
import { callClientOptionsFromEnv, remoteCall } from "./callClient.js";

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
