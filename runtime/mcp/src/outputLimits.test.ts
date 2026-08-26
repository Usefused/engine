import { describe, expect, it } from "vitest";
import {
  DOCUMENTATION_OUTPUT_POLICY,
  EXECUTE_RESULT_OUTPUT_POLICY,
  JsonOutputPolicy,
  serializeBoundedJson,
} from "./outputLimits.js";

/** Creates a tiny policy so boundary behavior can be tested without large fixtures. */
function testPolicy(maxBytes: number): JsonOutputPolicy {
  return {
    maxBytes,
    limitCode: "MCP_TEST_OUTPUT_LIMIT_EXCEEDED",
    subject: "test output",
  };
}

describe("serializeBoundedJson", () => {
  it("admits JSON exactly at the UTF-8 byte limit", () => {
    const value = "é";
    const encoded = JSON.stringify(value);
    const output = serializeBoundedJson(value, testPolicy(Buffer.byteLength(encoded, "utf8")));

    expect(output).toEqual({ text: encoded, isError: false });
  });

  it("returns a stable error code when multi-byte output crosses the limit", () => {
    const rejectedValue = "private-é";
    const output = serializeBoundedJson(rejectedValue, testPolicy(4));

    expect(output.isError).toBe(true);
    expect(JSON.parse(output.text)).toEqual({
      code: "MCP_TEST_OUTPUT_LIMIT_EXCEEDED",
      message: "test output exceeds the 4-byte limit",
      max_bytes: 4,
    });
    expect(output.text).not.toContain(rejectedValue);
  });

  it("publishes distinct hard policies for documentation and execution results", () => {
    expect(DOCUMENTATION_OUTPUT_POLICY).toEqual({
      maxBytes: 64 * 1024,
      limitCode: "MCP_DOCUMENTATION_OUTPUT_LIMIT_EXCEEDED",
      subject: "search_docs output",
    });
    expect(EXECUTE_RESULT_OUTPUT_POLICY).toEqual({
      maxBytes: 1024 * 1024,
      limitCode: "MCP_EXECUTE_RESULT_LIMIT_EXCEEDED",
      subject: "execute result",
    });
  });

  it("rejects absent serialization with a stable machine-readable failure", () => {
    expect(serializeBoundedJson(undefined, testPolicy(100))).toEqual({
      text: JSON.stringify({ code: "MCP_OUTPUT_SERIALIZATION_FAILED", message: "test output is not JSON serializable" }),
      isError: true,
    });
  });

  it("stops before visiting subsequent values once existing text crosses the byte floor", () => {
    let visited = false;
    const output = serializeBoundedJson({
      first: "x".repeat(101),
      // A side-effecting getter proves rejection happens during traversal, not after full serialization.
      get later() { visited = true; return "unreachable"; },
    }, testPolicy(100));

    expect(visited).toBe(false);
    expect(JSON.parse(output.text).code).toBe("MCP_TEST_OUTPUT_LIMIT_EXCEEDED");
  });

  it("checks escaped bytes after conservative preflight admission", () => {
    const output = serializeBoundedJson("\u0000", testPolicy(4));

    expect(JSON.parse(output.text)).toMatchObject({
      code: "MCP_TEST_OUTPUT_LIMIT_EXCEEDED", actual_bytes: 8,
    });
  });

  it("preserves native JSON handling for boxed primitives and omitted object values", () => {
    expect(serializeBoundedJson(new Number(0), testPolicy(1))).toEqual({ text: "0", isError: false });
    expect(serializeBoundedJson({ omitted: undefined }, testPolicy(2))).toEqual({ text: "{}", isError: false });
  });

  it("does not render a hostile value thrown from a serialization hook", () => {
    const output = serializeBoundedJson({
      // Throwing a value with a hostile getter ensures the failure path never inspects that value.
      toJSON() { throw { get message() { throw new Error("must not render"); } }; },
    }, testPolicy(100));

    expect(JSON.parse(output.text).code).toBe("MCP_OUTPUT_SERIALIZATION_FAILED");
  });
});
