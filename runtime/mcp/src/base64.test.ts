import { describe, expect, it } from "vitest";
import { BASE64_MAX_BYTES, boundedAtob, decodeBase64, encodeBase64 } from "./base64.js";
import { runExecute, SessionState } from "./sandbox.js";

/** Covers provider encodings without importing the unrestricted Buffer API into the VM. */
describe("bounded base64 helpers", () => {
  /** Unicode and BOM preservation prevent text corruption while accepting both provider alphabets. */
  it.each(["", "hello", "Résumé 🌍", "\uFEFFtext", "\u083e\u083f"])("round-trips UTF-8 text %j", (text) => {
    expect(decodeBase64(encodeBase64(text))).toBe(text);
    expect(decodeBase64(encodeBase64(text, true))).toBe(text);
  });

  /** HTTP encodings can omit padding or contain ASCII whitespace without changing their bytes. */
  it("accepts unpadded and whitespace-separated base64", () => {
    expect(decodeBase64(" aG\tVsbG8\n")).toBe("hello");
    expect(boundedAtob(" aG\tVsbG8\n")).toBe("hello");
  });

  /** atob is deliberately binary-string compatibility, not a second UTF-8 implementation. */
  it("preserves binary bytes and rejects URL-safe input in atob", () => {
    expect(boundedAtob("/wA=")).toBe("\xff\x00");
    expect(() => decodeBase64("/wA=")).toThrow(/MCP_BASE64_INVALID_UTF8/);
    expect(() => boundedAtob("_wA")).toThrow(/atob requires standard base64/);
  });

  /** Malformed provider data must not be silently truncated by permissive Node decoding. */
  it.each(["A", "aG!VsbG8=", "a===", "aGV=sbG8", "aGVsbG8==", "====", "aGVsbG8=\u00a0"])("rejects malformed input %j", (value) => {
    expect(() => decodeBase64(value)).toThrow(/MCP_BASE64_INVALID/);
    expect(() => boundedAtob(value)).toThrow(/MCP_BASE64_INVALID/);
  });

  /** Both encoded length and decoded bytes are checked at their exact admission boundary. */
  it("accepts the byte ceiling and rejects an extra decoded byte", () => {
    const text = "x".repeat(BASE64_MAX_BYTES);
    const encoded = encodeBase64(text);
    expect(decodeBase64(encoded)).toBe(text);
    expect(boundedAtob(encoded)).toBe(text);
    const oversized = Buffer.from(text + "x").toString("base64");
    expect(() => decodeBase64(oversized)).toThrow(/MCP_BASE64_LIMIT/);
    expect(() => boundedAtob(oversized)).toThrow(/MCP_BASE64_LIMIT/);
    expect(() => encodeBase64(text + "x")).toThrow(/MCP_BASE64_LIMIT/);
  });

  /** UTF-8 byte admission cannot rely only on JavaScript string length. */
  it("bounds multibyte encoding and rejects coercion hooks", () => {
    expect(() => encodeBase64("🌍".repeat(BASE64_MAX_BYTES / 4 + 1))).toThrow(/MCP_BASE64_LIMIT/);
    expect(() => decodeBase64({ toString() { throw new Error("should not run"); } } as unknown as string)).toThrow(/expected a string/);
    expect(() => encodeBase64("ok", "yes" as unknown as boolean)).toThrow(/urlSafe must be a boolean/);
  });

  /** Public helpers should be usable from execute while full Node capabilities remain absent. */
  it("exposes string-only helpers in the sandbox", async () => {
    const outcome = await runExecute('return {text:decodeBase64(encodeBase64("Résumé 🌍", true)), binary:atob("/w==").charCodeAt(0), buffer:typeof Buffer};', { sessionId: "synthetic", enginePort: "0" }, new SessionState());
    expect(outcome.isError).toBe(false);
    expect(JSON.parse(outcome.text)).toEqual({ text: "Résumé 🌍", binary: 255, buffer: "undefined" });
  });
});
