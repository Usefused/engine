/** Result references are intentionally shorter-lived than credentials or active client conversations. */
export const SESSION_RESULT_TTL_MS = 5 * 60 * 1000;

/** Keeps model recovery choices aligned with Engine transport failures. */
export type RecoveryAction =
  | "continue_stored_result"
  | "correct_execute_arguments"
  | "adjust_result_projection"
  | "reinitialize_connection"
  | "do_not_replay";

export type ExecuteRequestAction =
  | "use_next_request"
  | "correct_arguments"
  | "adjust_projection"
  | "reformat_if_session_state_used"
  | "do_not_replay";

export type ProviderExecutionState = "not_started" | "complete" | "unknown";

/** One compact public recovery shape replaces transport and audit details. */
export interface ModelRecovery {
  recovery_action: RecoveryAction;
  execute_request: ExecuteRequestAction;
  provider_execution: ProviderExecutionState;
  automatic_replay: false;
}

/** Gives capable MCP hosts a bounded, non-secret description of the script session contract. */
export const SESSION_CONTRACT_METADATA = Object.freeze({
  schema_version: 1,
  transport_session: "client_managed",
  session_id_input: false,
  script_scope: "current_mcp_connection",
  script_apis: Object.freeze(["session.get", "session.set", "session.page"]),
  result_ttl_ms: SESSION_RESULT_TTL_MS,
  automatic_execute_replay: false,
  recovery_actions: Object.freeze<RecoveryAction[]>([
    "continue_stored_result",
    "correct_execute_arguments",
    "adjust_result_projection",
    "reinitialize_connection",
    "do_not_replay",
  ]),
});

/** Repeats the safety-critical contract in model-visible text because clients may hide initialize metadata. */
export const SESSION_AGENT_RULE =
  "The MCP client has already attached every execute call to one active client-owned session. Never invent, request, or pass a session ID. Use execute for session.get, session.set, and session.page; their state exists only on this MCP connection. Follow recovery_action and execute_request exactly: correct arguments, adjust a result projection, run the supplied stored-result request, let the client reconnect, or do not replay an unknown outcome. A new connection cannot read prior state or result_ref values.";

export interface SessionToolRequest {
  tool: "execute";
  arguments: { script: string; outputBudgetBytes: number };
}

export interface SessionContinuation extends ModelRecovery {
  recovery_action: "continue_stored_result";
  execute_request: "use_next_request";
  provider_execution: "complete";
  automatic_replay: false;
  session: {
    scope: "current_mcp_connection";
    same_session_required: true;
  };
  next_request: SessionToolRequest;
}

/** Builds one directly executable retained read without exposing transport session identity. */
export function retainedContinuation(script: string, outputBudgetBytes: number): SessionContinuation {
  return {
    recovery_action: "continue_stored_result",
    execute_request: "use_next_request",
    provider_execution: "complete",
    automatic_replay: false,
    session: { scope: "current_mcp_connection", same_session_required: true },
    next_request: executeRequest(script, outputBudgetBytes),
  };
}

/** Builds the exact public execute request used by retained result navigation. */
export function executeRequest(script: string, outputBudgetBytes: number): SessionToolRequest {
  return { tool: "execute", arguments: { script, outputBudgetBytes } };
}

/** Quotes every retained selector as data so provider-authored keys cannot become script. */
export function pageScript(reference: string, path: string, fields: string[] | undefined, offset: number): string {
  const fieldArgument = fields === undefined ? "" : `,fields:${JSON.stringify(fields)}`;
  return `return session.page(${JSON.stringify(reference)}, {path:${JSON.stringify(path)}${fieldArgument},offset:${offset}});`;
}

/** Produces a compact, bounded inspection when no whole retained row can be paged safely. */
export function inspectionScript(reference: string, root: unknown, outputBudgetBytes: number): string {
  // String slices reserve ample room for JSON escaping and the closed action wrapper.
  if (typeof root === "string") {
    const characters = Math.max(8, Math.floor((outputBudgetBytes - 256) / 6));
    return `const v=session.get(${JSON.stringify(reference)});return {recovery_action:"adjust_result_projection",execute_request:"adjust_projection",provider_execution:"complete",automatic_replay:false,type:"string",length:v.length,value:v.slice(0,${characters})};`;
  }
  // Arrays with rows too large to page still return progress information without sampling private values.
  if (Array.isArray(root)) {
    return `const v=session.get(${JSON.stringify(reference)});return {recovery_action:"adjust_result_projection",execute_request:"adjust_projection",provider_execution:"complete",automatic_replay:false,type:"array",count:v.length,first_type:v.length?(Array.isArray(v[0])?"array":v[0]===null?"null":typeof v[0]):null};`;
  }
  // Bounded key fragments orient a correction without allowing hostile names to overflow the next result.
  if (root !== null && typeof root === "object") {
    return `const v=session.get(${JSON.stringify(reference)}),k=Object.keys(v);return {recovery_action:"adjust_result_projection",execute_request:"adjust_projection",provider_execution:"complete",automatic_replay:false,type:"object",count:k.length,keys:k.slice(0,2).map(x=>({name:x.slice(0,16),complete:x.length<=16}))};`;
  }
  return `return {recovery_action:"adjust_result_projection",execute_request:"adjust_projection",provider_execution:"complete",automatic_replay:false,type:typeof session.get(${JSON.stringify(reference)})};`;
}

/** Maps trusted runtime codes to the smallest safe model recovery contract. */
export function recoveryForError(message: string): ModelRecovery {
  const code = /^([A-Z][A-Z0-9_]+):/.exec(message)?.[1] ?? "";
  // A lost bridge session requires client transport work, never a model-authored identifier.
  if (code === "MCP_BRIDGE_SESSION_UNAVAILABLE") {
    return recovery("reinitialize_connection", "reformat_if_session_state_used", "not_started");
  }
  // An expired or evicted snapshot cannot be repaired by changing a selector on the missing value.
  if (code === "MCP_RESULT_UNAVAILABLE") {
    return recovery("do_not_replay", "do_not_replay", "complete");
  }
  // Output and retained-navigation failures require a new session-only projection, not provider replay.
  if (code.startsWith("MCP_RESULT_") || ["MCP_SESSION_GET_ARGUMENTS_INVALID", "MCP_OUTPUT_SERIALIZATION_FAILED", "MCP_EXECUTE_RESULT_LIMIT_EXCEEDED", "MCP_EXECUTE_VISIBLE_OUTPUT_LIMIT_EXCEEDED", "MCP_EXECUTE_ERROR_OUTPUT_LIMIT_EXCEEDED"].includes(code)) {
    return recovery("adjust_result_projection", "adjust_projection", "complete");
  }
  // Only the outer output-budget guard is known to reject before the VM can dispatch a provider call.
  if (code === "MCP_OUTPUT_BUDGET_INVALID") {
    return recovery("correct_execute_arguments", "correct_arguments", "not_started");
  }
  return recovery("do_not_replay", "do_not_replay", "unknown");
}

/** Constructs closed recovery values without transport/session diagnostics. */
function recovery(recoveryAction: RecoveryAction, executeRequest: ExecuteRequestAction, providerExecution: ProviderExecutionState): ModelRecovery {
  return { recovery_action: recoveryAction, execute_request: executeRequest, provider_execution: providerExecution, automatic_replay: false };
}
