package api

import (
	"context"
	"errors"

	"github.com/Usefused/engine/internal/engine"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/unified"
)

// unifiedTargetOutcome keeps the raw response used by dependency/rollback
// mappings separate from the effective value exposed to final output mapping.
type unifiedTargetOutcome struct {
	result           *enginev1.UnifiedTargetResult
	response         any
	output           any
	outputReady      bool
	forwardSucceeded bool
}

// executeUnifiedTarget maps one ready target, enters the existing physical
// boundary, and retains its raw response only for in-call dependencies.
func (s *EngineGRPCServer) executeUnifiedTarget(ctx context.Context, call preparedUnifiedCall, target preparedUnifiedTarget, responses map[string]any) unifiedTargetOutcome {
	request, err := prepareUnifiedPhysicalRequest(call, target, target.input, responses, "forward")
	if err != nil {
		return unifiedTargetOutcome{result: unifiedErrorResult(target.name, "input_mapping_failed", nil)}
	}
	physical, err := s.unifiedRuntime.ExecuteResolvedPhysicalJSON(ctx, call.identity, target.operation, request)
	if err != nil {
		classified := classifyUnifiedPhysicalError(err)
		return unifiedTargetOutcome{result: unifiedErrorResult(target.name, classified.code, classified.action)}
	}
	data, response, output, responseReady, code := projectUnifiedOutput(call.input, target, physical.Body)
	// projection belongs outside the provider call. A bad user mapping is a
	// public result error, but the confirmed raw response remains usable by direct
	// dependants and must not trigger compensation.
	if code != "" {
		return unifiedTargetOutcome{
			result: unifiedErrorResult(target.name, code, nil), response: response,
			forwardSucceeded: responseReady,
		}
	}
	return unifiedTargetOutcome{
		result:   &enginev1.UnifiedTargetResult{Target: target.name, Status: "success", DataJson: data},
		response: response, output: output, outputReady: true, forwardSucceeded: true,
	}
}

// prepareUnifiedPhysicalRequest derives mapped params and deterministic replay
// identity for either a forward or rollback child.
func prepareUnifiedPhysicalRequest(call preparedUnifiedCall, target preparedUnifiedTarget, program *unified.Program, responses map[string]any, phase string) (sandbox.PhysicalExecutionRequest, error) {
	params, canonical, err := evaluateUnifiedInput(program, call.input, target.name, responses)
	if err != nil {
		return sandbox.PhysicalExecutionRequest{}, err
	}
	pagination := forwardPaginationIntent(target, phase)
	idempotencyKey, requestHash := deriveUnifiedChildIdentity(
		call.appID, call.operation, target.name, phase, call.idempotencyKey, canonical, target.selector,
	)
	return sandbox.PhysicalExecutionRequest{
		Params: params, Credentials: unifiedSelectorCredentials(target.selector),
		Environment:    unifiedSelectorEnvironment(target.selector),
		IdempotencyKey: idempotencyKey, RequestBodyHash: requestHash,
		Pagination: pagination, Transport: call.transport,
	}, nil
}

// forwardPaginationIntent prevents a caller's result-size preference from changing compensation behavior.
func forwardPaginationIntent(target preparedUnifiedTarget, phase string) *engine.PaginationIntent {
	// Rollbacks execute their immutable contract fully and never inherit forward target controls.
	if phase != "forward" {
		return nil
	}
	return target.pagination
}

// projectUnifiedOutput decodes one successful provider body and applies only
// the current target's configured output projection.
func projectUnifiedOutput(input any, target preparedUnifiedTarget, responseBody []byte) ([]byte, any, any, bool, string) {
	canonical, response, err := decodeCanonicalUnifiedValue(responseBody)
	if err != nil {
		return nil, nil, nil, false, "response_not_json"
	}
	if target.output == nil {
		return canonical, response, response, true, ""
	}
	mapped, err := target.output.program.Evaluate(unified.EvaluationContext{
		Input: input, Target: target.name, Response: response,
	})
	if err != nil || unified.IsOmitted(mapped) {
		return nil, response, nil, true, "output_mapping_failed"
	}
	if err := target.output.schema.VisitJSON(mapped); err != nil {
		return nil, response, nil, true, "output_validation_failed"
	}
	canonical, err = encodeCanonicalUnifiedValue(mapped)
	if err != nil {
		return nil, response, nil, true, "output_encoding_failed"
	}
	return canonical, response, mapped, true, ""
}

// projectUnifiedOperationOutput evaluates one final result after every selected binding has settled.
func projectUnifiedOperationOutput(input any, output *preparedUnifiedOutput, responses map[string]any) ([]byte, string) {
	if output == nil {
		return nil, ""
	}
	mapped, err := output.program.Evaluate(unified.EvaluationContext{Input: input, Responses: responses})
	if err != nil || unified.IsOmitted(mapped) {
		return nil, "output_mapping_failed"
	}
	if err := output.schema.VisitJSON(mapped); err != nil {
		return nil, "output_validation_failed"
	}
	encoded, err := encodeCanonicalUnifiedValue(mapped)
	if err != nil {
		return nil, "output_encoding_failed"
	}
	return encoded, ""
}

type classifiedUnifiedError struct {
	code   string
	action *enginev1.UnifiedAuthAction
}

// classifyUnifiedPhysicalError translates internal provider/auth failures into
// stable, bounded per-target outcomes.
func classifyUnifiedPhysicalError(err error) classifiedUnifiedError {
	if action := unifiedAuthAction(err); action != nil {
		return classifiedUnifiedError{code: unifiedAuthErrorCode(err), action: action}
	}
	switch {
	case errors.Is(err, context.Canceled):
		return classifiedUnifiedError{code: "cancelled"}
	case errors.Is(err, context.DeadlineExceeded):
		return classifiedUnifiedError{code: "deadline_exceeded"}
	case errors.Is(err, sandbox.ErrPhysicalResponseTooLarge):
		return classifiedUnifiedError{code: "response_too_large"}
	case errors.Is(err, sandbox.ErrPhysicalResponseNotJSON):
		return classifiedUnifiedError{code: "response_not_json"}
	case errors.Is(err, sandbox.ErrPhysicalResponseStatus):
		return classifiedUnifiedError{code: "provider_error"}
	default:
		return classifiedUnifiedError{code: "execution_failed"}
	}
}

// unifiedAuthAction extracts only non-secret Engine routing identifiers from a
// typed connected-auth failure.
func unifiedAuthAction(err error) *enginev1.UnifiedAuthAction {
	var connection *sandbox.ConnectionRequiredError
	if errors.As(err, &connection) {
		return &enginev1.UnifiedAuthAction{
			Action: "connect", BucketId: connection.BucketID, ServiceId: connection.ServiceID,
			EndUserRef: connection.EndUserRef,
		}
	}
	var reconnect *sandbox.ReconnectRequiredError
	if errors.As(err, &reconnect) {
		return &enginev1.UnifiedAuthAction{
			Action: "reconnect", BucketId: reconnect.BucketID, ServiceId: reconnect.ServiceID,
			EndUserRef: reconnect.EndUserRef, ConnectionId: reconnect.ConnectionID, Reason: reconnect.Reason,
		}
	}
	var resource *sandbox.ResourceSelectionRequiredError
	if errors.As(err, &resource) {
		return &enginev1.UnifiedAuthAction{
			Action: "select_resource", BucketId: resource.BucketID, ServiceId: resource.ServiceID,
			EndUserRef: resource.EndUserRef, ConnectionId: resource.ConnectionID, Reason: resource.Reason,
		}
	}
	return nil
}

// unifiedAuthErrorCode returns the matching stable code for an actionable auth
// error after its typed action has been detected.
func unifiedAuthErrorCode(err error) string {
	var connection *sandbox.ConnectionRequiredError
	if errors.As(err, &connection) {
		return "connection_required"
	}
	var reconnect *sandbox.ReconnectRequiredError
	if errors.As(err, &reconnect) {
		return "reconnect_required"
	}
	return "resource_selection_required"
}

// unifiedErrorResult creates a data-free target error outcome.
func unifiedErrorResult(target, code string, action *enginev1.UnifiedAuthAction) *enginev1.UnifiedTargetResult {
	return &enginev1.UnifiedTargetResult{Target: target, Status: "error", ErrorCode: code, AuthAction: action}
}
