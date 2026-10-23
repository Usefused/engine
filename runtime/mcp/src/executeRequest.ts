import { z } from "zod";
import { CallClientOptions } from "./callClient.js";
import { CallImpl, DEFAULT_EXECUTE_LIMITS, ExecuteLimits, runExecute, SessionState } from "./sandbox.js";
import { EXECUTE_INPUT_SHAPE } from "./executeInput.js";

const requestSchema = z.object(EXECUTE_INPUT_SHAPE).strict();

/** Compiles a single-operation request into the same bounded, authorized call path as an authored script. */
export async function runExecuteRequest(
  input: unknown,
  options: CallClientOptions,
  session: SessionState,
  limits: ExecuteLimits = DEFAULT_EXECUTE_LIMITS,
  callImpl?: CallImpl,
  signal?: AbortSignal,
) {
  const parsed = requestSchema.safeParse(input);
  // Shape errors must never dispatch a provider call or echo arbitrary argument values.
  if (!parsed.success) return rejectRequest("Invalid execute arguments. Use operationId and params, or script.", options, session, limits, callImpl, signal);
  const args = parsed.data;
  const direct = args.operationId !== undefined;
  // Exactly one execution mode prevents silently ignoring a script or issuing an unintended call.
  if (direct === (args.script !== undefined)) return rejectRequest("Choose operationId with params, or script; not both.", options, session, limits, callImpl, signal);
  // Direct-only controls cannot be silently ignored on the scripting path.
  if (!direct && (args.params !== undefined || args.select !== undefined || args.saveAs !== undefined)) return rejectRequest("params, select and saveAs require operationId.", options, session, limits, callImpl, signal);
  // Reserved snapshot names fail before execution rather than after a provider mutation has completed.
  if (args.saveAs?.startsWith("fused-result:")) return rejectRequest("saveAs must be a named key, not a result_ref.", options, session, limits, callImpl, signal);
  const script = direct ? directScript(args) : args.script!;
  return runExecute(script, options, session, { ...limits, outputBudgetBytes: args.outputBudgetBytes ?? limits.outputBudgetBytes }, callImpl, signal);
}

/** Literal JSON encoding keeps operation IDs, field names and values from becoming executable source. */
function directScript(args: z.infer<typeof requestSchema>): string {
  // Parsing a JSON string preserves literal __proto__ keys instead of applying object-literal prototype semantics.
  const lines = [`const result = await call(${JSON.stringify(args.operationId)}, JSON.parse(${JSON.stringify(JSON.stringify(args.params ?? {}))}));`];
  // Save the full result before projection so follow-up questions need no repeated provider request.
  if (args.saveAs !== undefined) lines.push(`session.set(${JSON.stringify(args.saveAs)}, result);`);
  // Projection changes only visible fields; provider parameters and the saved value remain intact.
  if (args.select !== undefined) {
    lines.push(`const fields = ${JSON.stringify(args.select)};`,
      'const pick = value => value !== null && typeof value === "object" && !Array.isArray(value) ? Object.fromEntries(fields.filter(key => Object.prototype.hasOwnProperty.call(value, key)).map(key => [key, value[key]])) : value;',
      "return Array.isArray(result) ? result.map(pick) : pick(result);");
  } else {
    lines.push("return result;");
  }
  return lines.join("\n");
}

/** Local admission failures reuse the bounded error contract with no bridge activity. */
function rejectRequest(message: string, options: CallClientOptions, session: SessionState, limits: ExecuteLimits, callImpl?: CallImpl, signal?: AbortSignal) {
  return runExecute(`throw new Error(${JSON.stringify("MCP_EXECUTE_ARGUMENTS_INVALID: " + message)});`, options, session, limits, callImpl, signal);
}
