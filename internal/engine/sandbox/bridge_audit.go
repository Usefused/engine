package sandbox

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/executionevent"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

type bridgeReplayKey struct{}

// WithBridgeReplay marks a retained response in the normal receipt without issuing a device call.
func WithBridgeReplay(ctx context.Context) context.Context {
	return context.WithValue(ctx, bridgeReplayKey{}, true)
}

// RunBridgeInvocation keeps local operations inside ordinary quotas and canonical execution history.
func RunBridgeInvocation(ctx context.Context, identity auth.RuntimeIdentity, alias string, invoke func(context.Context) (json.RawMessage, error)) (json.RawMessage, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.dispatch.bridge")
	defer span.End()
	span.SetAttributes(attribute.String("app.id", identity.AppID.String()), attribute.String("provider.protocol", "bridge"))
	release, err := trackAuthenticatedExecution(ctx, identity, span)
	// Capacity rejection occurs before accepting command responsibility.
	if err != nil {
		return nil, err
	}
	defer release()
	started := time.Now()
	result, err := invoke(ctx)
	transport := ExecutionTransportFromContext(ctx)
	event := models.EngineExecutionEvent{ID: uuid.New(), AccountID: identity.AccountID, AppFamilyID: identity.AppFamilyID, AppID: identity.AppID, AppTokenID: identity.TokenID, AppVersion: identity.AppVersion, Transport: transport, Direction: models.EngineExecutionDirectionOutbound, EndpointName: "bridge:" + alias, ProviderProtocol: "bridge", Status: models.EngineExecutionStatusSuccess, StartedAt: started, EndedAt: time.Now(), CreatedAt: time.Now(), AttemptCount: 1}
	// Raw device errors and payloads are excluded from activity and telemetry.
	if err != nil {
		event.Status = models.EngineExecutionStatusFailed
		event.FailureReason = "Bridge execution unavailable"
	}
	event.IdempotencyReplayed, _ = ctx.Value(bridgeReplayKey{}).(bool)
	event.FailureCategory, event.FailureCode = classifyExecutionFailure(err, 0)
	event.LatencyMs = event.EndedAt.Sub(started).Milliseconds()
	event.TraceID, event.SpanID = span.SpanContext().TraceID().String(), span.SpanContext().SpanID().String()
	executionevent.AttachUnifiedChild(ctx, &event)
	_ = executionevent.Publish(context.WithoutCancel(ctx), event)
	span.SetAttributes(attribute.String("execution.outcome", event.Status))
	return result, err
}
