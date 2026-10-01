package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"io"
	"os/exec"
	"sync"
	"time"
)

type persistentCapabilityWorker struct {
	command  *exec.Cmd
	stdin    io.WriteCloser
	writer   *capabilityFrameWriter
	cleanup  func()
	queue    chan struct{}
	slots    chan struct{}
	slotCond *sync.Cond
	closed   chan struct{}
	once     sync.Once
	mu       sync.Mutex
	nextID   uint64
	pending  map[uint64]*capabilityInvocation
}

type capabilityInvocation struct {
	ctx              context.Context
	host             CapabilityScriptHost
	result           chan capabilityInvocationResult
	ids              map[uint64]struct{}
	ordinals         map[int]struct{}
	parallel         chan struct{}
	releaseAdmission func()
}

type capabilityInvocationResult struct {
	output json.RawMessage
	err    error
}

// startPersistentCapabilityWorker launches a confined process and validates its one loaded bundle.
func startPersistentCapabilityWorker(ctx, lifetime context.Context, bundle []byte) (*persistentCapabilityWorker, error) {
	command, cleanup, err := capabilityWorkerCommand(lifetime)
	if err != nil {
		return nil, ErrCapabilityWorkerUnavailable
	}
	stdin, err := command.StdinPipe()
	// Both pipes must be established before untrusted code can start.
	if err != nil {
		cleanup()
		return nil, ErrCapabilityWorkerUnavailable
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		cleanup()
		return nil, ErrCapabilityWorkerUnavailable
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		cleanup()
		return nil, ErrCapabilityWorkerUnavailable
	}
	worker := &persistentCapabilityWorker{
		command: command, stdin: stdin, writer: &capabilityFrameWriter{encoder: json.NewEncoder(stdin)}, cleanup: cleanup,
		queue: make(chan struct{}, capabilityWorkerQueueLimit), slots: make(chan struct{}, capabilityWorkerCapacity),
		closed: make(chan struct{}), pending: make(map[uint64]*capabilityInvocation),
	}
	worker.slotCond = sync.NewCond(&worker.mu)
	ready := make(chan error, 1)
	go worker.readLoop(capabilityFrameScanner(stdout), ready)
	if err := worker.writer.write(capabilityProcessFrame{Kind: "load", Bundle: bundle}); err != nil {
		worker.stop()
		return nil, ErrCapabilityWorkerUnavailable
	}
	// Load includes a five-second declaration inspection inside the confined process.
	select {
	case err := <-ready:
		if err != nil {
			worker.stop()
			return nil, err
		}
	case <-ctx.Done():
		worker.stop()
		return nil, ctx.Err()
	case <-time.After(7 * time.Second):
		worker.stop()
		return nil, ErrCapabilityWorkerUnavailable
	}
	return worker, nil
}

// run reserves one interpreter slot and correlates all frames by request ID.
func (worker *persistentCapabilityWorker) run(ctx context.Context, input json.RawMessage, host CapabilityScriptHost, control CapabilityDeterminism, limit int, releaseAdmission func()) (json.RawMessage, error) {
	// Retain ownership locally until the invocation enters the worker's pending map.
	transferred := false
	defer func() {
		// Rejections before dispatch cannot keep a global execution permit.
		if !transferred {
			releaseAdmission()
		}
	}()
	start := time.Now()
	// A closed or saturated child must return the deployment lease to the scheduler.
	if err := worker.admit(ctx, limit); err != nil {
		return nil, err
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.Int64("worker.queue_wait_ms", time.Since(start).Milliseconds()))
	defer func() { <-worker.queue }()
	invocation := &capabilityInvocation{
		ctx: ctx, host: host, releaseAdmission: releaseAdmission, result: make(chan capabilityInvocationResult, 1),
		ids: make(map[uint64]struct{}, maxCapabilityHostCalls), ordinals: make(map[int]struct{}, maxCapabilityHostCalls),
		parallel: make(chan struct{}, maxCapabilityParallel),
	}
	worker.mu.Lock()
	// Failure after a slot is acquired must release it before returning.
	if worker.isClosedLocked() {
		worker.releaseSlotLocked()
		worker.mu.Unlock()
		return nil, ErrCapabilityWorkerUnavailable
	}
	worker.nextID++
	id := worker.nextID
	worker.pending[id] = invocation
	// Only worker completion or termination owns the lease from this point.
	transferred = true
	worker.mu.Unlock()
	request := capabilityProcessFrame{Kind: "run", RequestID: id, Input: input, Determinism: control}
	// A failed dispatch terminates the child before returning its global permit.
	if err := worker.writer.write(request); err != nil {
		worker.stop()
		return nil, ErrCapabilityWorkerUnavailable
	}
	return worker.await(ctx, id, invocation)
}

// admit bounds waiting callers and reserves one interpreter slot in the shared child.
func (worker *persistentCapabilityWorker) admit(ctx context.Context, limit int) error {
	select {
	case worker.queue <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-worker.closed:
		return ErrCapabilityWorkerUnavailable
	default:
		// A bounded waiting room prevents an overloaded family from accumulating HTTP goroutines.
		return ErrCapabilityWorkerOverloaded
	}
	// A separate slot gate lets a plan change take effect without replacing the app process.
	if err := worker.reserveSlot(ctx, limit); err != nil {
		<-worker.queue
		return err
	}
	return nil
}

// reserveSlot waits for the plan's per-app ceiling within the fixed process safety capacity.
func (worker *persistentCapabilityWorker) reserveSlot(ctx context.Context, limit int) error {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	stopWake := context.AfterFunc(ctx, func() {
		worker.mu.Lock()
		worker.slotCond.Broadcast()
		worker.mu.Unlock()
	})
	defer stopWake()
	for {
		// A canceled or dead request must leave the bounded queue without claiming an interpreter.
		if err := ctx.Err(); err != nil {
			return err
		}
		if worker.isClosedLocked() {
			return ErrCapabilityWorkerUnavailable
		}
		// The mutex makes occupancy checks and slot claims atomic across competing requests.
		if len(worker.slots) < limit {
			worker.slots <- struct{}{}
			return nil
		}
		worker.slotCond.Wait()
	}
}

// releaseSlotLocked wakes queued calls only after a completed interpreter frees capacity.
func (worker *persistentCapabilityWorker) releaseSlotLocked() {
	<-worker.slots
	worker.slotCond.Broadcast()
}

// await returns one invocation's result without taking a slot from unrelated requests.
func (worker *persistentCapabilityWorker) await(ctx context.Context, id uint64, invocation *capabilityInvocation) (json.RawMessage, error) {
	select {
	case result := <-invocation.result:
		return result.output, result.err
	case <-ctx.Done():
		// An abandoned HTTP request releases its slot only when the child finishes canceling.
		_ = worker.writer.write(capabilityProcessFrame{Kind: "cancel", RequestID: id})
		return nil, ctx.Err()
	case <-worker.closed:
		return nil, ErrCapabilityWorkerUnavailable
	}
}

// readLoop validates the load acknowledgement and then dispatches one shared IPC stream.
func (worker *persistentCapabilityWorker) readLoop(scanner *bufio.Scanner, ready chan<- error) {
	if !scanner.Scan() {
		ready <- ErrCapabilityWorkerUnavailable
		worker.stop()
		return
	}
	loaded, err := decodeCapabilityFrame(scanner.Bytes())
	// No invocation may start unless the child acknowledged its exact resident bundle.
	if err != nil || loaded.Kind != "loaded" || loaded.RequestID != 0 {
		ready <- ErrCapabilityWorkerUnavailable
		worker.stop()
		return
	}
	ready <- nil
	for scanner.Scan() {
		frame, err := decodeCapabilityFrame(scanner.Bytes())
		// A malformed worker frame invalidates the entire confined process.
		if err != nil || worker.handleFrame(frame) != nil {
			worker.stop()
			return
		}
	}
	worker.stop()
}

// handleFrame routes a child call or result only to its originating invocation.
func (worker *persistentCapabilityWorker) handleFrame(frame capabilityProcessFrame) error {
	worker.mu.Lock()
	invocation := worker.pending[frame.RequestID]
	worker.mu.Unlock()
	// Unknown request IDs cannot acquire an unrelated execution's host authority.
	if invocation == nil || frame.RequestID == 0 {
		return errors.New("capability worker protocol failed")
	}
	output, done, err := handleCapabilityProcessFrame(invocation.ctx, worker.writer, "run", invocation.host, frame, invocation.ids, invocation.ordinals, invocation.parallel)
	// An invalid host call is a process protocol violation; an authored done error belongs only to this request.
	if err != nil && !done {
		return err
	}
	if done {
		worker.finish(frame.RequestID, capabilityInvocationResult{output: output, err: err})
	}
	return nil
}

// finish releases capacity exactly once after the child's final frame.
func (worker *persistentCapabilityWorker) finish(id uint64, result capabilityInvocationResult) {
	worker.mu.Lock()
	invocation := worker.pending[id]
	delete(worker.pending, id)
	// A late frame after process shutdown has no slot left to release.
	if invocation != nil {
		worker.releaseSlotLocked()
	}
	worker.mu.Unlock()
	// Canceled callers may no longer be listening, so the result channel is buffered.
	if invocation != nil {
		invocation.releaseAdmission()
		invocation.result <- result
	}
}

// active reports interpreter slots that have not yet emitted a final frame.
func (worker *persistentCapabilityWorker) active() int {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	return len(worker.pending)
}

// isClosed lets the manager replace a child that exhausted its cumulative CPU budget.
func (worker *persistentCapabilityWorker) isClosed() bool {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	return worker.isClosedLocked()
}

// isClosedLocked checks process state while the pending map lock is held.
func (worker *persistentCapabilityWorker) isClosedLocked() bool {
	select {
	case <-worker.closed:
		return true
	default:
		return false
	}
}

// stop terminates the child and fails every pending execution without duplicating cleanup.
func (worker *persistentCapabilityWorker) stop() {
	worker.once.Do(func() {
		worker.mu.Lock()
		close(worker.closed)
		pending := worker.pending
		worker.pending = make(map[uint64]*capabilityInvocation)
		// Process shutdown must wake every queued plan-slot waiter and release admitted slots once.
		for range pending {
			worker.releaseSlotLocked()
		}
		worker.slotCond.Broadcast()
		worker.mu.Unlock()
		_ = worker.stdin.Close()
		_ = worker.command.Process.Kill()
		_ = worker.command.Wait()
		worker.cleanup()
		// Shutdown returns global permits only after the child has stopped executing.
		for _, invocation := range pending {
			invocation.releaseAdmission()
			invocation.result <- capabilityInvocationResult{err: ErrCapabilityWorkerUnavailable}
		}
	})
}
