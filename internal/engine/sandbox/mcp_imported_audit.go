package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/executionevent"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"time"
)

// RunImportedMCPInvocation uses the ordinary account execution gate and canonical receipt publisher.
func RunImportedMCPInvocation(ctx context.Context, identity auth.RuntimeIdentity, selection models.SDKSelection, method, name string, invoke func(context.Context) (json.RawMessage, error)) (result json.RawMessage, err error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.dispatch.imported_mcp")
	defer span.End()
	span.SetAttributes(attribute.String("app.id", identity.AppID.String()), attribute.String("app.family_id", identity.AppFamilyID.String()), attribute.String("mcp.method", method), attribute.Int("execution.contract_version", models.AppSelectionSchemaVersion), attribute.Int("execution.required_capabilities_count", 1), attribute.String("execution.contract_negotiation.outcome", "accepted"))
	release, err := trackAuthenticatedExecution(ctx, identity, span)
	// Quota rejection happens before dispatch and produces no provider execution receipt.
	if err != nil {
		return nil, err
	}
	defer release()
	started := time.Now()
	result, err = invoke(ctx)
	auditErr := importedMCPResultError(result, err)
	event := importedMCPExecutionEvent(identity, selection, method, name, started, auditErr)
	// Canonical workers remain the only durable history writers, even when the client disconnects.
	event.TraceID = span.SpanContext().TraceID().String()
	event.SpanID = span.SpanContext().SpanID().String()
	_ = executionevent.Publish(context.WithoutCancel(ctx), event)
	span.SetAttributes(attribute.String("execution.outcome", event.Status), attribute.String("execution.failure_code", event.FailureCode))
	return result, err
}

// importedMCPResultError treats protocol tool failures as failed activity without discarding their standard content result.
func importedMCPResultError(result json.RawMessage, err error) error {
	// Transport failures remain authoritative even when no result was serialized.
	if err != nil {
		return err
	}
	var envelope struct {
		IsError bool `json:"isError"`
	}
	_ = json.Unmarshal(result, &envelope)
	// MCP tool errors are ordinary protocol results but unsuccessful provider work.
	if envelope.IsError {
		return errors.New("MCP tool execution failed")
	}
	return nil
}

// importedMCPExecutionEvent keeps arguments, concrete resource URIs, provider content and credentials out of durable metadata.
func importedMCPExecutionEvent(identity auth.RuntimeIdentity, selection models.SDKSelection, method, name string, started time.Time, err error) models.EngineExecutionEvent {
	// A template expansion can contain user data, so resource reads use only the closed protocol method as their activity label.
	if method == "resources/read" {
		name = models.ImportedMCPOperation(selection.ServiceID, "resource", "read")
	}
	event := models.EngineExecutionEvent{ID: uuid.New(), AccountID: identity.AccountID, AppFamilyID: identity.AppFamilyID, AppID: identity.AppID, AppTokenID: identity.TokenID, AppVersion: identity.AppVersion, ServiceID: selection.ServiceID, ServiceVersionID: selection.ServiceVersionID.String(), Transport: models.EngineExecutionTransportMCP, Direction: models.EngineExecutionDirectionOutbound, EndpointName: name, ProviderProtocol: "mcp", HTTPMethod: "POST", RequestPath: method, Status: models.EngineExecutionStatusSuccess, StartedAt: started, EndedAt: time.Now(), CreatedAt: time.Now(), AttemptCount: 1}
	// Failure classification is shared with physical receipts and never serializes raw provider diagnostics.
	if err != nil {
		event.Status = models.EngineExecutionStatusFailed
		event.FailureReason = executionFailureReason(err, 0)
	}
	event.FailureCategory, event.FailureCode = classifyExecutionFailure(err, 0)
	event.ProviderStatusClass = providerStatusClass(0, err)
	event.LatencyMs = event.EndedAt.Sub(started).Milliseconds()
	return event
}
