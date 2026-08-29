import vm from "node:vm";
import { BridgeAuthAction, bridgeAuthActionForError, bridgeRecoveryForError, CallClientOptions, PhysicalCallOptions, remoteCall } from "./callClient.js";
import { SerializedJsonOutput, serializeBoundedJson } from "./outputLimits.js";
import { DeliveredResult, EXECUTE_ERROR_OUTPUT_POLICY, isResultReference, RetainedResults } from "./retainedResults.js";
import { executeOutputBudget, EXECUTE_INLINE_BYTES } from "./resultBudget.js";
import { pageResult, ResultPageOptions } from "./resultPaging.js";
import { Invocation, ExecutionOutcome } from "./invocation.js";
import { boundedAtob, decodeBase64, encodeBase64 } from "./base64.js";
import { ModelRecovery, modelVisibleExecuteErrorCode, recoveryForError } from "./sessionContract.js";

export interface ExecuteLimits {
  timeoutMs: number;
  maxCalls: number;
  outputBudgetBytes?: number;
}

export const DEFAULT_EXECUTE_LIMITS: ExecuteLimits = {
  timeoutMs: 30_000,
  maxCalls: 10,
};

const PHYSICAL_PAGINATION_CORRECTION = "Use {pagination:{maxPages:N}} only when search_docs exact operationId detail reports pagination.caller_bound_supported=true for this operation; otherwise omit the third argument and use call(operationId, params). Never reuse another operation's bound.";

/** Exposes only admitted text so no caller can accidentally serialize an untrusted result again. */
export interface ExecuteOutcome extends DeliveredResult {
  access: ResultAccess;
  outputBudgetBytes: number;
  executionOutcome: ExecutionOutcome;
  authAction?: ExecuteAuthAction;
}

/** ExecuteAuthAction combines one trusted URL with aggregate script replay safety. */
export interface ExecuteAuthAction extends BridgeAuthAction {
  recovery: ModelRecovery;
}

/** Tracks only bounded retrieval counts; references and result values never become audit metadata. */
interface ResultAccess {
  retained_reads: number;
  unavailable_reads: number;
}

/** Counts only admitted bridge attempts so outer recovery can conservatively account for sibling calls. */
interface CallExecutionHistory {
  bridge_calls_started: number;
}

/**
 * One SessionState belongs to one MCP process, while each invocation gets a
 * fresh VM realm. Explicit script state keeps its existing API; automatic
 * result snapshots have independent byte/count/expiry bounds. Neither store
 * survives session closure or crosses a process/session boundary.
 */
export class SessionState {
  private readonly store = new Map<string, unknown>();
  private readonly results = new RetainedResults();

  /** Resolves retained results through the existing session API without a new provider invocation. */
  get(key: string, access?: ResultAccess): unknown {
    // Only the reserved namespace has expiry semantics; ordinary script state keeps its existing behavior.
    if (isResultReference(key)) {
      return this.readResult(key, access);
    }
    return this.store.get(key);
  }

  /** Preserves explicit script state without allowing it to overwrite runtime-owned result references. */
  set(key: string, value: unknown): void {
    // Runtime snapshots must remain immutable even when a script guesses their namespace.
    if (isResultReference(key)) {
      throw new Error("MCP_RESULT_REFERENCE_RESERVED: Retained result references cannot be overwritten.");
    }
    this.store.set(key, value);
  }

  /** Uses the invocation's budget for delivery as well as paging to prevent re-retaining a correctly sized page. */
  deliver(value: unknown, maxBytes = EXECUTE_INLINE_BYTES): DeliveredResult {
    return this.results.deliver(value, maxBytes);
  }

  /** Retrieves only immutable retained snapshots, sharing the existing payload-free read audit. */
  page(key: string, options: ResultPageOptions = {}, maxBytes = EXECUTE_INLINE_BYTES, access?: ResultAccess): unknown {
    executeOutputBudget(maxBytes);
    // Arbitrary session.set values may contain hooks and are not admitted result snapshots.
    if (!isResultReference(key)) throw new Error("MCP_RESULT_REFERENCE_INVALID: session.page requires a retained result reference.");
    return pageResult(this.readResult(key, access), key, options, maxBytes);
  }

  /** Counts retained reads without retaining IDs or confusing an unavailable result with JSON null. */
  private readResult(key: string, access?: ResultAccess): unknown {
    // Direct host tests have no invocation observation, while sandbox reads always supply one.
    if (access) {
      access.retained_reads = Math.min(access.retained_reads + 1, 100_000);
    }
    try {
      return this.results.get(key);
    } catch (error) {
      // The bounded unavailable count is enough to debug expiry without exposing a reference.
      if (access) {
        access.unavailable_reads = Math.min(access.unavailable_reads + 1, 100_000);
      }
      throw error;
    }
  }
}

/** The shape of "make one call() round trip" -- defaults to remoteCall's
 * real HTTP transport. Overridable so tests can exercise the sandbox's
 * allowlist/timeout/call-count behavior without a live Engine to talk to;
 * production code never passes this parameter. */
export type CallImpl = (
  options: CallClientOptions,
  operationId: string,
  params: Record<string, unknown>,
  signal?: AbortSignal,
  callOptions?: PhysicalCallOptions,
) => Promise<unknown>;

/**
 * runExecute runs one untrusted script in a fresh vm realm and returns an
 * admitted JSON result or bounded error text. Invocation-owned capabilities are revoked
 * on every exit; Promise timeout alone cannot stop a suspended script's later side effects.
 */
export async function runExecute(
  script: string,
  callOptions: CallClientOptions,
  session: SessionState,
  limits: ExecuteLimits = DEFAULT_EXECUTE_LIMITS,
  callImpl: CallImpl = remoteCall,
  signal?: AbortSignal,
): Promise<ExecuteOutcome> {
  const invocation = new Invocation(limits.timeoutMs, signal);
  const deadline = invocation.deadline;
  const callHistory: CallExecutionHistory = { bridge_calls_started: 0 };
  const boundCall = buildBoundCall(callOptions, limits.maxCalls, callImpl, invocation, callHistory);
  const access: ResultAccess = { retained_reads: 0, unavailable_reads: 0 };
  const wrapped = `(async () => {\n${script}\n})()`;
  let outputBudget = EXECUTE_INLINE_BYTES;

  try {
    invocation.assertActive();
    outputBudget = executeOutputBudget(limits.outputBudgetBytes);
    const context = vm.createContext(buildSandboxGlobals(boundCall, session, access, outputBudget, invocation));
    const compiled = new vm.Script(wrapped, { filename: "execute.ts" });
    // vm's own `timeout` guards one specific failure mode: a synchronous,
    // CPU-bound infinite loop (e.g. `while (true) {}`) in the script's
    // pre-first-await code, which would otherwise block this single-
    // threaded process's event loop entirely -- including the ability to
    // run the invocation watchdog callback that cancellation relies
    // on. Async capabilities therefore share a separate cancellable invocation.
    const resultPromise = compiled.runInContext(context, { timeout: Math.max(1, deadline - Date.now()) }) as Promise<unknown>;
    const result = await invocation.wait(resultPromise);
    invocation.assertActive();
    // Serialization is synchronous and shares the deadline; finally revokes authority before yielding again.
    const output = withinSerializationDeadline(() => session.deliver(result ?? null, outputBudget), deadline);
    invocation.assertActive();
    return { ...output, access, outputBudgetBytes: outputBudget, executionOutcome: invocation.outcome(output.isError) };
  } catch (err) {
    // Error formatting must not leave callbacks or provider requests running in the background.
    invocation.close();
    const authAction = executeAuthAction(err, callHistory);
    return { ...serializeExecuteError(err, deadline, limits.timeoutMs, outputBudget, callHistory), delivery: "error", access, outputBudgetBytes: outputBudget, executionOutcome: invocation.outcome(true), ...(authAction ? { authAction } : {}) };
  } finally {
    invocation.close();
  }
}

/** Retains a trusted browser URL while downgrading replay safety for earlier sibling calls. */
function executeAuthAction(error: unknown, callHistory: CallExecutionHistory): ExecuteAuthAction | undefined {
  const action = bridgeAuthActionForError(error);
  // Script-thrown errors and malformed bridge responses cannot create URL elicitation.
  if (!action) return undefined;
  const recovery = executionAwareRecovery(error, "", callHistory);
  // Browser completion remains the next user action even when prior provider work forbids replaying the execute script.
  return { ...action, recovery: { ...recovery, recovery_action: "complete_authentication" } };
}

/** Executes all user-provided serialization hooks under the invocation's remaining deadline. */
function withinSerializationDeadline<T extends SerializedJsonOutput>(serialize: () => T, deadline: number): T {
  const remainingMs = deadline - Date.now();
  // An exhausted execution cannot acquire another timeout window while formatting its result.
  if (remainingMs <= 0) {
    throw new Error("execute serialization deadline exceeded");
  }
  const context = vm.createContext({ serialize });
  return new vm.Script("serialize()", { filename: "execute-output.js" }).runInContext(context, { timeout: remainingMs }) as T;
}

/** Renders hostile error hooks inside the deadline and honors smaller client output budgets. */
function serializeExecuteError(error: unknown, deadline: number, timeoutMs: number, maxBytes: number, callHistory: CallExecutionHistory): SerializedJsonOutput {
  try {
    return withinSerializationDeadline(() => boundedExecuteError(error, describeError(error), maxBytes, callHistory), deadline);
  } catch {
    // Failure to render an error must never recurse through its getters or toString outside the VM.
    const timedOut = Date.now() >= deadline;
    return {
      text: JSON.stringify({
        code: timedOut ? "MCP_EXECUTE_TIMEOUT" : "MCP_EXECUTE_ERROR_SERIALIZATION_FAILED",
        message: timedOut ? `execute timed out after ${timeoutMs}ms` : "execute error could not be rendered safely",
        ...recoveryForError(timedOut ? "MCP_EXECUTE_TIMEOUT:" : "MCP_EXECUTE_ERROR_SERIALIZATION_FAILED:"),
      }),
      isError: true,
    };
  }
}

/** Errors cannot be paged, so the smaller of the error policy and invocation budget is authoritative. */
function boundedExecuteError(error: unknown, message: string, maxBytes: number, callHistory: CallExecutionHistory): SerializedJsonOutput {
  const policy = { ...EXECUTE_ERROR_OUTPUT_POLICY, maxBytes: Math.min(maxBytes, EXECUTE_ERROR_OUTPUT_POLICY.maxBytes) };
  const output = serializeBoundedJson({
    code: modelVisibleExecuteErrorCode(message),
    message,
    ...executionAwareRecovery(error, message, callHistory),
  }, policy);
  // Oversized hostile messages collapse to the fixed limit failure plus one closed recovery action.
  if (output.isError) {
    const failure = JSON.parse(output.text) as Record<string, unknown>;
    return { text: JSON.stringify({ ...failure, ...recoveryForError("MCP_EXECUTE_ERROR_OUTPUT_LIMIT_EXCEEDED:") }), isError: true };
  }
  return { ...output, isError: true };
}

/** Uses trusted bridge recovery only when invocation history proves no sibling call may have dispatched. */
function executionAwareRecovery(error: unknown, message: string, callHistory: CallExecutionHistory): ModelRecovery {
  const bridgeRecovery = bridgeRecoveryForError(error);
  const candidate = bridgeRecovery ?? recoveryForError(message);
  // Outcomes other than not-started do not make a claim that sibling provider work was absent.
  if (candidate.provider_execution !== "not_started") return candidate;
  const safeBridgeCalls = bridgeRecovery ? 1 : 0;
  // A bridge-proven rejection owns one attempt; local validation must precede every bridge attempt.
  if (callHistory.bridge_calls_started <= safeBridgeCalls) return candidate;
  return recoveryForError("MCP_EXECUTE_FAILED:");
}

/**
 * describeError extracts a message from a thrown value, without relying on
 * `instanceof Error`. An error thrown inside the vm sandbox is an instance
 * of *that context's own* Error constructor, not this file's -- each vm
 * realm has its own distinct built-ins, so `instanceof Error` silently
 * fails across the sandbox boundary even for a completely ordinary
 * `throw new Error(...)`. Duck-typing on `.message` works regardless of
 * which realm the value came from.
 */
function describeError(err: unknown): string {
  // Cross-realm errors are identified structurally, and the caller bounds any property hooks in a VM.
  if (typeof err === "object" && err !== null && "message" in err) {
    const message = (err as { message: unknown }).message;
    // Reading once prevents a stateful getter from changing an admitted string into an object.
    if (typeof message === "string") {
      return message;
    }
  }
  return String(err);
}

/**
 * buildBoundCall checks lifetime before dispatch and after resolution so even an
 * abort-ignoring transport cannot release a stale response into another operation.
 */
function buildBoundCall(callOptions: CallClientOptions, maxCalls: number, callImpl: CallImpl, invocation: Invocation, callHistory: CallExecutionHistory) {
  let callCount = 0;
  // Observe fire-and-forget rejections as well; callers still receive the original rejected promise.
  return (operationId: string, params: Record<string, unknown> = {}, rawOptions?: unknown): Promise<unknown> => {
    // Keep validation inside a promise so call() consistently supports await/catch.
    const pending = (async () => {
      invocation.assertActive();
      const physicalOptions = physicalCallOptions(rawOptions);
      callCount++;
      // Only admitted calls consume transport resources, regardless of script error handling.
      if (callCount > maxCalls) throw new Error(`call() limit exceeded (max ${maxCalls} per execute invocation)`);
      // Starting the trusted transport is enough to make any sibling error's aggregate provider outcome uncertain.
      callHistory.bridge_calls_started = Math.min(callHistory.bridge_calls_started + 1, maxCalls);
      const result = await invocation.wait(callImpl(callOptions, operationId, params, invocation.signal, physicalOptions));
      invocation.assertActive();
      return result;
    })();
    // Cleanup may abort a request the script deliberately did not await.
    void pending.catch(() => {});
    return pending;
  };
}

/** physicalCallOptions rejects ambiguous controls before any provider transport is acquired. */
function physicalCallOptions(value: unknown): PhysicalCallOptions | undefined {
  // Absence retains the operation's reviewed automatic pagination policy.
  if (value === undefined) return undefined;
  // A single named control prevents provider inputs or future options from being silently ignored.
  if (!isPlainRecord(value) || Object.keys(value).length !== 1 || !("pagination" in value)) {
    throw new Error(`MCP_CALL_OPTIONS_INVALID: ${PHYSICAL_PAGINATION_CORRECTION}`);
  }
  const pagination = value.pagination;
  // The nested shape stays distinct from provider page-size parameters such as Gmail maxResults.
  if (!isPlainRecord(pagination) || Object.keys(pagination).length !== 1 || !("maxPages" in pagination)) {
    throw new Error(`MCP_CALL_PAGINATION_INVALID: pagination must contain only maxPages. ${PHYSICAL_PAGINATION_CORRECTION}`);
  }
  const maxPages = pagination.maxPages;
  // Exact integer admission avoids coercing model-authored values into a different execution intent.
  if (typeof maxPages !== "number" || !Number.isSafeInteger(maxPages) || maxPages < 1) {
    throw new Error(`MCP_CALL_PAGINATION_INVALID: maxPages must be a positive safe integer. ${PHYSICAL_PAGINATION_CORRECTION}`);
  }
  return { pagination: { maxPages } };
}

/** isPlainRecord excludes null and arrays while accepting objects created inside the VM realm. */
function isPlainRecord(value: unknown): value is Record<string, unknown> {
  // Cross-realm objects have a distinct Object prototype, so structural admission is required here.
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/**
 * buildSandboxGlobals is an explicit allowlist, built from scratch, not a
 * copy of the outer global scope with a few dangerous things deleted. That
 * distinction matters: an allowlist can't accidentally leak a capability
 * that gets added to Node's globals later; a denylist can. Only `call`,
 * `session`, `console`, managed delays, and bounded decoding helpers are placed here --
 * everything else the script sees (JSON, Math, Array, Object, Promise,
 * Function, ...) is inherent to any JS realm and was never something this
 * function granted or could withhold. Timers are invocation-owned and decoding
 * helpers return bounded strings, never Node Buffer objects. Nothing with an I/O path (fetch,
 * process, require, http, net, fs, child_process, dns) is ever added.
 *
 * Caveat, stated plainly rather than implied: this allowlist reduces the
 * *convenient* attack surface, it is not itself the security boundary. The
 * injected `call`/`session` function references were created in the outer
 * (trusted) realm, so their prototype chains still lead back to that
 * realm's Function/Object constructors -- a sufficiently sophisticated
 * script could in principle pivot through that chain (this is the
 * well-documented reason `vm` escapes exist and don't require
 * sophistication to find). The real boundary is the OS process (one per
 * session, never pooled). Provider credentials stay in Engine, but session
 * authority and provider responses are still sensitive; process separation
 * and this allowlist alone must not be described as complete OS isolation.
 * See sprint/lighter_mcp_runtime_design.md, Sandbox and Isolation Rules.
 */
function buildSandboxGlobals(
  boundCall: (operationId: string, params?: Record<string, unknown>, options?: PhysicalCallOptions) => Promise<unknown>,
  session: SessionState,
  access: ResultAccess,
  maxBytes: number,
  invocation: Invocation,
): Record<string, unknown> {
  return {
    call: boundCall,
    session: {
      // Invocation-local counts avoid cross-call attribution when several tools run concurrently.
      get: (...args: unknown[]) => {
        invocation.assertActive();
        // Rejecting before the read prevents a misleading root result and keeps audit counts exact.
        if (args.length !== 1 || typeof args[0] !== "string") {
          throw new Error("MCP_SESSION_GET_ARGUMENTS_INVALID: session.get accepts exactly one string key; use session.page(ref, {path, fields, offset}) for array paths or project session.get(ref) in JavaScript.");
        }
        return session.get(args[0], access);
      },
      // Writes retain the existing explicit-state API but cannot replace runtime snapshots.
      set: (key: string, value: unknown) => { invocation.assertActive(); session.set(key, value); },
      // Paging shares this invocation's output budget and retained-read audit without any provider transport.
      page: (key: string, options?: ResultPageOptions) => { invocation.assertActive(); return session.page(key, options, maxBytes, access); },
    },
    console,
    setTimeout: invocation.setTimeout.bind(invocation),
    clearTimeout: invocation.clearTimeout.bind(invocation),
    sleep: invocation.sleep.bind(invocation),
    decodeBase64,
    encodeBase64,
    atob: boundedAtob,
  };
}
