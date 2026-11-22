package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/executionevent"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/unified"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// publishUnifiedAppReceipt feeds authored failures into the existing event worker, including runs with no provider calls.
func (s *EngineGRPCServer) publishUnifiedAppReceipt(ctx context.Context, spec capabilityRunSpec, id uuid.UUID, recorder *recordingCapabilityHost) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	repository, ok := s.store.(store.ExecutionResultStore)
	// Older stores without retained results cannot publish a guessed outcome.
	if !ok {
		return
	}
	record, err := repository.GetExecutionResult(ctx, spec.identity.AccountID, spec.identity.AppID, id)
	// A terminal durable result is the sole authority for a logical receipt.
	if err != nil || record.CompletedAt == nil {
		return
	}
	event := unifiedAppReceipt(ctx, record, spec)
	// Only recorded calls contribute steps; scripts with no effects keep an empty list.
	if recorder != nil {
		event.UnifiedSteps = recorder.receiptSteps()
		event.Timings = recorder.phaseTimings()
		// Only a failed durable outcome may expose a bounded worker failure stage to ordinary receipt readers.
		if event.Status == models.EngineExecutionStatusFailed {
			event.FailureReason = recorder.receiptFailureReason()
		}
	}
	// The existing publisher owns deduplication and never carries diagnostic payloads.
	if err := executionevent.Publish(ctx, event); err != nil {
		trace.SpanFromContext(ctx).SetStatus(codes.Error, "execution_receipt_unavailable")
	}
}

// receiptFailureReason retains the observed stage without copying private exception text into activity.
func (host *recordingCapabilityHost) receiptFailureReason() string {
	host.mu.Lock()
	defer host.mu.Unlock()
	for _, phase := range host.phases {
		// A completed stage must not be blamed merely because it was the last timing received.
		if phase.Failed {
			return "unified_app_" + telemetryFailurePhase(phase.Name) + "_failed"
		}
	}
	return ""
}

// unifiedAppReceipt contains bounded summary metadata while sensitive values stay encrypted on the result.
func unifiedAppReceipt(ctx context.Context, record *store.ExecutionResult, spec capabilityRunSpec) models.EngineExecutionEvent {
	status := models.EngineExecutionStatusFailed
	// Failed and indeterminate terminal states both count as unsuccessful calls.
	if record.Status == "succeeded" {
		status = models.EngineExecutionStatusSuccess
	}
	transport := models.EngineExecutionTransportREST
	// The direct MCP entry point marks its transport before delegating to the shared runner.
	if spec.transport == "mcp" || spec.identity.Kind == store.AppKindMCP {
		transport = models.EngineExecutionTransportMCP
	}
	// Attached SDK calls retain their consumer transport while executing the hosted version.
	if spec.transport == "sdk" {
		transport = models.EngineExecutionTransportSDK
	}
	// Automatic provider events retain webhook transport while sharing the normal logical app receipt.
	if spec.transport == "webhook" {
		transport = models.EngineExecutionTransportWebhook
	}
	// Timed triggers use canonical execution receipts without impersonating an HTTP caller.
	if spec.transport == "schedule" {
		transport = models.EngineExecutionTransportSchedule
	}
	event := models.EngineExecutionEvent{ID: record.ID, AccountID: record.AccountID, AppFamilyID: record.AppFamilyID, AppID: record.AppID, AppTokenID: record.AppTokenID, AppVersion: record.AppVersion, ExecutionKind: "unified", EndpointName: "execute", Transport: transport, Direction: models.EngineExecutionDirectionOutbound, Status: status, FailureCode: record.ErrorCode, StartedAt: record.CreatedAt, EndedAt: *record.CompletedAt, LatencyMs: record.CompletedAt.Sub(record.CreatedAt).Milliseconds()}
	spanContext := trace.SpanContextFromContext(ctx)
	// Disabled tracing must not fabricate all-zero trace IDs in durable history.
	if spanContext.IsValid() {
		event.TraceID = spanContext.TraceID().String()
		event.SpanID = spanContext.SpanID().String()
	}
	return event
}

// receiptSteps exposes fixed outcome metadata while request, response, and errors remain private.
func (host *recordingCapabilityHost) receiptSteps() []models.UnifiedExecutionStep {
	host.mu.Lock()
	defer host.mu.Unlock()
	steps := make([]models.UnifiedExecutionStep, 0, len(host.calls))
	for _, call := range host.calls {
		operation, kind, include := receiptStepDescriptor(call.Request)
		// Internal snapshot checkpoints are not external executions and never emit physical receipts.
		if !include {
			continue
		}
		step := models.UnifiedExecutionStep{
			Target:    fmt.Sprintf("call_%d", call.Ordinal),
			Operation: operation,
			Kind:      kind,
			Phase:     "forward",
			Status:    "success",
		}
		// Pending calls after cancellation cannot be represented as successful provider work.
		if call.Error != "" || call.Completion == 0 {
			step.Status = "error"
			step.ErrorCode = "execution_failed"
		}
		steps = append(steps, step)
	}
	return steps
}

// receiptStepDescriptor classifies a recorded host call into bounded receipt metadata without leaking inputs.
func receiptStepDescriptor(raw json.RawMessage) (operation, kind string, include bool) {
	var envelope struct {
		Snapshot   string `json:"snapshot"`
		UnifiedApp string `json:"unifiedApp"`
		Bridge     string `json:"bridge"`
		Operation  string `json:"operation"`
	}
	// Preserve unparseable call entries as unnamed steps rather than dropping failure evidence.
	if len(raw) == 0 || json.Unmarshal(raw, &envelope) != nil {
		return "", "", true
	}
	// Snapshot reads and writes are internal Engine checkpoints, not external forward executions.
	if strings.TrimSpace(envelope.Snapshot) != "" {
		return "", "", false
	}
	// Nested Unified App calls run under the called app's family rather than emitting a physical child receipt here.
	if alias := strings.TrimSpace(envelope.UnifiedApp); alias != "" {
		return safeUnifiedStepOperation(alias), "unified_app", true
	}
	// Bridge calls match the canonical "bridge:<alias>" operation label used in physical Bridge receipts.
	if alias := strings.TrimSpace(envelope.Bridge); alias != "" {
		return safeUnifiedStepOperation("bridge:" + alias), "bridge", true
	}
	// Workspace provider operations retain their selected operation name so pre-dispatch failures stay readable.
	if op := strings.TrimSpace(envelope.Operation); op != "" {
		return safeUnifiedStepOperation(op), "operation", true
	}
	return "", "", true
}

// safeUnifiedStepOperation keeps optional display names within the canonical public-name validator contract.
func safeUnifiedStepOperation(value string) string {
	// Reject malformed or overlong authored names instead of causing receipt validation to drop the entire event.
	if !unified.ValidPublicName(value, 256) {
		return ""
	}
	return value
}
