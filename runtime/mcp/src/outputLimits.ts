/** Describes one immutable MCP JSON output budget and its stable failure code. */
export interface JsonOutputPolicy {
  readonly maxBytes: number;
  readonly limitCode: string;
  readonly subject: string;
}

/** Keeps operation documentation useful while preventing one schema from dominating a session. */
export const DOCUMENTATION_OUTPUT_POLICY: JsonOutputPolicy = Object.freeze({
  maxBytes: 64 * 1024,
  limitCode: "MCP_DOCUMENTATION_OUTPUT_LIMIT_EXCEEDED",
  subject: "search_docs output",
});

/** Allows practical API results while bounding data returned across the MCP process boundary. */
export const EXECUTE_RESULT_OUTPUT_POLICY: JsonOutputPolicy = Object.freeze({
  maxBytes: 1024 * 1024,
  limitCode: "MCP_EXECUTE_RESULT_LIMIT_EXCEEDED",
  subject: "execute result",
});

/** Carries either admitted JSON or a bounded, machine-readable limit failure. */
export interface SerializedJsonOutput {
  text: string;
  isError: boolean;
}

/** Tracks a conservative byte floor while native JSON serialization visits values. */
interface OutputBudget {
  bytes: number;
  root: boolean;
  readonly exceeded: object;
}

/** Rejects expanding values before escaping allocation, then checks the exact UTF-8 output size. */
export function serializeBoundedJson(value: unknown, policy: JsonOutputPolicy): SerializedJsonOutput {
  const budget: OutputBudget = { bytes: 0, root: true, exceeded: {} };
  try {
    const text = JSON.stringify(value, boundedReplacer(policy, budget));
    // Unsupported root values must return an explicit failure instead of
    // disappearing from the MCP response's required text field.
    if (text === undefined) {
      return serializationFailure(policy);
    }
    const actualBytes = Buffer.byteLength(text, "utf8");
    // Escaping adds bytes beyond the conservative preflight floor, so exact
    // encoded size remains the final admission authority.
    if (actualBytes > policy.maxBytes) {
      return outputLimitFailure(policy, actualBytes);
    }
    return { text, isError: false };
  } catch (error) {
    // Identity comparison cannot invoke an untrusted thrown object's getters.
    if (error === budget.exceeded) {
      return outputLimitFailure(policy);
    }
    return serializationFailure(policy);
  }
}

/** Charges a safe byte floor before native stringify allocates escaped primitive output. */
function boundedReplacer(policy: JsonOutputPolicy, budget: OutputBudget) {
  /** Visits one native JSON value without reading additional user-controlled properties. */
  return function chargeValue(this: unknown, key: string, value: unknown): unknown {
    const arrayItem = Array.isArray(this);
    const valueBytes = minimumValueBytes(value, arrayItem);
    // Omitted object values have no wire key, unlike array holes which encode as null.
    const keyBytes = !budget.root && !arrayItem && valueBytes > 0 ? Buffer.byteLength(key, "utf8") + 3 : 0;
    budget.root = false;
    budget.bytes += valueBytes + keyBytes;
    // The floor omits escaping and commas, so crossing it proves the exact
    // result cannot fit without rejecting otherwise-admissible JSON.
    if (budget.bytes > policy.maxBytes) {
      throw budget.exceeded;
    }
    return value;
  };
}

/** Counts only unavoidable JSON bytes; optional escaping is checked after encoding. */
function minimumValueBytes(value: unknown, arrayItem: boolean): number {
  // JSON null is a literal in both object and array positions.
  if (value === null) {
    return 4;
  }
  // Native stringify handles coercion; this preflight must not invoke getters or toString itself.
  switch (typeof value) {
    case "string": return Buffer.byteLength(value, "utf8") + 2;
    case "number": return JSON.stringify(value).length;
    case "boolean": return value ? 4 : 5;
    // Boxed numbers may encode as one digit, while ordinary containers need at least two delimiters.
    case "object": return 1;
    default: return arrayItem ? 4 : 0;
  }
}

/** Builds a small stable failure without echoing any rejected output bytes. */
function outputLimitFailure(policy: JsonOutputPolicy, actualBytes?: number): SerializedJsonOutput {
  return {
    text: JSON.stringify({
      code: policy.limitCode,
      message: `${policy.subject} exceeds the ${policy.maxBytes}-byte limit`,
      max_bytes: policy.maxBytes,
      actual_bytes: actualBytes,
    }),
    isError: true,
  };
}

/** Keeps malformed or hostile JSON results machine-readable without rendering thrown values. */
function serializationFailure(policy: JsonOutputPolicy): SerializedJsonOutput {
  return {
    text: JSON.stringify({
      code: "MCP_OUTPUT_SERIALIZATION_FAILED",
      message: `${policy.subject} is not JSON serializable`,
    }),
    isError: true,
  };
}
