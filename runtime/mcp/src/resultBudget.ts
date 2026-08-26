import { JsonOutputPolicy } from "./outputLimits.js";

/** Byte policy is client-configurable, not a claim about an unknown model's tokenizer. */
export const EXECUTE_INLINE_BYTES = 16 * 1024;
export const EXECUTE_MIN_OUTPUT_BYTES = 1024;
export const EXECUTE_VISIBLE_OUTPUT_POLICY: JsonOutputPolicy = Object.freeze({
  maxBytes: 64 * 1024,
  limitCode: "MCP_EXECUTE_VISIBLE_OUTPUT_LIMIT_EXCEEDED",
  subject: "execute visible output",
});

/** Rejects invalid policy before execution so a bad budget cannot cause a provider side effect. */
export function executeOutputBudget(value: unknown = EXECUTE_INLINE_BYTES): number {
  // Accept only integer bytes within the compiled ceiling; coercion could silently enlarge a client limit.
  if (typeof value !== "number" || !Number.isInteger(value) || value < EXECUTE_MIN_OUTPUT_BYTES || value > EXECUTE_VISIBLE_OUTPUT_POLICY.maxBytes) {
    throw new Error("MCP_OUTPUT_BUDGET_INVALID: outputBudgetBytes must be an integer from 1024 to 65536.");
  }
  return value;
}
