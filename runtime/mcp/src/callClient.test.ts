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
});
