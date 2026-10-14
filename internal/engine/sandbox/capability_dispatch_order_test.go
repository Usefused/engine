package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"testing"
)

type invocationOrderRecorder struct {
	worker     *persistentCapabilityWorker
	lastID     uint64
	count      int
	orderError error
}

// Write records actual serialized IPC order and completes requests without relying on OS worker availability.
func (recorder *invocationOrderRecorder) Write(data []byte) (int, error) {
	var frame capabilityProcessFrame
	// The fixture accepts only complete run frames emitted by the production dispatch path.
	if err := json.Unmarshal(data, &frame); err != nil {
		return 0, err
	}
	// The real child requires increasing IDs even when executions themselves overlap.
	if frame.Kind != "run" || frame.RequestID <= recorder.lastID {
		recorder.orderError = fmt.Errorf("run ID %d arrived after %d", frame.RequestID, recorder.lastID)
	}
	recorder.lastID = frame.RequestID
	recorder.count++
	// Completion frees capacity while other goroutines compete to assign and dispatch subsequent requests.
	recorder.worker.finish(frame.RequestID, capabilityInvocationResult{output: json.RawMessage(`{}`)})
	runtime.Gosched()
	return len(data), nil
}

// TestCapabilityWorkerDispatchPreservesRequestOrder exercises the real parent dispatch under sustained concurrent invocation pressure.
func TestCapabilityWorkerDispatchPreservesRequestOrder(t *testing.T) {
	const callers, requests = 64, 32
	worker := &persistentCapabilityWorker{
		queue: make(chan struct{}, callers), slots: make(chan struct{}, callers),
		closed: make(chan struct{}), pending: make(map[uint64]*capabilityInvocation),
	}
	worker.slotCond = sync.NewCond(&worker.mu)
	recorder := &invocationOrderRecorder{worker: worker}
	worker.writer = &capabilityFrameWriter{encoder: json.NewEncoder(recorder)}
	control, err := NewCapabilityDeterminism()
	// Every invocation still traverses the same deterministic input serialization used by a hosted worker.
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		// A shared barrier creates contention at the ID allocation and IPC write boundary.
		go func() {
			defer group.Done()
			<-start
			for range requests {
				_, err := worker.run(context.Background(), json.RawMessage(`{}`), &capabilityScriptTestHost{}, control, callers, func() { /* No global scheduler permit is held by this fixture. */ })
				// Ordering errors are recorded independently; an execution error is an unrelated regression.
				if err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	close(start)
	group.Wait()
	// Every request must be delivered once, in increasing order, with all interpreter slots returned.
	if recorder.orderError != nil || recorder.count != callers*requests || worker.active() != 0 {
		t.Fatalf("order=%v delivered=%d active=%d", recorder.orderError, recorder.count, worker.active())
	}
}
