import vm from "node:vm";
import { CallClientOptions, remoteCall } from "./callClient.js";
import { SerializedJsonOutput, serializeBoundedJson } from "./outputLimits.js";
import { DeliveredResult, EXECUTE_ERROR_OUTPUT_POLICY, isResultReference, RetainedResults } from "./retainedResults.js";
import { executeOutputBudget, EXECUTE_INLINE_BYTES } from "./resultBudget.js";
import { pageResult, ResultPageOptions } from "./resultPaging.js";

export interface ExecuteLimits {
  timeoutMs: number;
  maxCalls: number;
  outputBudgetBytes?: number;
}

export const DEFAULT_EXECUTE_LIMITS: ExecuteLimits = {
  timeoutMs: 30_000,
  maxCalls: 10,
};

/** Exposes only admitted text so no caller can accidentally serialize an untrusted result again. */
export interface ExecuteOutcome extends DeliveredResult {
  access: ResultAccess;
  outputBudgetBytes: number;
}

/** Tracks only bounded retrieval counts; references and result values never become audit metadata. */
interface ResultAccess {
  retained_reads: number;
  unavailable_reads: number;
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
) => Promise<unknown>;

/**
 * runExecute runs one untrusted script in a fresh vm realm and returns an
 * admitted JSON result or bounded error text. This is the security-critical function in this
 * package -- read the allowlist comment below before changing anything here.
 */
export async function runExecute(
  script: string,
  callOptions: CallClientOptions,
  session: SessionState,
  limits: ExecuteLimits = DEFAULT_EXECUTE_LIMITS,
  callImpl: CallImpl = remoteCall,
): Promise<ExecuteOutcome> {
  const deadline = Date.now() + limits.timeoutMs;
  const boundCall = buildBoundCall(callOptions, limits.maxCalls, callImpl);
  const access: ResultAccess = { retained_reads: 0, unavailable_reads: 0 };
  const wrapped = `(async () => {\n${script}\n})()`;
  let outputBudget = EXECUTE_INLINE_BYTES;

  try {
    outputBudget = executeOutputBudget(limits.outputBudgetBytes);
    const context = vm.createContext(buildSandboxGlobals(boundCall, session, access, outputBudget));
    const compiled = new vm.Script(wrapped, { filename: "execute.ts" });
    // vm's own `timeout` guards one specific failure mode: a synchronous,
    // CPU-bound infinite loop (e.g. `while (true) {}`) in the script's
    // pre-first-await code, which would otherwise block this single-
    // threaded process's event loop entirely -- including the ability to
    // run the setTimeout callback that withWallClockTimeout below relies
    // on. It does NOT bound anything async (see that function's comment for
    // why a second, independent mechanism is required).
    const resultPromise = compiled.runInContext(context, { timeout: limits.timeoutMs }) as Promise<unknown>;
    const result = await withWallClockTimeout(resultPromise, limits.timeoutMs);
    // Retention and previewing stay under the same deadline as user-defined serialization hooks.
    const output = withinSerializationDeadline(() => session.deliver(result ?? null, outputBudget), deadline);
    return { ...output, access, outputBudgetBytes: outputBudget };
  } catch (err) {
    return { ...serializeExecuteError(err, deadline, limits.timeoutMs, outputBudget), delivery: "error", access, outputBudgetBytes: outputBudget };
  }
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
function serializeExecuteError(error: unknown, deadline: number, timeoutMs: number, maxBytes: number): SerializedJsonOutput {
  try {
    return withinSerializationDeadline(() => boundedExecuteError(describeError(error), maxBytes), deadline);
  } catch {
    // Failure to render an error must never recurse through its getters or toString outside the VM.
    const timedOut = Date.now() >= deadline;
    return {
      text: JSON.stringify({
        code: timedOut ? "MCP_EXECUTE_TIMEOUT" : "MCP_EXECUTE_ERROR_SERIALIZATION_FAILED",
        message: timedOut ? `execute timed out after ${timeoutMs}ms` : "execute error could not be rendered safely",
      }),
      isError: true,
    };
  }
}

/** Errors cannot be paged, so the smaller of the error policy and invocation budget is authoritative. */
function boundedExecuteError(message: string, maxBytes: number): SerializedJsonOutput {
  const output = serializeBoundedJson(message, { ...EXECUTE_ERROR_OUTPUT_POLICY, maxBytes: Math.min(maxBytes, EXECUTE_ERROR_OUTPUT_POLICY.maxBytes) });
  // Rejected messages retain the stable limit failure; accepted messages keep their original text.
  return output.isError ? output : { text: message, isError: true };
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
 * buildBoundCall wraps remoteCall with the hard call-count cap. The counter
 * lives in this closure, in trusted host code the sandboxed script has no
 * reference to and can't reset or read -- the cap can't be bypassed by
 * anything the script does, only by changing this function.
 */
function buildBoundCall(callOptions: CallClientOptions, maxCalls: number, callImpl: CallImpl) {
  let callCount = 0;
  return async (operationId: string, params: Record<string, unknown> = {}): Promise<unknown> => {
    callCount++;
    if (callCount > maxCalls) {
      throw new Error(`call() limit exceeded (max ${maxCalls} per execute invocation)`);
    }
    return callImpl(callOptions, operationId, params);
  };
}

/**
 * buildSandboxGlobals is an explicit allowlist, built from scratch, not a
 * copy of the outer global scope with a few dangerous things deleted. That
 * distinction matters: an allowlist can't accidentally leak a capability
 * that gets added to Node's globals later; a denylist can. Only `call`,
 * `session`, `console`, `setTimeout`, and `clearTimeout` are placed here --
 * everything else the script sees (JSON, Math, Array, Object, Promise,
 * Function, ...) is inherent to any JS realm and was never something this
 * function granted or could withhold. Nothing with an I/O path (fetch,
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
 * session, never pooled) and the fact that this process never holds
 * credential material at all, so even a full pivot yields nothing to steal.
 * See sprint/lighter_mcp_runtime_design.md, Sandbox and Isolation Rules.
 */
function buildSandboxGlobals(
  boundCall: (operationId: string, params?: Record<string, unknown>) => Promise<unknown>,
  session: SessionState,
  access: ResultAccess,
  maxBytes: number,
): Record<string, unknown> {
  return {
    call: boundCall,
    session: {
      // Invocation-local counts avoid cross-call attribution when several tools run concurrently.
      get: (key: string) => session.get(key, access),
      // Writes retain the existing explicit-state API but cannot replace runtime snapshots.
      set: (key: string, value: unknown) => session.set(key, value),
      // Paging shares this invocation's output budget and retained-read audit without any provider transport.
      page: (key: string, options?: ResultPageOptions) => session.page(key, options, maxBytes, access),
    },
    console,
    setTimeout,
    clearTimeout,
  };
}

/**
 * withWallClockTimeout bounds the *async* portion of a script's execution --
 * an await'd call() that hangs, or a loop of many calls that individually
 * return but never finish as a whole. vm's own `timeout` option (see
 * runExecute) cannot catch this: it only measures synchronous execution
 * time, and once the script is suspended at an `await`, control has already
 * returned to the event loop. This function is what actually enforces the
 * wall-clock budget the design doc calls for.
 */
function withWallClockTimeout<T>(promise: Promise<T>, timeoutMs: number): Promise<T> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      reject(new Error(`execute timed out after ${timeoutMs}ms`));
    }, timeoutMs);

    promise.then(
      (value) => {
        clearTimeout(timer);
        resolve(value);
      },
      (err) => {
        clearTimeout(timer);
        reject(err);
      },
    );
  });
}
