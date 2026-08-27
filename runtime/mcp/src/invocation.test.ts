import { afterEach, describe, expect, it, vi } from "vitest";
import { Invocation } from "./invocation.js";
import { CallImpl, runExecute, SessionState } from "./sandbox.js";

const options = { sessionId: "synthetic", enginePort: "0" };
const limits = { timeoutMs: 50, maxCalls: 10 };

/** Keeps fake clocks local so other runtime tests exercise their normal deadlines. */
afterEach(() => { vi.useRealTimers(); });

/** Exercises suspended continuations using fake providers, never device-control operations. */
describe("invocation lifetime", () => {
  /** Reproduces off/wait/on with an expired wait and proves the second dispatch never happens. */
  it("cancels the timer and prevents the next provider call on timeout", async () => {
    vi.useFakeTimers();
    const call = vi.fn().mockResolvedValue({ ok: true });
    const pending = runExecute('await call("first"); await new Promise(r => setTimeout(r, 200)); await call("second");', options, new SessionState(), limits, call);
    await vi.advanceTimersByTimeAsync(50);
    expect(await pending).toMatchObject({ isError: true, executionOutcome: "timed_out" });
    await vi.advanceTimersByTimeAsync(300);
    expect(call).toHaveBeenCalledTimes(1);
    expect(call.mock.calls[0][3].aborted).toBe(true);
    expect(vi.getTimerCount()).toBe(0);
  });

  /** Rejected waits cannot be caught to acquire a fresh dispatch budget. */
  it("blocks a caught timeout from making another call or changing session state", async () => {
    vi.useFakeTimers();
    const call = vi.fn().mockResolvedValue({ ok: true });
    const session = new SessionState();
    const pending = runExecute('try { await sleep(200); } catch { try { session.set("late", true); } catch {} await call("late"); }', options, session, limits, call);
    await vi.advanceTimersByTimeAsync(300);
    expect(await pending).toMatchObject({ isError: true, executionOutcome: "timed_out" });
    expect(call).not.toHaveBeenCalled();
    expect(session.get("late")).toBeUndefined();
  });

  /** A stale HTTP response cannot wake a script into a new operation after its caller has left. */
  it("blocks continuation even when a transport ignores abort", async () => {
    vi.useFakeTimers();
    let release!: (value: unknown) => void;
    // Deliberately ignore AbortSignal to test the independent post-await guard.
    const call = vi.fn<CallImpl>(() => new Promise((resolve) => { release = resolve; }));
    const pending = runExecute('await call("first"); await call("second");', options, new SessionState(), limits, call);
    await vi.advanceTimersByTimeAsync(50);
    expect(await pending).toMatchObject({ executionOutcome: "timed_out" });
    release({ ok: true });
    await vi.advanceTimersByTimeAsync(1);
    expect(call).toHaveBeenCalledTimes(1);
  });

  /** Successful return and ordinary errors both revoke detached work before the next invocation. */
  it.each(['return "done";', 'throw new Error("failed");'])("cleans up detached timers on %s", async (ending) => {
    vi.useFakeTimers();
    const call = vi.fn().mockResolvedValue({});
    await runExecute(`setTimeout(() => call("late"), 100); ${ending}`, options, new SessionState(), limits, call);
    await vi.advanceTimersByTimeAsync(200);
    expect(call).not.toHaveBeenCalled();
    expect(vi.getTimerCount()).toBe(0);
  });

  /** Completed invocations cannot leak in-flight, unawaited requests into the next tool call. */
  it("aborts unawaited requests on successful return", async () => {
    const call = vi.fn<CallImpl>(() => new Promise(() => {}));
    const outcome = await runExecute('call("pending"); return true;', options, new SessionState(), limits, call);
    expect(outcome).toMatchObject({ executionOutcome: "completed", isError: false });
    expect(call.mock.calls[0][3]?.aborted).toBe(true);
  });

  /** Detached sleep promises are observed by the host when cleanup rejects them. */
  it("cleans up unawaited sleeps without an unhandled rejection", async () => {
    vi.useFakeTimers();
    const pending = runExecute('sleep(200); await sleep(100);', options, new SessionState(), limits);
    await vi.advanceTimersByTimeAsync(300);
    expect(await pending).toMatchObject({ executionOutcome: "timed_out" });
    expect(await runExecute('sleep(200); return true;', options, new SessionState(), limits)).toMatchObject({ executionOutcome: "completed" });
    await vi.advanceTimersByTimeAsync(300);
    expect(vi.getTimerCount()).toBe(0);
  });

  /** CPU work can delay a watchdog, but must not defer the clock check at provider dispatch. */
  it("checks the absolute deadline even before the watchdog callback runs", () => {
    vi.useFakeTimers();
    const invocation = new Invocation(50);
    vi.setSystemTime(Date.now() + 51);
    expect(() => invocation.assertActive()).toThrow(/MCP_EXECUTE_TIMEOUT/);
    expect(invocation.outcome(true)).toBe("timed_out");
    expect(invocation.signal.aborted).toBe(true);
    expect(vi.getTimerCount()).toBe(0);
  });

  /** Explicit client cancellation is not a policy timeout and cannot disclose client-provided reasons. */
  it("honors caller cancellation without poisoning the reusable session", async () => {
    vi.useFakeTimers();
    const controller = new AbortController();
    const session = new SessionState();
    const call = vi.fn().mockResolvedValue({});
    const pending = runExecute('await sleep(25); await call("late");', options, session, limits, call, controller.signal);
    controller.abort("private cancellation reason");
    const outcome = await pending;
    expect(outcome).toMatchObject({ isError: true, executionOutcome: "cancelled" });
    expect(outcome.text).toContain("MCP_EXECUTE_CANCELLED");
    expect(outcome.text).not.toContain("private");
    await vi.advanceTimersByTimeAsync(100);
    expect(call).not.toHaveBeenCalled();
    expect(await runExecute('return 42;', options, session, limits, call)).toMatchObject({ text: "42", executionOutcome: "completed" });
  });

  /** Requests cancelled before dispatch must never issue a provider call. */
  it("rejects an already-cancelled invocation before evaluating its script", async () => {
    const controller = new AbortController();
    controller.abort();
    const call = vi.fn().mockResolvedValue({});
    expect(await runExecute('await call("never");', options, new SessionState(), limits, call, controller.signal)).toMatchObject({ executionOutcome: "cancelled" });
    expect(call).not.toHaveBeenCalled();
  });
});

/** Preserves useful timer compatibility without leaking Node timer objects or session authority. */
describe("managed timers", () => {
  /** Verifies normal delay, callback arguments, numeric handles, and explicit clearing together. */
  it("supports both sleep and setTimeout within budget", async () => {
    vi.useFakeTimers();
    const pending = runExecute('const handle = setTimeout(() => { throw new Error("cleared"); }, 10); clearTimeout(handle); await sleep(5); return await new Promise(resolve => setTimeout((v) => resolve({v,handle:typeof handle}), 5, "ok"));', options, new SessionState(), limits);
    await vi.advanceTimersByTimeAsync(20);
    expect(await pending).toMatchObject({ text: '{"v":"ok","handle":"number"}', isError: false });
    expect(vi.getTimerCount()).toBe(0);
  });

  /** Detached callback exceptions become ordinary execute errors instead of terminating Node. */
  it.each(['() => { throw new Error("callback failure"); }', 'async () => { throw new Error("callback failure"); }'])("handles callback rejection: %s", async (callback) => {
    vi.useFakeTimers();
    const pending = runExecute(`setTimeout(${callback}, 5); await sleep(40);`, options, new SessionState(), limits);
    await vi.advanceTimersByTimeAsync(50);
    const outcome = await pending;
    expect(outcome).toMatchObject({ isError: true, executionOutcome: "failed" });
    expect(JSON.parse(outcome.text)).toMatchObject({ message: "callback failure", recovery_action: "do_not_replay", provider_execution: "unknown" });
    expect(vi.getTimerCount()).toBe(0);
  });

  /** Invalid timer parameters must fail closed rather than becoming immediate provider mutations. */
  it.each(['setTimeout("code", 1)', 'sleep(-1)', 'sleep(Infinity)', 'sleep(2147483648)', 'sleep("1")'])("rejects %s", async (script) => {
    const outcome = await runExecute(`await ${script};`, options, new SessionState(), limits);
    expect(outcome.isError).toBe(true);
    expect(outcome.text).toContain("MCP_EXECUTE_TIMER_INVALID");
  });

  /** Allocation pressure is bounded even if callbacks would all be cancelled by the deadline. */
  it("caps concurrent pending timers", async () => {
    const outcome = await runExecute('for (let i=0;i<1001;i++) setTimeout(() => {}, 1000);', options, new SessionState(), { ...limits, timeoutMs: 500 });
    expect(outcome.text).toContain("MCP_EXECUTE_TIMER_LIMIT");
  });
});
