import Ajv from "ajv";
import Ajv2020 from "ajv/dist/2020.js";
import type { ErrorObject, ValidateFunction } from "ajv";

export interface InputIssue { path: string; message: string }
export interface InputValidation { parsed: boolean; value?: unknown; issues: InputIssue[]; warning?: string }
export interface InputContractValidator { validate?: ValidateFunction; warning?: string }

/** Compiles the active contract without fetching references, coercing types, adding defaults, or deleting keys. */
export function compileInputContract(schema: unknown): InputContractValidator {
  try {
    // An absent contract is different from an unrestricted boolean schema.
    if (schema === null || schema === undefined) return { warning: "Schema unavailable. Engine will validate when enabling this schedule." };
    const dialect = typeof schema === "object" ? (schema as Record<string, unknown>).$schema : undefined;
    // Engine uses modern JSON Schema; explicitly declared draft-07 contracts retain their older semantics.
    const Validator = typeof dialect === "string" && dialect.includes("draft-07") ? Ajv : Ajv2020;
    // Finite numbers stay mandatory even with permissive schema keywords; JSON serialization cannot preserve Infinity.
    const validator = new Validator({ allErrors: true, strict: false, strictNumbers: true, validateFormats: false, coerceTypes: false, useDefaults: false, removeAdditional: false });
    // Asynchronous contracts cannot safely participate in native synchronous form validity.
    if (typeof schema === "object" && (schema as Record<string, unknown>).$async) throw new Error("Async schema");
    const validate = validator.compile(schema as Record<string, unknown> | boolean);
    return { validate };
  } catch {
    // Compilation failures must never show a green validation result or discard the user's input.
    return { warning: "This schema cannot be checked in the browser. Engine will validate when enabling this schedule." };
  }
}

/** Keeps errors actionable by naming JSON Pointer locations without echoing potentially private input values. */
function inputIssue(error: ErrorObject): InputIssue {
  let path = error.instancePath;
  const property = error.params.missingProperty ?? error.params.additionalProperty;
  // Required and extra-property errors originate at the parent; point directly at the affected member.
  if (typeof property === "string") path += `/${property.replace(/~/g, "~0").replace(/\//g, "~1")}`;
  return { path: path || "/", message: error.keyword === "required" ? "is required" : error.message ?? "does not match the input schema" };
}

/** Separates syntax from contract errors and leaves the exact draft untouched for correction. */
export function validateInputText(text: string, contract: InputContractValidator): InputValidation {
  let value: unknown;
  try {
    // Reject numeric overflow in any property before formatting could turn it into null.
    value = JSON.parse(text, (_key, item) => {
      if (typeof item === "number" && !Number.isFinite(item)) throw new RangeError("Number outside supported range");
      return item;
    });
  }
  catch (cause) {
    // Numbers outside the finite range cannot be preserved by the editor or save request.
    if (cause instanceof RangeError) return { parsed: false, issues: [{ path: "/", message: "A number is outside the supported range. Use a finite JSON number." }] };
    // Only parser coordinates cross into the UI; parser messages can include private snippets of input.
    const location = cause instanceof Error ? cause.message.match(/(?:line \d+ column \d+|position \d+)/)?.[0] : undefined;
    return { parsed: false, issues: [{ path: "/", message: `Invalid JSON${location ? ` at ${location}` : ""}. Check quotes, commas, and brackets.` }] };
  }
  // A valid document is not necessarily schema-checked when contract loading or compilation failed.
  if (!contract.validate) return { parsed: true, value, issues: [], warning: contract.warning };
  try {
    const valid = contract.validate(value);
    return { parsed: true, value, issues: valid ? [] : (contract.validate.errors ?? []).slice(0, 8).map(inputIssue) };
  } catch {
    // Recursive validation limits must not crash editing or imply a successful schema check.
    return { parsed: true, value, issues: [], warning: "This input could not be checked in the browser. Engine will validate when enabling this schedule." };
  }
}
