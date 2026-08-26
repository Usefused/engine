package api

import (
	"context"

	"github.com/Usefused/engine/internal/engine/executionevent"

	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
)

type unifiedExecutionState uint8

const (
	unifiedStatePending unifiedExecutionState = iota
	unifiedStateScheduled
	unifiedStateSucceeded
	unifiedStateFailed
	unifiedStateSkipped
)

type unifiedForwardCompletion struct {
	index   int
	outcome unifiedTargetOutcome
}

type plannedUnifiedRollback struct {
	targetIndex int
	triggeredBy []string
}

type unifiedRollbackCompletion struct {
	position int
	result   *enginev1.UnifiedRollbackResult
}

// unifiedGraphOutcome separates caller-visible target receipts from effective
// binding values consumed by the final operation output projection.
type unifiedGraphOutcome struct {
	results   []*enginev1.UnifiedTargetResult
	rollbacks []*enginev1.UnifiedRollbackResult
	outputs   map[string]any
}

// executeUnifiedGraph starts each target as soon as its own direct dependencies
// succeed, then waits for all forwards before beginning compensation.
func (s *EngineGRPCServer) executeUnifiedGraph(ctx context.Context, call preparedUnifiedCall) unifiedGraphOutcome {
	results := make([]*enginev1.UnifiedTargetResult, len(call.targets))
	states := make([]unifiedExecutionState, len(call.targets))
	responses := make(map[string]any, len(call.targets))
	outputs := make(map[string]any, len(call.targets))
	byTarget := indexPreparedUnifiedTargets(call.targets)
	ready, completed := discoverUnifiedForwardWork(call.targets, results, states, byTarget)
	completions := make(chan unifiedForwardCompletion, len(call.targets))
	running := 0
	for completed < len(call.targets) {
		for running < maxUnifiedConcurrency && len(ready) > 0 {
			index := ready[0]
			ready = ready[1:]
			responseContext := unifiedDependencyResponses(call.targets[index], responses)
			running++
			go s.runUnifiedForward(ctx, call, index, responseContext, completions)
		}
		// a validated dependency DAG always has scheduled or running work;
		// this guard prevents corrupted state from deadlocking the RPC.
		if running == 0 {
			break
		}
		completion := <-completions
		running--
		completed++
		results[completion.index] = completion.outcome.result
		if completion.outcome.forwardSucceeded {
			states[completion.index] = unifiedStateSucceeded
			responses[call.targets[completion.index].name] = completion.outcome.response
			if completion.outcome.outputReady {
				outputs[call.targets[completion.index].name] = completion.outcome.output
			}
		} else {
			states[completion.index] = unifiedStateFailed
		}
		newReady, skipped := discoverUnifiedForwardWork(call.targets, results, states, byTarget)
		ready = append(ready, newReady...)
		completed += skipped
	}
	rollbacks := s.executeUnifiedRollbacks(ctx, call, results, states, responses, byTarget)
	return unifiedGraphOutcome{results: results, rollbacks: rollbacks, outputs: outputs}
}

// indexPreparedUnifiedTargets maps exact public targets to stable result order.
func indexPreparedUnifiedTargets(targets []preparedUnifiedTarget) map[string]int {
	indexed := make(map[string]int, len(targets))
	for index, target := range targets {
		indexed[target.name] = index
	}
	return indexed
}

// discoverUnifiedForwardWork schedules newly ready targets and propagates
// dependency skips until no additional pending target is conclusively blocked.
func discoverUnifiedForwardWork(targets []preparedUnifiedTarget, results []*enginev1.UnifiedTargetResult, states []unifiedExecutionState, byTarget map[string]int) ([]int, int) {
	ready := make([]int, 0, len(targets))
	skipped := 0
	for {
		propagatedSkip := false
		for index, target := range targets {
			if states[index] != unifiedStatePending {
				continue
			}
			switch unifiedDependencyState(target.dependsOn, states, byTarget) {
			case unifiedStateSucceeded:
				states[index] = unifiedStateScheduled
				ready = append(ready, index)
			case unifiedStateFailed:
				states[index] = unifiedStateSkipped
				results[index] = &enginev1.UnifiedTargetResult{
					Target: target.name, Status: "skipped", ErrorCode: "dependency_failed",
				}
				skipped++
				propagatedSkip = true
			}
		}
		if !propagatedSkip {
			return ready, skipped
		}
	}
}

// unifiedDependencyState returns succeeded when every dependency succeeded,
// failed when any failed/skipped, and scheduled while at least one is pending.
func unifiedDependencyState(dependencies []string, states []unifiedExecutionState, byTarget map[string]int) unifiedExecutionState {
	state := unifiedStateSucceeded
	for _, dependency := range dependencies {
		switch states[byTarget[dependency]] {
		case unifiedStateFailed, unifiedStateSkipped:
			return unifiedStateFailed
		case unifiedStateSucceeded:
			continue
		default:
			state = unifiedStateScheduled
		}
	}
	return state
}

// unifiedDependencyResponses copies only the direct dependency responses
// admitted for one mapping, avoiding concurrent reads from the shared map.
func unifiedDependencyResponses(target preparedUnifiedTarget, responses map[string]any) map[string]any {
	if len(target.dependsOn) == 0 {
		return nil
	}
	selected := make(map[string]any, len(target.dependsOn))
	for _, dependency := range target.dependsOn {
		selected[dependency] = responses[dependency]
	}
	return selected
}

// runUnifiedForward executes one scheduled target and returns its graph/public
// outcomes to the single scheduler owner.
func (s *EngineGRPCServer) runUnifiedForward(ctx context.Context, call preparedUnifiedCall, index int, responses map[string]any, completions chan<- unifiedForwardCompletion) {
	outcome := s.executeUnifiedTarget(ctx, call, call.targets[index], responses)
	completions <- unifiedForwardCompletion{index: index, outcome: outcome}
}

// executeUnifiedRollbacks runs the direct-only deduplicated rollback plan in
// reverse dependency order; rollback failure never gates another compensation.
func (s *EngineGRPCServer) executeUnifiedRollbacks(ctx context.Context, call preparedUnifiedCall, results []*enginev1.UnifiedTargetResult, states []unifiedExecutionState, responses map[string]any, byTarget map[string]int) []*enginev1.UnifiedRollbackResult {
	plans := planUnifiedRollbacks(call.targets, states, byTarget)
	if len(plans) == 0 {
		return nil
	}
	rollbackResults := make([]*enginev1.UnifiedRollbackResult, len(plans))
	// caller cancellation must not suppress compensation after confirmed
	// side effects; each physical rollback still owns its execution-policy bound.
	rollbackCtx := context.WithoutCancel(ctx)
	rollbackStates := make([]unifiedExecutionState, len(plans))
	ready := discoverUnifiedRollbackWork(plans, call.targets, rollbackStates)
	completions := make(chan unifiedRollbackCompletion, len(plans))
	completed, running := 0, 0
	for completed < len(plans) {
		for running < maxUnifiedConcurrency && len(ready) > 0 {
			position := ready[0]
			ready = ready[1:]
			running++
			go s.runUnifiedRollback(rollbackCtx, call, position, plans[position], responses, completions)
		}
		// reverse ordering over an admitted DAG always exposes a leaf; this
		// guard avoids a deadlock if in-memory state is ever corrupted.
		if running == 0 {
			break
		}
		completion := <-completions
		running--
		completed++
		rollbackResults[completion.position] = completion.result
		rollbackStates[completion.position] = unifiedStateSucceeded
		ready = append(ready, discoverUnifiedRollbackWork(plans, call.targets, rollbackStates)...)
	}
	return rollbackResults
}

// planUnifiedRollbacks selects successful direct dependencies of physical input
// failures and dependency-skipped bindings, deduplicating shared targets.
func planUnifiedRollbacks(targets []preparedUnifiedTarget, states []unifiedExecutionState, byTarget map[string]int) []plannedUnifiedRollback {
	triggers := make(map[int][]string)
	for failedIndex, state := range states {
		if state != unifiedStateFailed && state != unifiedStateSkipped {
			continue
		}
		for _, dependency := range targets[failedIndex].dependsOn {
			dependencyIndex := byTarget[dependency]
			// only confirmed successful direct dependencies with an explicit
			// rollback are eligible; failed nodes, siblings, and ancestors are inert.
			if states[dependencyIndex] != unifiedStateSucceeded || targets[dependencyIndex].rollback == nil {
				continue
			}
			triggers[dependencyIndex] = appendUniqueUnifiedTrigger(triggers[dependencyIndex], targets[failedIndex].name)
		}
	}
	plans := make([]plannedUnifiedRollback, 0, len(triggers))
	for index := range targets {
		if triggeredBy := triggers[index]; len(triggeredBy) > 0 {
			plans = append(plans, plannedUnifiedRollback{targetIndex: index, triggeredBy: triggeredBy})
		}
	}
	return plans
}

// appendUniqueUnifiedTrigger preserves request order while ensuring repeated
// failure paths cannot duplicate one rollback trigger label.
func appendUniqueUnifiedTrigger(values []string, target string) []string {
	for _, value := range values {
		if value == target {
			return values
		}
	}
	return append(values, target)
}

// discoverUnifiedRollbackWork schedules planned targets only after every
// planned direct consumer has completed compensation, regardless of outcome.
func discoverUnifiedRollbackWork(plans []plannedUnifiedRollback, targets []preparedUnifiedTarget, states []unifiedExecutionState) []int {
	ready := make([]int, 0, len(plans))
	for position, plan := range plans {
		if states[position] != unifiedStatePending {
			continue
		}
		if unifiedRollbackConsumersSettled(plan.targetIndex, plans, targets, states) {
			states[position] = unifiedStateScheduled
			ready = append(ready, position)
		}
	}
	return ready
}

// unifiedRollbackConsumersSettled enforces reverse order only among explicitly
// planned direct targets and never adds an undeclared ancestor.
func unifiedRollbackConsumersSettled(targetIndex int, plans []plannedUnifiedRollback, targets []preparedUnifiedTarget, states []unifiedExecutionState) bool {
	dependencyName := targets[targetIndex].name
	for position, candidate := range plans {
		if candidate.targetIndex == targetIndex {
			continue
		}
		if containsUnifiedTarget(targets[candidate.targetIndex].dependsOn, dependencyName) && states[position] != unifiedStateSucceeded {
			return false
		}
	}
	return true
}

// containsUnifiedTarget tests a bounded in-memory dependency list.
func containsUnifiedTarget(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// runUnifiedRollback executes one ready compensation and returns its result to
// the single reverse-order scheduler owner.
func (s *EngineGRPCServer) runUnifiedRollback(ctx context.Context, call preparedUnifiedCall, position int, plan plannedUnifiedRollback, responses map[string]any, completions chan<- unifiedRollbackCompletion) {
	result := s.executeUnifiedRollback(ctx, call, plan, responses)
	completions <- unifiedRollbackCompletion{position: position, result: result}
}

// executeUnifiedRollback maps the compensated target's own raw response and
// reports only bounded status/auth metadata, never the rollback provider body.
func (s *EngineGRPCServer) executeUnifiedRollback(ctx context.Context, call preparedUnifiedCall, plan plannedUnifiedRollback, responses map[string]any) *enginev1.UnifiedRollbackResult {
	target := call.targets[plan.targetIndex]
	result := &enginev1.UnifiedRollbackResult{Target: target.name, TriggeredBy: append([]string(nil), plan.triggeredBy...)}
	responseContext := map[string]any{target.name: responses[target.name]}
	request, err := prepareUnifiedPhysicalRequest(call, target, target.rollback.input, responseContext, "rollback")
	// Failed compensation mappings remain logical diagnostics, never provider attempts.
	if err != nil {
		result.Status, result.ErrorCode = "error", "input_mapping_failed"
		return result
	}
	ctx = executionevent.WithUnifiedChild(ctx, call.receiptID, target.name, "rollback")
	err = s.unifiedRuntime.ExecuteResolvedPhysicalSuccess(ctx, call.identity, target.rollback.operation, request)
	// Compensation failure is reported independently from the forward failure.
	if err != nil {
		classified := classifyUnifiedPhysicalError(err)
		result.Status, result.ErrorCode, result.AuthAction = "error", classified.code, classified.action
		return result
	}
	result.Status = "success"
	return result
}
