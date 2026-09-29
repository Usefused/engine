package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/engine/executionappvm"
)

const capabilityFrameBytes = 5 << 20

// ErrCapabilityWorkerUnavailable distinguishes an isolation or IPC failure from invalid authored declarations.
var ErrCapabilityWorkerUnavailable = errors.New("unified app worker is unavailable")

var capabilityTestWorker struct {
	once sync.Once
	path string
	err  error
}

type capabilityProcessFrame = executionappvm.Frame

type capabilityFrameWriter struct {
	mu      sync.Mutex
	encoder *json.Encoder
}

type capabilityCallOrdinalKey struct{}

// IsCapabilityWorkerAvailable probes the real isolated process with a fixed, side-effect-free declaration.
func IsCapabilityWorkerAvailable(ctx context.Context) bool {
	const probe = `globalThis.FusedExecutionManifest={schemaVersion:1,inputSchema:{},outputSchema:{},searchable:[],selectedOperations:[]};globalThis.FusedUnifiedApp={input:{parse(v){return v}},output:{parse(v){return v}},execute:async()=>({})};`
	manifest, err := InspectCapabilityBundle(ctx, []byte(probe))
	// Tests may skip only when the same production worker path cannot execute safely on this host.
	return err == nil && string(manifest) == `{"schemaVersion":1,"inputSchema":{},"outputSchema":{},"searchable":[],"selectedOperations":[]}`
}

// CapabilityCallOrdinal returns the authored host-call order assigned by the single interpreter goroutine.
func CapabilityCallOrdinal(ctx context.Context) (int, bool) {
	ordinal, ok := ctx.Value(capabilityCallOrdinalKey{}).(int)
	// Replay treats missing or out-of-range sequence numbers as unauthenticated ordering.
	if !ok || ordinal < 1 || ordinal > maxCapabilityHostCalls {
		return 0, false
	}
	return ordinal, true
}

// write serializes complete JSON frames so parallel host calls cannot interleave pipe writes.
func (writer *capabilityFrameWriter) write(frame capabilityProcessFrame) error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.encoder.Encode(frame)
}

// capabilityFrameScanner bounds each IPC message before decoding it into Engine-owned data.
func capabilityFrameScanner(reader io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), capabilityFrameBytes)
	return scanner
}

// runCapabilityProcess admits one invocation to a disposable OS-confined worker.
func runCapabilityProcess(ctx context.Context, request capabilityProcessFrame, host CapabilityScriptHost) (json.RawMessage, error) {
	deadline := capabilityWallTime
	// Bundle inspection has a shorter deadline because it has no provider effects.
	if request.Kind == "inspect" {
		deadline = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	command, cleanup, err := capabilityWorkerCommand(ctx)
	// Unsupported or unavailable OS isolation always rejects authored code.
	if err != nil {
		return nil, ErrCapabilityWorkerUnavailable
	}
	defer cleanup()
	return exchangeCapabilityProcess(ctx, command, request, host)
}

// exchangeCapabilityProcess owns process lifetime and exchanges bounded JSON frames with its child.
func exchangeCapabilityProcess(ctx context.Context, command *exec.Cmd, request capabilityProcessFrame, host CapabilityScriptHost) (json.RawMessage, error) {
	stdin, err := command.StdinPipe()
	// Both pipes must exist before launching the isolated process.
	if err != nil {
		return nil, ErrCapabilityWorkerUnavailable
	}
	stdout, err := command.StdoutPipe()
	// A missing response pipe cannot be replaced by in-process interpretation.
	if err != nil {
		return nil, ErrCapabilityWorkerUnavailable
	}
	command.Stderr = io.Discard
	// Child creation is the security boundary; the Engine never runs this request in-process on failure.
	if err := command.Start(); err != nil {
		return nil, ErrCapabilityWorkerUnavailable
	}
	defer func() {
		_ = stdin.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	writer := &capabilityFrameWriter{encoder: json.NewEncoder(stdin)}
	if err := writer.write(request); err != nil {
		// An incomplete bootstrap request cannot acquire host authority.
		return nil, ErrCapabilityWorkerUnavailable
	}
	return readCapabilityProcess(ctx, capabilityFrameScanner(stdout), writer, request.Kind, host)
}

// readCapabilityProcess handles one bounded stream of worker frames until a final result or failure.
func readCapabilityProcess(ctx context.Context, scanner *bufio.Scanner, writer *capabilityFrameWriter, requestKind string, host CapabilityScriptHost) (json.RawMessage, error) {
	seenIDs := make(map[uint64]struct{}, maxCapabilityHostCalls)
	seenOrdinals := make(map[int]struct{}, maxCapabilityHostCalls)
	semaphore := make(chan struct{}, maxCapabilityParallel)
	for scanner.Scan() {
		frame, err := decodeCapabilityFrame(scanner.Bytes())
		// A malformed child frame aborts the entire invocation.
		if err != nil {
			return nil, err
		}
		output, done, err := handleCapabilityProcessFrame(ctx, writer, requestKind, host, frame, seenIDs, seenOrdinals, semaphore)
		if err != nil {
			// A rejected host request cannot be treated as a completed result.
			return nil, err
		}
		// Only a validated final frame can finish this invocation.
		if done {
			return output, nil
		}
	}
	// EOF, oversized frames, or a terminated child cannot produce a successful invocation.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, ErrCapabilityWorkerUnavailable
}

// handleCapabilityProcessFrame admits only a final result or a bounded host effect from a run worker.
func handleCapabilityProcessFrame(ctx context.Context, writer *capabilityFrameWriter, requestKind string, host CapabilityScriptHost, frame capabilityProcessFrame, ids map[uint64]struct{}, ordinals map[int]struct{}, semaphore chan struct{}) (json.RawMessage, bool, error) {
	// Bundle inspection cannot request any Engine effect.
	if frame.Kind == "done" {
		// Completed worker timings share the original invocation context and cannot carry authored labels.
		if executionappvm.ValidExecutionPhases(frame.Phases) {
			if observer, ok := host.(executionappvm.PhaseObserver); ok {
				observer.RecordExecutionPhases(ctx, frame.Phases)
			}
		}
		output, err := capabilityProcessOutput(frame)
		return output, true, err
	}
	if frame.Kind != "call" || requestKind != "run" || host == nil {
		return nil, false, errors.New("capability worker protocol failed")
	}
	if err := admitCapabilityWorkerCall(frame, ids, ordinals); err != nil {
		return nil, false, err
	}
	go serveCapabilityHostCall(ctx, writer, host, frame, semaphore)
	return nil, false, nil
}

// admitCapabilityWorkerCall caps effects and rejects duplicate IDs or replay ordinals from a compromised child.
func admitCapabilityWorkerCall(frame capabilityProcessFrame, ids map[uint64]struct{}, ordinals map[int]struct{}) error {
	// The parent does not rely solely on the interpreter's internal effect budget.
	if frame.ID == 0 || frame.Ordinal < 1 || frame.Ordinal > maxCapabilityHostCalls || len(ids) >= maxCapabilityHostCalls {
		return errors.New("capability host call limit exceeded")
	}
	if _, exists := ids[frame.ID]; exists {
		// A repeated IPC identifier could dispatch the same effect twice.
		return errors.New("capability worker protocol failed")
	}
	if _, exists := ordinals[frame.Ordinal]; exists {
		// Replay ordinal uniqueness is independent of IPC arrival order.
		return errors.New("capability worker protocol failed")
	}
	ids[frame.ID] = struct{}{}
	ordinals[frame.Ordinal] = struct{}{}
	return nil
}

// decodeCapabilityFrame rejects malformed and oversized IPC rather than trusting the child.
func decodeCapabilityFrame(raw []byte) (capabilityProcessFrame, error) {
	var frame capabilityProcessFrame
	if len(raw) == 0 || len(raw) >= capabilityFrameBytes || json.Unmarshal(raw, &frame) != nil {
		return frame, errors.New("capability worker protocol failed")
	}
	return frame, nil
}

// capabilityProcessOutput accepts only bounded JSON returned by the isolated worker.
func capabilityProcessOutput(frame capabilityProcessFrame) (json.RawMessage, error) {
	// Worker errors have no authored source or provider payload attached to public failures.
	if frame.Error != "" {
		// The typed error keeps private detail out of ordinary Error() projections.
		if frame.Diagnostic != nil {
			return nil, frame.Diagnostic
		}
		return nil, errors.New(frame.Error)
	}
	if len(frame.Value) == 0 || len(frame.Value) > maxCapabilityOutputBytes || !json.Valid(frame.Value) {
		return nil, errors.New("capability worker output is invalid")
	}
	return frame.Value, nil
}

// serveCapabilityHostCall dispatches a worker request through the existing trusted host surface.
func serveCapabilityHostCall(ctx context.Context, writer *capabilityFrameWriter, host CapabilityScriptHost, request capabilityProcessFrame, semaphore chan struct{}) {
	// At most four trusted effects can run concurrently even if the child issues all 32 calls.
	select {
	case semaphore <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-semaphore }()
	result := capabilityProcessFrame{Kind: "reply", RequestID: request.RequestID, ID: request.ID, Value: json.RawMessage("null")}
	value, err := dispatchCapabilityHostCall(ctx, host, request)
	// Authored code sees a bounded error string, never a Go object or credential.
	if err != nil {
		result.Error = err.Error()
	} else if len(value) > 0 {
		// Effect results remain JSON-only and bounded before they cross into the child.
		if len(value) > maxCapabilityOutputBytes || !json.Valid(value) {
			result.Error = "capability host result is invalid"
		} else {
			result.Value = value
		}
	}
	_ = writer.write(result)
}

// dispatchCapabilityHostCall allows only the three JSON-only Engine effects declared by the capability API.
func dispatchCapabilityHostCall(ctx context.Context, host CapabilityScriptHost, request capabilityProcessFrame) (json.RawMessage, error) {
	// Reject forged child requests before they reach provider or execution storage.
	if request.ID == 0 || request.Ordinal < 1 || request.Ordinal > maxCapabilityHostCalls || len(request.Payload) > maxCapabilityInputBytes || !json.Valid(request.Payload) {
		return nil, errors.New("capability host call is invalid")
	}
	ctx = context.WithValue(ctx, capabilityCallOrdinalKey{}, request.Ordinal)
	switch request.Method {
	case "fetch":
		return host.Fetch(ctx, request.Payload)
	case "dbGet":
		return host.DBGet(ctx)
	case "dbSet":
		return nil, host.DBSet(ctx, request.Payload)
	default:
		return nil, errors.New("capability host call is unavailable")
	}
}

// capabilityChildEnvironment strips inherited credentials and configuration from the worker.
func capabilityChildEnvironment() []string {
	return []string{"FUSED_CAPABILITY_WORKER=1"}
}

// capabilityExecutable requires the packaged standalone worker beside this Engine binary.
func capabilityExecutable() (string, error) {
	engine, err := os.Executable()
	// A missing Engine identity cannot be resolved to a trusted sibling worker.
	if err != nil {
		return "", err
	}
	// Package tests use a packaged sibling or build the standalone worker because their transient
	// test binary can lack a sibling; production never compiles or self-reexecutes authored code.
	if strings.HasSuffix(engine, ".test") {
		return capabilityExecutableForTest()
	}
	worker := filepath.Join(filepath.Dir(engine), "fused-execution-worker")
	info, err := os.Stat(worker)
	// The worker must be a regular packaged executable, never a directory or caller-selected path.
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", ErrCapabilityWorkerUnavailable
	}
	return worker, nil
}

// capabilityExecutableForTest resolves a packaged worker or builds one for Linux package integration tests.
func capabilityExecutableForTest() (string, error) {
	// macOS package tests exercise the interpreter directly because its nested sandbox profile
	// is unavailable on this host; Linux integration tests use a packaged or locally built worker.
	if runtime.GOOS != "linux" {
		return "", ErrCapabilityWorkerUnavailable
	}
	capabilityTestWorker.once.Do(func() {
		// A prebuilt test worker permits hermetic Linux containers without a Go toolchain.
		if prebuilt := os.Getenv("FUSED_EXECUTION_WORKER_TEST_BINARY"); filepath.IsAbs(prebuilt) {
			capabilityTestWorker.path = prebuilt
			return
		}
		// Packaged-image tests can reuse the worker beside their mounted test binary without source or Go.
		if executable, err := os.Executable(); err == nil {
			worker := filepath.Join(filepath.Dir(executable), "fused-execution-worker")
			// Only a regular executable sibling is a plausible packaged worker for this test binary.
			if info, err := os.Stat(worker); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
				capabilityTestWorker.path = worker
				return
			}
		}
		_, source, _, ok := runtime.Caller(0)
		// Tests cannot substitute an arbitrary working directory for the checked-out worker source.
		if !ok {
			capabilityTestWorker.err = ErrCapabilityWorkerUnavailable
			return
		}
		root := filepath.Clean(filepath.Join(filepath.Dir(source), "../../.."))
		dir, err := os.MkdirTemp("", "fused-execution-worker-test-")
		// Test worker build failures remain unavailable rather than reverting to Engine self-reexec.
		if err != nil {
			capabilityTestWorker.err = err
			return
		}
		capabilityTestWorker.path = filepath.Join(dir, "fused-execution-worker")
		command := exec.Command("go", "build", "-tags", "headless", "-o", capabilityTestWorker.path, "./cmd/execution-worker")
		command.Dir = root
		capabilityTestWorker.err = command.Run()
	})
	return capabilityTestWorker.path, capabilityTestWorker.err
}
