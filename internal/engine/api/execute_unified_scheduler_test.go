package api

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/unified"
	"github.com/google/uuid"
)

type scriptedUnifiedRuntime struct {
	mu                  sync.Mutex
	forwardCalls        []string
	rollbackCalls       []string
	failForward         map[string]error
	failRollback        map[string]error
	forwardStarted      map[string]chan struct{}
	forwardRelease      map[string]<-chan struct{}
	rollbackContextErrs []error
}

// ResolveExactPhysicalOperations returns aligned opaque values because these
// scheduler tests exercise work orchestration after preflight.
func (runtime *scriptedUnifiedRuntime) ResolveExactPhysicalOperations(_ context.Context, _ uuid.UUID, bindings []sandbox.ExactOperationBinding) ([]sandbox.ResolvedPhysicalOperation, error) {
	return make([]sandbox.ResolvedPhysicalOperation, len(bindings)), nil
}

// ValidateResolvedPhysicalSelectors accepts scheduler fixture selectors.
func (*scriptedUnifiedRuntime) ValidateResolvedPhysicalSelectors(sandbox.ResolvedPhysicalOperation, sandbox.PhysicalExecutionSelectors) error {
	return nil
}

// ExecuteResolvedPhysicalJSON records and executes one scripted forward call.
func (runtime *scriptedUnifiedRuntime) ExecuteResolvedPhysicalJSON(_ context.Context, _ auth.RuntimeIdentity, _ sandbox.ResolvedPhysicalOperation, request sandbox.PhysicalExecutionRequest) (sandbox.PhysicalExecutionResult, error) {
	name := fmt.Sprint(request.Params["call"])
	runtime.mu.Lock()
	runtime.forwardCalls = append(runtime.forwardCalls, name)
	started := runtime.forwardStarted[name]
	release := runtime.forwardRelease[name]
	err := runtime.failForward[name]
	runtime.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		<-release
	}
	if err != nil {
		return sandbox.PhysicalExecutionResult{}, err
	}
	body := []byte(fmt.Sprintf(`{"id":%q}`, name+"-id"))
	return sandbox.PhysicalExecutionResult{Body: body}, nil
}

// ExecuteResolvedPhysicalSuccess records one scripted bodyless compensation.
func (runtime *scriptedUnifiedRuntime) ExecuteResolvedPhysicalSuccess(ctx context.Context, _ auth.RuntimeIdentity, _ sandbox.ResolvedPhysicalOperation, request sandbox.PhysicalExecutionRequest) error {
	name := fmt.Sprint(request.Params["rollback"])
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.rollbackCalls = append(runtime.rollbackCalls, name)
	runtime.rollbackContextErrs = append(runtime.rollbackContextErrs, ctx.Err())
	return runtime.failRollback[name]
}

// TestUnifiedSchedulerExecutesOneIndependentTarget proves single-target calls
// create exactly one forward and no inert compensation.
func TestUnifiedSchedulerExecutesOneIndependentTarget(t *testing.T) {
	runtime := newScriptedUnifiedRuntime()
	response := executePreparedUnifiedCall(t, runtime, preparedUnifiedTargetFixture(t, "A", nil, true))
	if len(response.Results) != 1 || response.Results[0].GetStatus() != "success" || len(response.RollbackResults) != 0 {
		t.Fatalf("response = %#v", response)
	}
	assertScriptedCalls(t, runtime, []string{"A"}, nil)
}

// TestUnifiedSchedulerReleasesDependentWithoutLevelBarrier proves an unrelated
// slow root cannot delay a target whose own dependency already succeeded.
func TestUnifiedSchedulerReleasesDependentWithoutLevelBarrier(t *testing.T) {
	runtime := newScriptedUnifiedRuntime()
	aStarted, bStarted, cStarted := make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{}, 1)
	aRelease := make(chan struct{})
	runtime.forwardStarted = map[string]chan struct{}{"A": aStarted, "B": bStarted, "C": cStarted}
	runtime.forwardRelease = map[string]<-chan struct{}{"A": aRelease}
	call := preparedUnifiedCallFixture(t,
		preparedUnifiedTargetFixture(t, "A", nil, false),
		preparedUnifiedTargetFixture(t, "B", nil, false),
		preparedUnifiedTargetFixture(t, "C", []string{"B"}, false),
	)
	done := make(chan *enginev1.ExecuteUnifiedResponse, 1)
	go func() { done <- executePreparedUnifiedCallWithCall(runtime, call) }()
	waitUnifiedSignal(t, aStarted, "A")
	waitUnifiedSignal(t, bStarted, "B")
	// C must start while A is still blocked, proving scheduling follows only
	// C's explicit edge rather than a global topological-level barrier.
	waitUnifiedSignal(t, cStarted, "C")
	close(aRelease)
	response := <-done
	if len(response.Results) != 3 {
		t.Fatalf("result count = %d", len(response.Results))
	}
}

// TestUnifiedSchedulerRollsBackOnlyFailedBindingsDirectDependency locks the
// non-transitive compensation rule for a three-node chain.
func TestUnifiedSchedulerRollsBackOnlyFailedBindingsDirectDependency(t *testing.T) {
	runtime := newScriptedUnifiedRuntime()
	runtime.failForward["C"] = errors.New("provider failed")
	response := executePreparedUnifiedCall(t, runtime,
		preparedUnifiedTargetFixture(t, "A", nil, true),
		preparedUnifiedTargetFixture(t, "B", []string{"A"}, true),
		preparedUnifiedTargetFixture(t, "C", []string{"B"}, false),
	)
	assertRollbackTargets(t, response, []string{"B"}, [][]string{{"C"}})
	assertScriptedCalls(t, runtime, []string{"A", "B", "C"}, []string{"B"})
}

// TestUnifiedSchedulerRollsBackExplicitDependenciesInReverseOrder proves all
// explicit direct targets run and a failed compensation does not gate another.
func TestUnifiedSchedulerRollsBackExplicitDependenciesInReverseOrder(t *testing.T) {
	runtime := newScriptedUnifiedRuntime()
	runtime.failForward["C"] = errors.New("provider failed")
	runtime.failRollback["B"] = errors.New("rollback failed")
	response := executePreparedUnifiedCall(t, runtime,
		preparedUnifiedTargetFixture(t, "A", nil, true),
		preparedUnifiedTargetFixture(t, "B", []string{"A"}, true),
		preparedUnifiedTargetFixture(t, "C", []string{"A", "B"}, false),
	)
	assertRollbackTargets(t, response, []string{"A", "B"}, [][]string{{"C"}, {"C"}})
	assertScriptedCalls(t, runtime, []string{"A", "B", "C"}, []string{"B", "A"})
	if response.RollbackResults[1].GetStatus() != "error" || response.RollbackResults[0].GetStatus() != "success" {
		t.Fatalf("rollback results = %#v", response.RollbackResults)
	}
}

// TestUnifiedSchedulerDeduplicatesSharedDependencyRollback proves concurrent
// consumer failures trigger one compensation with both trigger identities.
func TestUnifiedSchedulerDeduplicatesSharedDependencyRollback(t *testing.T) {
	runtime := newScriptedUnifiedRuntime()
	runtime.failForward["B"] = errors.New("B failed")
	runtime.failForward["C"] = errors.New("C failed")
	response := executePreparedUnifiedCall(t, runtime,
		preparedUnifiedTargetFixture(t, "A", nil, true),
		preparedUnifiedTargetFixture(t, "B", []string{"A"}, false),
		preparedUnifiedTargetFixture(t, "C", []string{"A"}, false),
	)
	assertRollbackTargets(t, response, []string{"A"}, [][]string{{"B", "C"}})
	assertScriptedCalls(t, runtime, []string{"A", "B", "C"}, []string{"A"})
}

// TestUnifiedSchedulerCompensatesSuccessfulPrerequisiteOfSkippedBinding covers
// a multi-dependency consumer with one failed and one successful prerequisite.
func TestUnifiedSchedulerCompensatesSuccessfulPrerequisiteOfSkippedBinding(t *testing.T) {
	runtime := newScriptedUnifiedRuntime()
	runtime.failForward["A"] = errors.New("A failed")
	response := executePreparedUnifiedCall(t, runtime,
		preparedUnifiedTargetFixture(t, "A", nil, false),
		preparedUnifiedTargetFixture(t, "B", nil, true),
		preparedUnifiedTargetFixture(t, "C", []string{"A", "B"}, false),
	)
	if response.Results[2].GetStatus() != "skipped" {
		t.Fatalf("C result = %#v", response.Results[2])
	}
	assertRollbackTargets(t, response, []string{"B"}, [][]string{{"C"}})
}

// TestUnifiedSchedulerCompensatesAfterDependentInputMappingFailure proves a
// consumer that cannot build provider params never enters the physical boundary
// and rolls back only its successful direct prerequisite.
func TestUnifiedSchedulerCompensatesAfterDependentInputMappingFailure(t *testing.T) {
	runtime := newScriptedUnifiedRuntime()
	consumer := preparedUnifiedTargetFixture(t, "B", []string{"A"}, false)
	consumer.input = mustCompileUnifiedProgram(t, "${response.A.missing}", []string{"A"})
	response := executePreparedUnifiedCall(t, runtime,
		preparedUnifiedTargetFixture(t, "A", nil, true), consumer,
	)
	if response.Results[1].GetErrorCode() != "input_mapping_failed" {
		t.Fatalf("consumer result = %#v", response.Results[1])
	}
	assertRollbackTargets(t, response, []string{"A"}, [][]string{{"B"}})
	assertScriptedCalls(t, runtime, []string{"A"}, []string{"A"})
}

// TestUnifiedSchedulerTreatsOutputProjectionFailureAsForwardSuccess proves raw
// responses still feed dependents and projection errors do not trigger rollback.
func TestUnifiedSchedulerTreatsOutputProjectionFailureAsForwardSuccess(t *testing.T) {
	runtime := newScriptedUnifiedRuntime()
	a := preparedUnifiedTargetFixture(t, "A", nil, true)
	a.output = invalidUnifiedOutputFixture(t, "A")
	response := executePreparedUnifiedCall(t, runtime, a, preparedUnifiedTargetFixture(t, "B", []string{"A"}, false))
	if response.Results[0].GetErrorCode() != "output_mapping_failed" || response.Results[1].GetStatus() != "success" {
		t.Fatalf("results = %#v", response.Results)
	}
	assertScriptedCalls(t, runtime, []string{"A", "B"}, nil)
}

// TestUnifiedSchedulerTreatsOutputSchemaFailureAsForwardSuccess proves schema
// projection is also isolated from graph success and compensation decisions.
func TestUnifiedSchedulerTreatsOutputSchemaFailureAsForwardSuccess(t *testing.T) {
	runtime := newScriptedUnifiedRuntime()
	a := preparedUnifiedTargetFixture(t, "A", nil, true)
	a.output = invalidUnifiedOutputSchemaFixture(t, "A")
	response := executePreparedUnifiedCall(t, runtime, a, preparedUnifiedTargetFixture(t, "B", []string{"A"}, false))
	if response.Results[0].GetErrorCode() != "output_validation_failed" || response.Results[1].GetStatus() != "success" {
		t.Fatalf("results = %#v", response.Results)
	}
	assertScriptedCalls(t, runtime, []string{"A", "B"}, nil)
}

// TestUnifiedSchedulerReturnsActionableAuthForForwardAndRollback proves typed
// connected-auth decisions survive both physical execution phases.
func TestUnifiedSchedulerReturnsActionableAuthForForwardAndRollback(t *testing.T) {
	forwardRuntime := newScriptedUnifiedRuntime()
	forwardRuntime.failForward["A"] = &sandbox.ConnectionRequiredError{
		Code: "connection_required", BucketID: "bucket", ServiceID: "service", EndUserRef: "user",
	}
	forward := executePreparedUnifiedCall(t, forwardRuntime, preparedUnifiedTargetFixture(t, "A", nil, false))
	if forward.Results[0].GetErrorCode() != "connection_required" || forward.Results[0].GetAuthAction().GetAction() != "connect" {
		t.Fatalf("forward auth result = %#v", forward.Results[0])
	}

	rollbackRuntime := newScriptedUnifiedRuntime()
	rollbackRuntime.failForward["B"] = errors.New("B failed")
	rollbackRuntime.failRollback["A"] = &sandbox.ReconnectRequiredError{
		Code: "reconnect_required", BucketID: "bucket", ServiceID: "service",
		EndUserRef: "user", ConnectionID: "connection", Reason: "stored_grant_unusable",
	}
	rollback := executePreparedUnifiedCall(t, rollbackRuntime,
		preparedUnifiedTargetFixture(t, "A", nil, true),
		preparedUnifiedTargetFixture(t, "B", []string{"A"}, false),
	)
	if rollback.RollbackResults[0].GetErrorCode() != "reconnect_required" || rollback.RollbackResults[0].GetAuthAction().GetAction() != "reconnect" {
		t.Fatalf("rollback auth result = %#v", rollback.RollbackResults[0])
	}
}

// TestUnifiedSchedulerRollbackSurvivesCallerCancellation proves cleanup uses a
// cancellation-free child while retaining physical execution-policy bounds.
func TestUnifiedSchedulerRollbackSurvivesCallerCancellation(t *testing.T) {
	runtime := newScriptedUnifiedRuntime()
	runtime.failForward["B"] = errors.New("B failed")
	call := preparedUnifiedCallFixture(t,
		preparedUnifiedTargetFixture(t, "A", nil, true),
		preparedUnifiedTargetFixture(t, "B", []string{"A"}, false),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	server := &EngineGRPCServer{unifiedRuntime: runtime}
	_, rollbacks := server.executeUnifiedGraph(ctx, call)
	if len(rollbacks) != 1 || rollbacks[0].GetStatus() != "success" {
		t.Fatalf("rollbacks = %#v", rollbacks)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if len(runtime.rollbackContextErrs) != 1 || runtime.rollbackContextErrs[0] != nil {
		t.Fatalf("rollback context errors = %#v", runtime.rollbackContextErrs)
	}
}

// TestUnifiedChildReplayIdentitySeparatesRollbackPhase proves forward and
// rollback idempotency keys cannot collide for the same logical target.
func TestUnifiedChildReplayIdentitySeparatesRollbackPhase(t *testing.T) {
	appID := uuid.New()
	forward, _ := deriveUnifiedChildIdentity(appID, "work.run", "A", "forward", "logical", []byte(`{}`), nil)
	rollback, _ := deriveUnifiedChildIdentity(appID, "work.run", "A", "rollback", "logical", []byte(`{}`), nil)
	if forward == rollback {
		t.Fatal("forward and rollback child identities collided")
	}
}

// newScriptedUnifiedRuntime initializes all optional decision maps.
func newScriptedUnifiedRuntime() *scriptedUnifiedRuntime {
	return &scriptedUnifiedRuntime{
		failForward: make(map[string]error), failRollback: make(map[string]error),
		forwardStarted: make(map[string]chan struct{}), forwardRelease: make(map[string]<-chan struct{}),
	}
}

// preparedUnifiedTargetFixture compiles one target whose dependent input reads
// the first explicitly declared prerequisite response.
func preparedUnifiedTargetFixture(t *testing.T, name string, dependencies []string, rollback bool) preparedUnifiedTarget {
	t.Helper()
	input := map[string]any{"call": name}
	if len(dependencies) > 0 {
		input["dependency_id"] = "${response." + dependencies[0] + ".id}"
	}
	target := preparedUnifiedTarget{
		name: name, dependsOn: append([]string(nil), dependencies...),
		input: mustCompileUnifiedProgram(t, input, dependencies),
	}
	if rollback {
		target.rollback = &preparedUnifiedRollback{input: mustCompileUnifiedProgram(t, map[string]any{
			"rollback": name, "id": "${response." + name + ".id}",
		}, []string{name})}
	}
	return target
}

// invalidUnifiedOutputFixture creates a reviewed current-target mapping whose
// required field is absent from the scripted provider response.
func invalidUnifiedOutputFixture(t *testing.T, target string) *preparedUnifiedOutput {
	t.Helper()
	program := mustCompileUnifiedProgram(t, "${response."+target+".missing}", []string{target})
	schema, err := compileUnifiedSchema([]byte(`{"type":"string"}`))
	if err != nil {
		t.Fatal(err)
	}
	return &preparedUnifiedOutput{program: program, schema: schema}
}

// invalidUnifiedOutputSchemaFixture returns an existing response field whose
// value intentionally violates the projected public schema.
func invalidUnifiedOutputSchemaFixture(t *testing.T, target string) *preparedUnifiedOutput {
	t.Helper()
	program := mustCompileUnifiedProgram(t, "${response."+target+".id}", []string{target})
	schema, err := compileUnifiedSchema([]byte(`{"type":"integer"}`))
	if err != nil {
		t.Fatal(err)
	}
	return &preparedUnifiedOutput{program: program, schema: schema}
}

// mustCompileUnifiedProgram compiles a bounded DynamicValue fixture.
func mustCompileUnifiedProgram(t *testing.T, value any, targets []string) *unified.Program {
	t.Helper()
	program, err := unified.CompileWithTargets(value, unified.DefaultLimits(), targets)
	if err != nil {
		t.Fatal(err)
	}
	return program
}

// preparedUnifiedCallFixture builds one deterministic in-memory logical call.
func preparedUnifiedCallFixture(t *testing.T, targets ...preparedUnifiedTarget) preparedUnifiedCall {
	t.Helper()
	return preparedUnifiedCall{
		appID: uuid.New(), operation: "work.run", idempotencyKey: "logical-1",
		input: map[string]any{"request": "value"}, targets: targets,
	}
}

// executePreparedUnifiedCall runs a prepared call through the scheduler test runtime.
func executePreparedUnifiedCall(t *testing.T, runtime *scriptedUnifiedRuntime, targets ...preparedUnifiedTarget) *enginev1.ExecuteUnifiedResponse {
	t.Helper()
	return executePreparedUnifiedCallWithCall(runtime, preparedUnifiedCallFixture(t, targets...))
}

// executePreparedUnifiedCallWithCall returns the same response envelope as the RPC.
func executePreparedUnifiedCallWithCall(runtime *scriptedUnifiedRuntime, call preparedUnifiedCall) *enginev1.ExecuteUnifiedResponse {
	server := &EngineGRPCServer{unifiedRuntime: runtime}
	results, rollbacks := server.executeUnifiedGraph(context.Background(), call)
	return &enginev1.ExecuteUnifiedResponse{Results: results, RollbackResults: rollbacks}
}

// waitUnifiedSignal bounds concurrency assertions so failures cannot hang CI.
func waitUnifiedSignal(t *testing.T, signal <-chan struct{}, target string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("target %s did not start", target)
	}
}

// assertRollbackTargets verifies deterministic response ordering and triggers.
func assertRollbackTargets(t *testing.T, response *enginev1.ExecuteUnifiedResponse, targets []string, triggers [][]string) {
	t.Helper()
	if len(response.RollbackResults) != len(targets) {
		t.Fatalf("rollback count = %d, want %d", len(response.RollbackResults), len(targets))
	}
	for index, result := range response.RollbackResults {
		if result.GetTarget() != targets[index] || !reflect.DeepEqual(result.GetTriggeredBy(), triggers[index]) {
			t.Fatalf("rollback %d = %#v", index, result)
		}
	}
}

// assertScriptedCalls compares the ordered calls where ordering is part of the contract.
func assertScriptedCalls(t *testing.T, runtime *scriptedUnifiedRuntime, forwards, rollbacks []string) {
	t.Helper()
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !sameUnifiedCallSet(runtime.forwardCalls, forwards) || !reflect.DeepEqual(runtime.rollbackCalls, rollbacks) {
		t.Fatalf("calls = forwards:%#v rollbacks:%#v, want forwards:%#v rollbacks:%#v", runtime.forwardCalls, runtime.rollbackCalls, forwards, rollbacks)
	}
}

// sameUnifiedCallSet ignores concurrent forward order while preserving exact membership.
func sameUnifiedCallSet(actual, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	counts := make(map[string]int, len(expected))
	for _, value := range expected {
		counts[value]++
	}
	for _, value := range actual {
		counts[value]--
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}
