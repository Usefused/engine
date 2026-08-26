import vm from "node:vm";
import { CallClientOptions, remoteCall } from "./callClient.js";
import { EXECUTE_RESULT_OUTPUT_POLICY, SerializedJsonOutput, serializeBoundedJson } from "./outputLimits.js";

export interface ExecuteLimits {
  timeoutMs: number;
  maxCalls: number;
}

export const DEFAULT_EXECUTE_LIMITS: ExecuteLimits = {
  timeoutMs: 30_000,
  maxCalls: 10,
};

/** Exposes only admitted text so no caller can accidentally serialize an untrusted result again. */
export type ExecuteOutcome = SerializedJsonOutput;

/**
 * SessionState is the design doc's "explicit session state" primitive
 * (session.get/set) -- deliberately not the same thing as the vm execution
 * realm, which is always fresh per invocation. One SessionState instance is
 * created per MCP session process and threaded into every runExecute call
 * for that session, so a script can stash a large intermediate result and a
 * later script in the same session can read it back, without either script's
 * vm realm (globals/prototypes) ever being reused.
 *
 * A plain in-memory Map is enough for the spike -- no SSE-reconnect
 * durability, no cross-process sharing. Both are explicitly out of scope
 * (sprint/lighter_mcp_runtime_spike_plan.md, Task 4).
 */
export class SessionState {
  private readonly store = new Map<string, unknown>();

  get(key: string): unknown {
    return this.store.get(key);
  }

  set(key: string, value: unknown): void {
    this.store.set(key, value);
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
  const context = vm.createContext(buildSandboxGlobals(boundCall, session));
  const wrapped = `(async () => {\n${script}\n})()`;

  try {
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
    return withinSerializationDeadline(() => serializeBoundedJson(result ?? null, EXECUTE_RESULT_OUTPUT_POLICY), deadline);
  } catch (err) {
    return serializeExecuteError(err, deadline, limits.timeoutMs);
  }
}

/** Executes all user-provided serialization hooks under the invocation's remaining deadline. */
function withinSerializationDeadline(serialize: () => ExecuteOutcome, deadline: number): ExecuteOutcome {
  const remainingMs = deadline - Date.now();
  // An exhausted execution cannot acquire another timeout window while formatting its result.
  if (remainingMs <= 0) {
    throw new Error("execute serialization deadline exceeded");
  }
  const context = vm.createContext({ serialize });
  return new vm.Script("serialize()", { filename: "execute-output.js" }).runInContext(context, { timeout: remainingMs }) as ExecuteOutcome;
}

/** Extracts and bounds error text inside the deadline because thrown values can contain hostile hooks. */
function serializeExecuteError(error: unknown, deadline: number, timeoutMs: number): ExecuteOutcome {
  try {
    return withinSerializationDeadline(() => boundedExecuteError(describeError(error)), deadline);
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

/** Preserves ordinary Engine/script messages while making failures share the result byte ceiling. */
function boundedExecuteError(message: string): ExecuteOutcome {
  const output = serializeBoundedJson(message, EXECUTE_RESULT_OUTPUT_POLICY);
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
): Record<string, unknown> {
  return {
    call: boundCall,
    session: {
      get: (key: string) => session.get(key),
      set: (key: string, value: unknown) => session.set(key, value),
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
