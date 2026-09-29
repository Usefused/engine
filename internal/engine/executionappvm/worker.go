package executionappvm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

type processHost struct {
	phases    []PhaseTiming
	ctx       context.Context
	writer    *frameWriter
	requestID uint64
	mutex     sync.Mutex
	pending   map[uint64]chan Frame
	nextID    atomic.Uint64
}

// Four VMs leave room under the 128 MiB data cap for the worker and host-call buffers.
const maxWorkerRequests = 4

type loadedWorker struct {
	bundle []byte
	writer *frameWriter
	mutex  sync.Mutex
	active map[uint64]*loadedRequest
	lastID uint64
}

type loadedRequest struct {
	host   *processHost
	cancel context.CancelFunc
}

// RunWorker confines the child before handling either legacy inspection or a loaded app version.
func RunWorker() int {
	// Isolation and resource limits must be active before parsing or evaluating authored code.
	if err := confineWorker(); err != nil {
		return 1
	}
	if err := limitCapabilityWorker(); err != nil {
		return 1
	}
	scanner := frameScanner(os.Stdin)
	// A missing bootstrap frame means this child was not started by a valid Engine request.
	if !scanner.Scan() {
		return 1
	}
	request, err := decodeFrame(scanner.Bytes())
	// Protocol failures terminate the disposable process before authored code runs.
	if err != nil {
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), WallTime)
	defer cancel()
	w := &frameWriter{encoder: json.NewEncoder(os.Stdout)}
	// A loaded bundle stays inside this one confined process for many independent executions.
	if request.Kind == "load" {
		return runLoadedWorker(scanner, w, request)
	}
	return executeCapabilityWorkerRequest(ctx, scanner, w, request)
}

// runLoadedWorker binds one immutable bundle to this process and dispatches bounded requests.
func runLoadedWorker(scanner *bufio.Scanner, writer *frameWriter, load Frame) int {
	// A second bundle cannot silently change the identity of an already loaded app version.
	if load.RequestID != 0 || len(load.Bundle) == 0 || len(load.Bundle) > MaxBundleBytes {
		return 1
	}
	// Validation runs after confinement, so an invalid or active top-level declaration
	// never becomes a resident version and never gains the host-call bridge.
	if _, err := InspectInProcess(context.Background(), load.Bundle); err != nil {
		return 1
	}
	worker := &loadedWorker{bundle: load.Bundle, writer: writer, active: make(map[uint64]*loadedRequest)}
	if writer.write(Frame{Kind: "loaded"}) != nil {
		return 1
	}
	for scanner.Scan() {
		frame, err := decodeFrame(scanner.Bytes())
		// Malformed parent frames terminate the process before another authored invocation starts.
		if err != nil || worker.handle(frame) != nil {
			worker.stop()
			return 1
		}
	}
	worker.stop()
	// The Engine owns the pipe lifetime; a closed input stream means this version is retiring.
	if scanner.Err() != nil {
		return 1
	}
	return 0
}

// handle routes one parent frame without sharing invocation state between requests.
func (worker *loadedWorker) handle(frame Frame) error {
	switch frame.Kind {
	case "run":
		return worker.start(frame)
	case "reply":
		return worker.reply(frame)
	case "cancel":
		return worker.cancel(frame.RequestID)
	default:
		// Only the first frame may carry bundle source; later frames cannot reload it.
		return errors.New("capability worker protocol failed")
	}
}

// start reserves one fresh interpreter and host bridge for an invocation ID.
func (worker *loadedWorker) start(frame Frame) error {
	worker.mutex.Lock()
	defer worker.mutex.Unlock()
	// Increasing IDs prevent late replies from a finished execution entering a later one.
	if frame.RequestID <= worker.lastID || frame.RequestID == 0 || len(frame.Bundle) != 0 || len(worker.active) >= maxWorkerRequests {
		return errors.New("capability worker protocol failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), WallTime)
	host := &processHost{ctx: ctx, writer: worker.writer, requestID: frame.RequestID, pending: make(map[uint64]chan Frame)}
	worker.active[frame.RequestID] = &loadedRequest{host: host, cancel: cancel}
	worker.lastID = frame.RequestID
	go worker.execute(ctx, frame, host)
	return nil
}

// execute gives each request new JavaScript globals while reusing the confined process.
func (worker *loadedWorker) execute(ctx context.Context, frame Frame, host *processHost) {
	output, err := RunInProcessWithDeterminism(ctx, worker.bundle, frame.Input, host, frame.Determinism)
	result := Frame{Kind: "done", RequestID: frame.RequestID, Value: output, Phases: host.phases}
	// Trusted IPC separates encrypted diagnostics from safe public errors.
	if err != nil {
		result.Error = "capability execution failed"
		result.Diagnostic = PrivateDiagnostic(err)
	}
	worker.mutex.Lock()
	request := worker.active[frame.RequestID]
	delete(worker.active, frame.RequestID)
	worker.mutex.Unlock()
	// Free the slot before publishing completion, so the next admitted run cannot race cleanup.
	if request != nil {
		request.cancel()
	}
	_ = worker.writer.write(result)
}

// reply settles only a host call belonging to the named invocation.
func (worker *loadedWorker) reply(frame Frame) error {
	worker.mutex.Lock()
	request := worker.active[frame.RequestID]
	worker.mutex.Unlock()
	// A late response to a canceled execution has no authority over another request.
	if request == nil {
		return nil
	}
	request.host.deliverReply(frame)
	return nil
}

// cancel interrupts one invocation without stopping unrelated requests in the same worker.
func (worker *loadedWorker) cancel(id uint64) error {
	worker.mutex.Lock()
	request := worker.active[id]
	worker.mutex.Unlock()
	// Cancellation after completion is idempotent because its final result may race the parent.
	if request != nil {
		request.cancel()
	}
	return nil
}

// stop interrupts active interpreters when the parent stream closes or violates the protocol.
func (worker *loadedWorker) stop() {
	worker.mutex.Lock()
	defer worker.mutex.Unlock()
	for _, request := range worker.active {
		request.cancel()
	}
}

// executeCapabilityWorkerRequest evaluates one request with no direct Engine host objects.
func executeCapabilityWorkerRequest(ctx context.Context, scanner interface {
	Scan() bool
	Bytes() []byte
}, writer *frameWriter, request Frame) int {
	var output json.RawMessage
	var phases []PhaseTiming
	var err error
	// Inspect has no host IPC, so declarations cannot use provider or database operations.
	switch request.Kind {
	case "inspect":
		output, err = InspectInProcess(ctx, request.Bundle)
	case "run":
		host := &processHost{ctx: ctx, writer: writer, pending: make(map[uint64]chan Frame)}
		go host.readReplies(scanner)
		output, err = RunInProcessWithDeterminism(ctx, request.Bundle, request.Input, host, request.Determinism)
		phases = host.phases
	default:
		// The worker accepts only the two parent-declared evaluation modes.
		return 1
	}
	result := Frame{Kind: "done", Value: output, Phases: phases}
	// Private details travel only in the separate diagnostic field for encrypted retention.
	if err != nil {
		result.Error = "capability execution failed"
		result.Diagnostic = PrivateDiagnostic(err)
	}
	if writer.write(result) != nil {
		// A result that cannot cross IPC must never be reported as a successful execution.
		return 1
	}
	return 0
}

// readReplies distributes parent responses to the matching pending child host calls.
func (host *processHost) readReplies(scanner interface {
	Scan() bool
	Bytes() []byte
}) {
	for scanner.Scan() {
		frame, err := decodeFrame(scanner.Bytes())
		// A broken or forged response cannot settle a host operation.
		if err != nil || frame.Kind != "reply" {
			return
		}
		host.deliverReply(frame)
	}
}

// deliverReply matches one Engine reply to a pending host call within this invocation.
func (host *processHost) deliverReply(frame Frame) {
	// A reply addressed to another invocation cannot settle a local promise.
	if frame.RequestID != host.requestID {
		return
	}
	host.mutex.Lock()
	pending := host.pending[frame.ID]
	delete(host.pending, frame.ID)
	host.mutex.Unlock()
	// Unknown and duplicate IDs cannot inject data into another pending operation.
	if pending != nil {
		pending <- frame
	}
}

// exchange sends a JSON-only host request and awaits the matching Engine reply.
func (host *processHost) exchange(ctx context.Context, method string, payload json.RawMessage) (json.RawMessage, error) {
	id := host.nextID.Add(1)
	result := make(chan Frame, 1)
	host.mutex.Lock()
	host.pending[id] = result
	host.mutex.Unlock()
	ordinal, ok := CallOrdinal(ctx)
	// An IPC call without an interpreter-issued ordinal cannot acquire host authority.
	if !ok {
		return nil, errors.New("capability host call order is unavailable")
	}
	request := Frame{Kind: "call", RequestID: host.requestID, ID: id, Ordinal: ordinal, Method: method, Payload: payload}
	if err := host.writer.write(request); err != nil {
		// A broken parent pipe cannot trigger a local effect fallback.
		return nil, errors.New("capability host bridge failed")
	}
	select {
	case reply := <-result:
		// The child accepts only bounded JSON as a host return value.
		if reply.Error != "" {
			return nil, errors.New(reply.Error)
		}
		if len(reply.Value) > MaxOutputBytes || !json.Valid(reply.Value) {
			return nil, errors.New("capability host result is invalid")
		}
		return reply.Value, nil
	case <-host.ctx.Done():
		return nil, host.ctx.Err()
	}
}

// Fetch sends one workspace operation request through the parent-owned execution boundary.
func (host *processHost) Fetch(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	return host.exchange(ctx, "fetch", input)
}

// DBGet reads execution data through parent IPC without giving the child a database connection.
func (host *processHost) DBGet(ctx context.Context) (json.RawMessage, error) {
	return host.exchange(ctx, "dbGet", json.RawMessage("null"))
}

// DBSet replaces execution data through parent IPC without giving the child a database connection.
func (host *processHost) DBSet(ctx context.Context, data json.RawMessage) error {
	_, err := host.exchange(ctx, "dbSet", data)
	return err
}

// Frame is the bounded JSON protocol shared with the trusted Engine parent.
type Frame struct {
	Phases      []PhaseTiming    `json:"phases,omitempty"`
	Kind        string           `json:"kind"`
	RequestID   uint64           `json:"requestId,omitempty"`
	ID          uint64           `json:"id,omitempty"`
	Ordinal     int              `json:"ordinal,omitempty"`
	Method      string           `json:"method,omitempty"`
	Bundle      []byte           `json:"bundle,omitempty"`
	Input       json.RawMessage  `json:"input,omitempty"`
	Payload     json.RawMessage  `json:"payload,omitempty"`
	Value       json.RawMessage  `json:"value,omitempty"`
	Diagnostic  *DiagnosticError `json:"diagnostic,omitempty"`
	Error       string           `json:"error,omitempty"`
	Determinism Determinism      `json:"determinism,omitempty"`
}

const maxFrameBytes = 5 << 20

type frameWriter struct {
	mu      sync.Mutex
	encoder *json.Encoder
}

// write serializes child frames so parallel host calls cannot interleave JSON.
func (writer *frameWriter) write(frame Frame) error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.encoder.Encode(frame)
}

// frameScanner bounds every parent-supplied JSON frame before decoding.
func frameScanner(reader io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxFrameBytes)
	return scanner
}

// decodeFrame rejects malformed or oversized parent IPC material.
func decodeFrame(raw []byte) (Frame, error) {
	var frame Frame
	// A malformed bootstrap or reply must stop the worker before another host call.
	if len(raw) == 0 || len(raw) >= maxFrameBytes || json.Unmarshal(raw, &frame) != nil {
		return frame, errors.New("capability worker protocol failed")
	}
	return frame, nil
}

// RecordExecutionPhases adds real timing metadata to the completion frame without exposing a new author effect.
func (host *processHost) RecordExecutionPhases(_ context.Context, phases []PhaseTiming) {
	host.phases = phases
}
