package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/executionevent"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/trace"
)

type captureJetStreamPublisher struct {
	message *nats.Msg
}

func (p *captureJetStreamPublisher) PublishMsgJS(message *nats.Msg) (*nats.PubAck, error) {
	p.message = message
	return &nats.PubAck{}, nil
}

func TestRecordEngineExecutionAuditPublishesCompactSafeEvent(t *testing.T) {
	capture := &captureJetStreamPublisher{}
	executionevent.SetPublisher(executionevent.NewPublisher(capture))
	defer executionevent.SetPublisher(nil)

	timings := engine.NewExecutionTimings()
	ctx := engine.ContextWithExecutionTimings(context.Background(), timings)
	ctx = contextWithExecutionIdentity(ctx, "raw-idempotency-key", "request-body-hash")
	ctx = contextWithExecutionTransport(ctx, models.EngineExecutionTransportSDK)
	engine.RecordExecutionTiming(ctx, "provider_total", 12*time.Millisecond)
	engine.RecordExecutionCount(ctx, "provider_attempt_count", 2)

	serviceID := uuid.New()
	operationID := uuid.New()
	artifactID := uuid.New()
	startedAt := time.Now().Add(-25 * time.Millisecond)
	recordEngineExecutionAudit(ctx, trace.SpanFromContext(ctx), executionAuditState{
		artifactID: artifactID, endpointName: "repos.list", startedAt: startedAt,
		match: &scopedEndpoint{
			service: &fusedobject.ServiceMetadata{ID: serviceID}, serviceVersionID: "version-1",
			endpoint: fusedobject.Endpoint{ID: operationID, Method: "GET", NormalizedPath: "/repos"},
		},
		selectedEnvironment: "production", environmentSource: "provider",
		providerHost: "api.example.com", providerHTTPStatus: 502,
	}, errors.New("provider failed"))

	if capture.message == nil {
		t.Fatal("expected a canonical event publication")
	}
	var envelope models.EngineExecutionEventEnvelope
	if err := json.Unmarshal(capture.message.Data, &envelope); err != nil {
		t.Fatal(err)
	}
	assertCompactSafeExecutionEvent(t, envelope.Event, artifactID, serviceID, operationID)
}

func TestRecordEngineExecutionAuditTreatsProviderAuthResponseAsFailure(t *testing.T) {
	capture := &captureJetStreamPublisher{}
	executionevent.SetPublisher(executionevent.NewPublisher(capture))
	defer executionevent.SetPublisher(nil)

	recordEngineExecutionAudit(context.Background(), trace.SpanFromContext(context.Background()), executionAuditState{
		artifactID: uuid.New(), endpointName: "repos.list", startedAt: time.Now(), providerHTTPStatus: 401,
	}, nil)

	var envelope models.EngineExecutionEventEnvelope
	if err := json.Unmarshal(capture.message.Data, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Event.Status != models.EngineExecutionStatusFailed || envelope.Event.FailureCategory != "auth" || envelope.Event.FailureCode != "provider_auth" {
		t.Fatalf("provider auth response was not classified as a failure: %#v", envelope.Event)
	}
	if envelope.Event.FailureReason != "provider returned HTTP 401" {
		t.Fatalf("failure reason = %q", envelope.Event.FailureReason)
	}
}

func assertCompactSafeExecutionEvent(t *testing.T, event models.EngineExecutionEvent, artifactID, serviceID, operationID uuid.UUID) {
	t.Helper()
	checks := []struct {
		valid   bool
		message string
	}{
		{executionEventIdentityMatches(event, artifactID, serviceID, operationID), "unexpected event ids"},
		{executionEventFailureMatches(event), "unexpected failure classification"},
		{executionEventHashIsSafe(event), "idempotency key was not safely hashed"},
		{executionEventProviderMetricsMatch(event), "unexpected provider metrics"},
		{executionEventProviderIdentityMatches(event), "unexpected provider identity"},
	}
	for _, check := range checks {
		if !check.valid {
			t.Fatalf("%s: %#v", check.message, event)
		}
	}
}

func executionEventIdentityMatches(event models.EngineExecutionEvent, artifactID, serviceID, operationID uuid.UUID) bool {
	return event.ArtifactID == artifactID && event.ServiceID == serviceID && event.OperationID == operationID
}

func executionEventFailureMatches(event models.EngineExecutionEvent) bool {
	return event.Status == models.EngineExecutionStatusFailed && event.FailureCategory == "provider"
}

func executionEventHashIsSafe(event models.EngineExecutionEvent) bool {
	return event.IdempotencyKeyHash != "" && event.IdempotencyKeyHash != "raw-idempotency-key"
}

func executionEventProviderMetricsMatch(event models.EngineExecutionEvent) bool {
	return event.ProviderLatencyMs != nil && *event.ProviderLatencyMs == 12 && event.AttemptCount == 2
}

func executionEventProviderIdentityMatches(event models.EngineExecutionEvent) bool {
	return event.ProviderStatusClass == "5xx" && event.HTTPMethod == "GET" && event.RequestPath == "/repos"
}
