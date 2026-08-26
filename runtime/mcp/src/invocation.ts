/** Bounds queued callbacks independently of the provider-call budget. */
const MAX_PENDING_TIMERS = 1000;
export type ExecutionOutcome = "completed" | "failed" | "timed_out" | "cancelled";

/** Owns cooperative cancellation; hard CPU/process isolation remains the Engine's responsibility. */
export class Invocation {
  readonly deadline: number;
  private readonly controller = new AbortController();
  private readonly timers = new Map<number, ReturnType<typeof setTimeout>>();
  private readonly watchdog: ReturnType<typeof setTimeout>;
  private nextTimer = 0;
  private closed = false;
  private stopped?: ExecutionOutcome;
  private readonly cancelled: Promise<never>;
  private rejectCancelled!: (reason: unknown) => void;

  /** Starts one deadline before evaluation so synchronous work cannot buy another async budget. */
  constructor(private readonly timeoutMs: number, private readonly callerSignal?: AbortSignal) {
    this.deadline = Date.now() + timeoutMs;
    // A single cancellation promise also wakes sleeps and pending provider calls promptly.
    this.cancelled = new Promise((_, reject) => { this.rejectCancelled = reject; });
    // Cancellation can precede VM evaluation; never leave its rejection unobserved.
    void this.cancelled.catch(() => {});
    this.watchdog = setTimeout(() => this.stop("timed_out"), timeoutMs);
    callerSignal?.addEventListener("abort", this.cancelFromCaller, { once: true });
    // An already-cancelled request must not dispatch even its first operation.
    if (callerSignal?.aborted) this.cancelFromCaller();
  }

  /** Gives only trusted transport code access to cancellation, never the script. */
  get signal(): AbortSignal { return this.controller.signal; }

  /** Keeps timer-driven and dispatch-time deadline checks on the same termination path. */
  assertActive(): void {
    // Timers can run late after synchronous work; dispatch must check the clock itself.
    if (!this.closed && Date.now() >= this.deadline) this.stop("timed_out");
    // Closure is permanent even if a script catches an abort and tries another call.
    if (this.closed) throw this.controller.signal.reason;
  }

  /** Cancels waits without assuming Promise rejection can stop the underlying JavaScript. */
  wait<T>(promise: Promise<T>): Promise<T> {
    const waiting = Promise.race([promise, this.cancelled]);
    // Cleanup also rejects intentionally unawaited sleeps; they must not crash the session process.
    void waiting.catch(() => {});
    return waiting;
  }

  /** Captures a bounded host-owned status before cleanup aborts outstanding transport work. */
  outcome(failed: boolean): ExecutionOutcome {
    // Deadline expiry during synchronous evaluation or serialization may precede the watchdog.
    if (!this.stopped && failed && Date.now() >= this.deadline) return "timed_out";
    return this.stopped ?? (failed ? "failed" : "completed");
  }

  /** Releases every invocation-owned capability on success, error, or cancellation. */
  close(reason = new Error("MCP_EXECUTE_CLOSED: execution has finished")): void {
    this.closed = true;
    clearTimeout(this.watchdog);
    for (const timer of this.timers.values()) clearTimeout(timer);
    this.timers.clear();
    this.callerSignal?.removeEventListener("abort", this.cancelFromCaller);
    this.rejectCancelled(reason);
    this.controller.abort(reason);
  }

  /** Returns a numeric handle instead of exposing a refreshable Node Timeout object. */
  setTimeout(callback: (...args: unknown[]) => unknown, delay = 0, ...args: unknown[]): number {
    this.assertActive();
    // Avoid string evaluation and coercion hooks at the host callback boundary.
    if (typeof callback !== "function") throw new Error("MCP_EXECUTE_TIMER_INVALID: callback must be a function");
    // Node clamps overflowing delays to one millisecond, which could trigger a mutation early.
    if (!Number.isFinite(delay) || delay < 0 || delay > 2_147_483_647) {
      throw new Error("MCP_EXECUTE_TIMER_INVALID: delay must be finite and between 0 and 2147483647 ms");
    }
    // The invocation deadline alone does not bound simultaneous timer allocations.
    if (this.timers.size >= MAX_PENDING_TIMERS) throw new Error("MCP_EXECUTE_TIMER_LIMIT: too many pending timers");
    const id = ++this.nextTimer;
    this.timers.set(id, setTimeout(() => this.fireTimer(id, callback, args), delay));
    return id;
  }

  /** Unknown handles are harmless and cannot cancel another invocation's host timers. */
  clearTimeout(id: number): void {
    clearTimeout(this.timers.get(id));
    this.timers.delete(id);
  }

  /** Provides an awaitable delay that shares the invocation deadline and cancellation. */
  sleep(delay: number): Promise<void> {
    // The timer callback intentionally resolves with no host callback arguments.
    return this.wait(new Promise<void>((resolve) => { this.setTimeout(() => resolve(), delay); }));
  }

  /** Converts callback failures into tool failures instead of crashing the session process. */
  private fireTimer(id: number, callback: (...args: unknown[]) => unknown, args: unknown[]): void {
    this.timers.delete(id);
    // Cleanup can win a race with an already-queued callback.
    if (this.closed) return;
    try {
      this.assertActive();
      // Async callbacks need an observer as well as the synchronous catch below.
      void Promise.resolve(callback(...args)).catch((error: unknown) => this.fail(error));
    } catch (error) {
      this.fail(error);
    }
  }

  /** Preserves the first failure when callback rejection races deadline or request cancellation. */
  private fail(reason: unknown): void {
    // A later callback failure must not reclassify a completed invocation.
    if (this.closed) return;
    this.rejectCancelled(reason);
    this.close();
  }

  /** Uses stable errors, never the client's arbitrary cancellation reason, in protocol output. */
  private stop(outcome: "timed_out" | "cancelled"): void {
    // First termination owns both the outcome and the cancellation reason.
    if (this.closed) return;
    this.stopped = outcome;
    // Timeout and caller cancellation require different recovery decisions from the agent.
    const reason = outcome === "timed_out"
      ? new Error(`MCP_EXECUTE_TIMEOUT: execute timed out after ${this.timeoutMs}ms`)
      : new Error("MCP_EXECUTE_CANCELLED: execution was cancelled");
    this.rejectCancelled(reason);
    this.close(reason);
  }

  /** Retains a removable listener identity without exposing the caller's signal to the VM. */
  private readonly cancelFromCaller = (): void => { this.stop("cancelled"); };
}
