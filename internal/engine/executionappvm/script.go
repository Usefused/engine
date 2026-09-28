package executionappvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dop251/goja"
)

const (
	MaxBundleBytes = 2 << 20
	MaxInputBytes  = 1 << 20
	MaxOutputBytes = 1 << 20
	MaxHostCalls   = 32
	MaxParallel    = 4
	WallTime       = 30 * time.Second
)

// Host owns the only effects available to authored JavaScript.
type Host interface {
	Fetch(context.Context, json.RawMessage) (json.RawMessage, error)
	DBGet(context.Context) (json.RawMessage, error)
	DBSet(context.Context, json.RawMessage) error
}

type capabilityHostResult struct {
	resolve func(interface{}) error
	reject  func(interface{}) error
	value   any
	err     error
}

type capabilityScriptSession struct {
	ctx       context.Context
	vm        *goja.Runtime
	host      Host
	results   chan capabilityHostResult
	semaphore chan struct{}
	hostCalls int
}

// RunInProcess runs only inside the already-confined child worker.
func RunInProcess(ctx context.Context, bundle []byte, input json.RawMessage, host Host) (json.RawMessage, error) {
	control, err := NewDeterminism()
	// Direct interpreter tests use the same controlled runtime as the worker path.
	if err != nil {
		return nil, err
	}
	return RunInProcessWithDeterminism(ctx, bundle, input, host, control)
}

// RunInProcessWithDeterminism installs Engine-provided sources before evaluating authored code.
func RunInProcessWithDeterminism(ctx context.Context, bundle []byte, input json.RawMessage, host Host, control Determinism) (json.RawMessage, error) {
	if err := validateCapabilityScriptRequest(bundle, input, host); err != nil {
		return nil, err
	}
	// Invalid controls may indicate corrupted replay evidence or a malformed worker request.
	if !control.Valid() {
		return nil, errors.New("capability deterministic controls are invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, WallTime)
	defer cancel()
	// Each invocation gets new globals; no provider credential or Node module enters the interpreter.
	vm := goja.New()
	vm.SetTimeSource(func() time.Time { return time.UnixMilli(control.StartedAtUnixMs).UTC() })
	vm.SetRandSource(capabilityRandomSource(control.RandomSeed))
	vm.SetMaxCallStackSize(512)
	stopInterrupt := context.AfterFunc(ctx, func() { vm.Interrupt("capability timed out") })
	defer stopInterrupt()
	session := &capabilityScriptSession{
		ctx: ctx, vm: vm, host: host,
		results:   make(chan capabilityHostResult, MaxHostCalls),
		semaphore: make(chan struct{}, MaxParallel),
	}
	if err := session.installGlobals(input); err != nil {
		return nil, err
	}
	program, err := goja.Compile("fused-capability.js", string(bundle), true)
	// Compilation never falls back to interpreting request data as JavaScript.
	if err != nil {
		return nil, errors.New("capability bundle is invalid")
	}
	if _, err := vm.RunProgram(program); err != nil {
		return nil, errors.New("capability bundle initialization failed")
	}
	return invokeCapabilityScript(ctx, vm, session.results)
}

// validateCapabilityScriptRequest bounds material before authored JavaScript can allocate memory.
func validateCapabilityScriptRequest(bundle []byte, input json.RawMessage, host Host) error {
	// A missing Engine-owned host cannot acquire execution authority.
	if host == nil {
		return errors.New("capability invocation is invalid")
	}
	// Source and input limits are independent of the caller's HTTP body limit.
	if len(bundle) == 0 || len(bundle) > MaxBundleBytes || len(input) > MaxInputBytes || !json.Valid(input) {
		return errors.New("capability invocation is invalid")
	}
	return nil
}

// installGlobals passes only JSON text and three host functions into the fresh interpreter.
func (session *capabilityScriptSession) installGlobals(input json.RawMessage) error {
	bridge := map[string]any{
		"fetch": session.fetch,
		"dbGet": session.dbGet,
		"dbSet": session.dbSet,
	}
	if err := session.vm.Set("__fusedHost", bridge); err != nil {
		return errors.New("capability host bridge is unavailable")
	}
	if err := session.vm.Set("fusedInputJSON", string(input)); err != nil {
		return errors.New("capability input is unavailable")
	}
	return nil
}

// fetch schedules one JSON-only workspace operation without exposing Engine credentials.
func (session *capabilityScriptSession) fetch(raw string) *goja.Promise {
	return scheduleCapabilityHostCall(session.ctx, session.vm, session.results, session.semaphore, &session.hostCalls, func(callCtx context.Context) (any, error) {
		// Provider input remains bounded before the Engine dispatcher sees it.
		if len(raw) > MaxInputBytes || !json.Valid([]byte(raw)) {
			return nil, errors.New("workspace operation input is invalid")
		}
		value, err := session.host.Fetch(callCtx, json.RawMessage(raw))
		// The bridge returns bounded canonical JSON to the author's proxy.
		if err != nil || len(value) > MaxOutputBytes || !json.Valid(value) {
			return nil, errors.New("workspace operation failed")
		}
		return string(value), nil
	})
}

// dbGet schedules one read of this execution's JSONB document.
func (session *capabilityScriptSession) dbGet() *goja.Promise {
	return scheduleCapabilityHostCall(session.ctx, session.vm, session.results, session.semaphore, &session.hostCalls, func(callCtx context.Context) (any, error) {
		value, err := session.host.DBGet(callCtx)
		// Unset data is represented as JSON null; other values must remain bounded JSON.
		if len(value) == 0 {
			return "null", nil
		}
		if err != nil || len(value) > 512<<10 || !json.Valid(value) {
			return nil, errors.New("execution data read failed")
		}
		return string(value), nil
	})
}

// dbSet schedules one bounded document replacement through Engine-owned storage.
func (session *capabilityScriptSession) dbSet(raw string) *goja.Promise {
	return scheduleCapabilityHostCall(session.ctx, session.vm, session.results, session.semaphore, &session.hostCalls, func(callCtx context.Context) (any, error) {
		// The data cap applies on every write, before durable storage checks it again.
		if len(raw) > 512<<10 || !json.Valid([]byte(raw)) {
			return nil, errors.New("execution data exceeds its limit or is invalid")
		}
		if err := session.host.DBSet(callCtx, json.RawMessage(raw)); err != nil {
			return nil, errors.New("execution data write failed")
		}
		return nil, nil
	})
}

// scheduleCapabilityHostCall runs a trusted Engine effect outside the interpreter and resolves it on the interpreter goroutine.
func scheduleCapabilityHostCall(ctx context.Context, vm *goja.Runtime, results chan<- capabilityHostResult, semaphore chan struct{}, callCount *int, work func(context.Context) (any, error)) *goja.Promise {
	promise, resolve, reject := vm.NewPromise()
	*callCount++
	// The interpreter assigns call order before concurrent work can race to the host bridge.
	ordinal := *callCount
	// An authored loop cannot allocate unlimited provider or database work.
	if *callCount > MaxHostCalls {
		_ = reject("capability host call limit exceeded")
		return promise
	}
	go func() {
		// Concurrent provider work is bounded independently of the total call budget.
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			results <- capabilityHostResult{resolve: resolve, reject: reject, err: ctx.Err()}
			return
		}
		defer func() { <-semaphore }()
		value, err := work(context.WithValue(ctx, callOrdinalKey{}, ordinal))
		results <- capabilityHostResult{resolve: resolve, reject: reject, value: value, err: err}
	}()
	return promise
}

// invokeCapabilityScript validates Zod input and output around the authored execute function.
func invokeCapabilityScript(ctx context.Context, vm *goja.Runtime, results <-chan capabilityHostResult) (json.RawMessage, error) {
	const invocation = `(async () => {
		const app = globalThis.FusedExecutionApp;
		if (!app || !app.input || !app.output || typeof app.execute !== "function") throw new Error("capability unavailable");
		const input = app.input.parse(JSON.parse(fusedInputJSON));
		const output = await app.execute({ input });
		return JSON.stringify(app.output.parse(output));
	})()`
	value, err := vm.RunString(invocation)
	// Authored exceptions stay private because they can include input or provider data.
	if err != nil {
		return nil, errors.New("capability execution failed")
	}
	promise, ok := value.Export().(*goja.Promise)
	if !ok {
		return nil, errors.New("capability result is invalid")
	}
	if err := waitCapabilityPromise(ctx, vm, promise, results); err != nil {
		return nil, err
	}
	return decodeCapabilityPromise(promise)
}

// waitCapabilityPromise delivers completed host work on the interpreter goroutine.
func waitCapabilityPromise(ctx context.Context, vm *goja.Runtime, promise *goja.Promise, results <-chan capabilityHostResult) error {
	for promise.State() == goja.PromiseStatePending {
		select {
		case result := <-results:
			resolveCapabilityHostResult(result)
			// A zero-length interpreter turn drains the author's await continuations.
			if _, err := vm.RunString(""); err != nil {
				return errors.New("capability execution failed")
			}
		case <-ctx.Done():
			return fmt.Errorf("capability execution ended: %w", ctx.Err())
		}
	}
	return nil
}

// resolveCapabilityHostResult settles one promise without exposing a Go object to authored code.
func resolveCapabilityHostResult(result capabilityHostResult) {
	// Provider or storage failures reject only the awaited operation, so authored code can catch them.
	if result.err != nil {
		_ = result.reject(result.err.Error())
		return
	}
	_ = result.resolve(result.value)
}

// decodeCapabilityPromise accepts only a fulfilled JSON value after Zod output parsing.
func decodeCapabilityPromise(promise *goja.Promise) (json.RawMessage, error) {
	// Only a fulfilled, bounded JSON value can become a public typed output.
	if promise.State() != goja.PromiseStateFulfilled {
		return nil, errors.New("capability execution failed")
	}
	encoded, ok := promise.Result().Export().(string)
	if !ok || len(encoded) > MaxOutputBytes || !json.Valid([]byte(encoded)) {
		return nil, errors.New("capability output is invalid")
	}
	return json.RawMessage(encoded), nil
}
