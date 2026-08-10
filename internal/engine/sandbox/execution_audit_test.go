package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/executionevent"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

type captureJetStreamPublisher struct {
	message *nats.Msg
}

type fixedPolicyValidator struct {
	identity auth.RuntimeIdentity
}

func (validator fixedPolicyValidator) Validate(_ context.Context, appID uuid.UUID, _ string) (auth.RuntimeIdentity, error) {
	validator.identity.AppID = appID
	return validator.identity, nil
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
	engine.AddExecutionTiming(ctx, "provider_total", 5*time.Millisecond)
	engine.AddExecutionTiming(ctx, "provider_total", 7*time.Millisecond)
	engine.RecordExecutionCount(ctx, "provider_attempt_count", 2)

	serviceID := uuid.New()
	operationID := uuid.New()
	appID := uuid.New()
	appFamilyID := uuid.New()
	accountID := uuid.New()
	startedAt := time.Now().Add(-25 * time.Millisecond)
	recordEngineExecutionAudit(ctx, trace.SpanFromContext(ctx), executionAuditState{
		identity: auth.RuntimeIdentity{AccountID: accountID, AppFamilyID: appFamilyID, AppID: appID, AppVersion: "1.0.0", Kind: "sdk", Status: "active"}, endpointName: "repos.list", startedAt: startedAt,
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
	assertCompactSafeExecutionEvent(t, envelope.Event, appID, accountID, serviceID, operationID)
}

// TestRecordEngineExecutionAuditRequiresAccountIDForActivityVisibility guards
// the exact regression this bug was: models.EngineExecutionEvent.AccountID
// must be populated from executionAuditState.accountID, or the event is
// stored with a NULL account_id and becomes permanently invisible to
// GetWorkspaceExecutionAnalytics -- a successful execution that never shows
// up on the Activity page, with no error anywhere in the path.
func TestRecordEngineExecutionAuditRequiresAccountIDForActivityVisibility(t *testing.T) {
	capture := &captureJetStreamPublisher{}
	executionevent.SetPublisher(executionevent.NewPublisher(capture))
	defer executionevent.SetPublisher(nil)

	accountID := uuid.New()
	recordEngineExecutionAudit(context.Background(), trace.SpanFromContext(context.Background()), executionAuditState{
		identity: auth.RuntimeIdentity{AccountID: accountID, AppFamilyID: uuid.New(), AppID: uuid.New(), AppVersion: "1.0.0", Kind: "sdk", Status: "active"}, endpointName: "repos.list", startedAt: time.Now(),
	}, nil)

	var envelope models.EngineExecutionEventEnvelope
	if err := json.Unmarshal(capture.message.Data, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Event.AccountID != accountID {
		t.Fatalf("event.AccountID = %s, want %s (Activity page would show \"No calls\" for this account)", envelope.Event.AccountID, accountID)
	}
}

func TestRecordEngineExecutionAuditTreatsProviderAuthResponseAsFailure(t *testing.T) {
	capture := &captureJetStreamPublisher{}
	executionevent.SetPublisher(executionevent.NewPublisher(capture))
	defer executionevent.SetPublisher(nil)

	recordEngineExecutionAudit(context.Background(), trace.SpanFromContext(context.Background()), executionAuditState{
		identity: auth.RuntimeIdentity{AccountID: uuid.New(), AppFamilyID: uuid.New(), AppID: uuid.New(), AppVersion: "1.0.0", Kind: "sdk", Status: "active"}, endpointName: "repos.list", startedAt: time.Now(), providerHTTPStatus: 401,
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

func TestEngineExecuteCoreAuditsAndTracesTokenScopeDenial(t *testing.T) {
	capture := &captureJetStreamPublisher{}
	executionevent.SetPublisher(executionevent.NewPublisher(capture))
	t.Cleanup(func() { executionevent.SetPublisher(nil) })

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})

	appID := uuid.New()
	validator := fixedPolicyValidator{identity: auth.RuntimeIdentity{
		AccountID: uuid.New(), AppFamilyID: uuid.New(), AppVersion: "1.0.0",
		Kind: store.AppKindMCP, Status: store.AppStatusActive,
		TokenPolicy: store.AppTokenPolicy{AllowedOperations: []string{"users.get"}},
	}}
	err := engineExecuteCore(context.Background(), nil, nil, validator, appID.String(), "secret-token", "users.delete", nil, nil, "", engine.NewBufferStream())
	if err == nil {
		t.Fatal("operation outside token policy was allowed")
	}
	if capture.message == nil {
		t.Fatal("token scope denial did not emit the canonical execution audit")
	}
	var envelope models.EngineExecutionEventEnvelope
	if err := json.Unmarshal(capture.message.Data, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Event.FailureCategory != "policy" || envelope.Event.FailureCode != "scope_denied" {
		t.Fatalf("scope denial classification = %s/%s", envelope.Event.FailureCategory, envelope.Event.FailureCode)
	}

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("execution span count = %d, want 1", len(spans))
	}
	attributes := make(map[attribute.Key]string, len(spans[0].Attributes()))
	for _, value := range spans[0].Attributes() {
		if value.Value.Type() == attribute.STRING {
			attributes[value.Key] = value.Value.AsString()
		}
	}
	if attributes["authorization.outcome"] != "denied" || attributes["execution.failure_code"] != "scope_denied" {
		t.Fatalf("scope denial OTEL attributes = %#v", attributes)
	}
}

func TestEngineExecuteCoreRequiresTokenAndAppScope(t *testing.T) {
	appID := uuid.New()
	serviceID := uuid.New()
	selectedEndpointID := uuid.New()
	otherEndpointID := uuid.New()
	appScope := func(endpointID uuid.UUID) *richMockCache {
		selections, err := json.Marshal([]models.SDKSelection{{
			ServiceID:   serviceID,
			EndpointIDs: []uuid.UUID{endpointID},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return &richMockCache{scopeJSON: selections, obj: &fusedobject.ServiceMetadata{}, epID: selectedEndpointID}
	}
	validator := func(operations ...string) fixedPolicyValidator {
		return fixedPolicyValidator{identity: auth.RuntimeIdentity{
			AccountID: uuid.New(), AppFamilyID: uuid.New(), AppVersion: "1.0.0",
			Kind: store.AppKindMCP, Status: store.AppStatusActive,
			TokenPolicy: store.AppTokenPolicy{AllowedOperations: operations},
		}}
	}

	tests := []struct {
		name          string
		cache         ObjectCache
		validator     auth.TokenValidator
		wantErrorText string
	}{
		{
			name: "app selected but token denied",
			// A nil cache proves token authorization runs before app-scope lookup.
			validator:     validator("users.get"),
			wantErrorText: "operation not allowed by token",
		},
		{
			name:          "token allowed but app unselected",
			cache:         appScope(otherEndpointID),
			validator:     validator("list_items"),
			wantErrorText: "unauthorized access to endpoint",
		},
		{
			name:          "token and app both allow",
			cache:         appScope(selectedEndpointID),
			validator:     validator("list_items"),
			wantErrorText: "dispatcher not initialized",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := engineExecuteCore(context.Background(), test.cache, nil, test.validator, appID.String(), "secret-token", "list_items", nil, nil, "", engine.NewBufferStream())
			if err == nil || !strings.Contains(err.Error(), test.wantErrorText) {
				t.Fatalf("engineExecuteCore() error = %v, want text %q", err, test.wantErrorText)
			}
		})
	}
}

func assertCompactSafeExecutionEvent(t *testing.T, event models.EngineExecutionEvent, appID, accountID, serviceID, operationID uuid.UUID) {
	t.Helper()
	checks := []struct {
		valid   bool
		message string
	}{
		{executionEventIdentityMatches(event, appID, accountID, serviceID, operationID), "unexpected event ids"},
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

func executionEventIdentityMatches(event models.EngineExecutionEvent, appID, accountID, serviceID, operationID uuid.UUID) bool {
	return event.AppID == appID && event.AccountID == accountID && event.ServiceID == serviceID && event.OperationID == operationID
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
