package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/Usefused/engine/internal/engine/executionevent"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

// executePreparedUnified records one logical outcome only after authorization and
// preflight, while the physical runtime remains the sole provider usage owner.
func (s *EngineGRPCServer) executePreparedUnified(ctx context.Context, call preparedUnifiedCall, started time.Time) *enginev1.ExecuteUnifiedResponse {
	call.receiptID = uuid.New()
	graph := s.executeUnifiedGraph(ctx, call)
	output, outputCode := projectUnifiedOperationOutput(call.input, call.output, graph.outputs)
	response := &enginev1.ExecuteUnifiedResponse{
		Results: graph.results, RollbackResults: graph.rollbacks,
		OutputJson: output, OutputErrorCode: outputCode,
	}
	event := unifiedReceipt(ctx, call, response, started, time.Now())
	// Audit transport failures must not replay a provider call that already completed.
	if err := executionevent.Publish(ctx, event); err != nil {
		slog.ErrorContext(ctx, "Failed to publish Unified execution receipt", slog.String("error_code", "execution_event_publication_failed"), slog.String("event_id", event.ID.String()))
	}
	return response
}

// unifiedReceipt retains bounded diagnostics, never the mapped input, provider
// response, or auth actions that may contain a user-specific routing reference.
func unifiedReceipt(ctx context.Context, call preparedUnifiedCall, response *enginev1.ExecuteUnifiedResponse, started, ended time.Time) models.EngineExecutionEvent {
	event := models.EngineExecutionEvent{
		ID: call.receiptID, ExecutionKind: "unified", AccountID: call.identity.AccountID,
		AppFamilyID: call.identity.AppFamilyID, AppID: call.appID, AppTokenID: call.identity.TokenID,
		AppVersion: call.identity.AppVersion, Transport: call.transport,
		Direction: models.EngineExecutionDirectionOutbound, EndpointName: call.operation,
		Status: models.EngineExecutionStatusSuccess, StartedAt: started, EndedAt: ended,
		CreatedAt: ended, LatencyMs: ended.Sub(started).Milliseconds(),
		UnifiedSteps: unifiedReceiptSteps(response),
	}
	// A successful compensation does not erase the original forward failure.
	for _, step := range event.UnifiedSteps {
		// Skipped dependants are incomplete work, even when they made no provider request.
		if step.Status != "success" {
			event.Status, event.FailureCode = models.EngineExecutionStatusFailed, "unified_target_failed"
		}
	}
	// Final output projection is part of the logical outcome, not a provider failure.
	if response.GetOutputErrorCode() != "" {
		event.Status, event.FailureCode = models.EngineExecutionStatusFailed, response.GetOutputErrorCode()
	}
	// Trace correlation is optional; durable parent/child IDs do not depend on exporters.
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		event.TraceID, event.SpanID = sc.TraceID().String(), sc.SpanID().String()
	}
	return event
}

// unifiedReceiptSteps copies only the bounded public execution diagnostics so
// skipped or mapping-failed targets remain visible even without a physical receipt.
func unifiedReceiptSteps(response *enginev1.ExecuteUnifiedResponse) []models.UnifiedExecutionStep {
	steps := make([]models.UnifiedExecutionStep, 0, len(response.GetResults())+len(response.GetRollbackResults()))
	// The scheduler already limits the forward graph to sixteen admitted targets.
	for _, result := range response.GetResults() {
		steps = append(steps, models.UnifiedExecutionStep{Target: result.GetTarget(), Phase: "forward", Status: result.GetStatus(), ErrorCode: result.GetErrorCode()})
	}
	// Compensations carry the compensated target name, not the provider body.
	for _, result := range response.GetRollbackResults() {
		steps = append(steps, models.UnifiedExecutionStep{Target: result.GetTarget(), Phase: "rollback", Status: result.GetStatus(), ErrorCode: result.GetErrorCode()})
	}
	return steps
}
