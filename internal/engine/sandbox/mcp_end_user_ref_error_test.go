package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/executionevent"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type mcpDynamicTokenValidator struct {
	identity auth.RuntimeIdentity
}

// Validate returns one dynamic MCP identity while preserving the app ID selected by the authenticated route.
func (validator *mcpDynamicTokenValidator) Validate(_ context.Context, appID uuid.UUID, _ string) (auth.RuntimeIdentity, error) {
	identity := validator.identity
	identity.AppID = appID
	return identity, nil
}

// TestClassifyMCPEndUserRefRequirementScopesGuidance verifies the dedicated correction cannot leak into SDK, fixed-token, or unrelated auth failures.
func TestClassifyMCPEndUserRefRequirementScopesGuidance(t *testing.T) {
	auths, requirements := testConnectedAuthContract()
	unsatisfied := &engine.AuthRoutingError{Code: "unsatisfied"}
	dynamic := auth.RuntimeIdentity{Kind: store.AppKindMCP, BindingMode: store.AppTokenBindingDynamic}
	connectionContext := contextWithMCPConnectionSelectors(context.Background())

	if got := classifyMCPEndUserRefRequirement(connectionContext, dynamic, auths, requirements, nil, unsatisfied); !errors.Is(got, errMCPEndUserRefRequired) {
		t.Fatalf("dynamic MCP error = %v, want %v", got, errMCPEndUserRefRequired)
	}
	// A connection selector proves the generic auth failure has another cause.
	if got := classifyMCPEndUserRefRequirement(connectionContext, dynamic, auths, requirements, map[string]any{"fused_end_user_ref": "user-1"}, unsatisfied); got != unsatisfied {
		t.Fatalf("selected-user error = %v, want original routing error", got)
	}
	// Fixed tokens resolve their connection from persisted bindings and never require the client header.
	fixed := auth.RuntimeIdentity{Kind: store.AppKindMCP, BindingMode: store.AppTokenBindingFixed}
	if got := classifyMCPEndUserRefRequirement(connectionContext, fixed, auths, requirements, nil, unsatisfied); got != unsatisfied {
		t.Fatalf("fixed-token error = %v, want original routing error", got)
	}
	// SDK identities retain their SDK-facing auth contract even if an internal caller propagates unrelated context.
	sdk := auth.RuntimeIdentity{Kind: store.AppKindSDK, BindingMode: store.AppTokenBindingDynamic}
	if got := classifyMCPEndUserRefRequirement(connectionContext, sdk, auths, requirements, nil, unsatisfied); got != unsatisfied {
		t.Fatalf("SDK error = %v, want original routing error", got)
	}
	// Unified selectors do not originate from the direct MCP connection handshake marker.
	if got := classifyMCPEndUserRefRequirement(context.Background(), dynamic, auths, requirements, nil, unsatisfied); got != unsatisfied {
		t.Fatalf("unmarked error = %v, want original routing error", got)
	}
	staticAuths := fusedobject.AuthConfigs{{Name: "apiKey", Type: "apiKey"}}
	staticRequirements := authrouting.Requirements{{Schemes: []authrouting.Requirement{{Scheme: "apiKey"}}}}
	// Static credential misses need bucket configuration guidance, not an end-user selector.
	if got := classifyMCPEndUserRefRequirement(connectionContext, dynamic, staticAuths, staticRequirements, nil, unsatisfied); got != unsatisfied {
		t.Fatalf("static-auth error = %v, want original routing error", got)
	}
}

// TestMCPDynamicConnectedAuthMissIsActionableAndAudited exercises the canonical physical boundary without allowing a provider request.
func TestMCPDynamicConnectedAuthMissIsActionableAndAudited(t *testing.T) {
	var providerCalls atomic.Int32
	vendor := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		providerCalls.Add(1)
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"unexpected":true}`))
	}))
	defer vendor.Close()

	identity, operation := mcpConnectedAuthPhysicalOperation(vendor.URL)
	withEntitlement(t, models.RuntimeEntitlement{MaxSandboxConcurrency: models.IntPtr(5)})
	activeExecutions.Delete(identity.AccountID)
	t.Cleanup(func() { activeExecutions.Delete(identity.AccountID) })

	previousResolver := globalSecretResolver
	globalSecretResolver = &mockSecretResolver{}
	t.Cleanup(func() { globalSecretResolver = previousResolver })

	auditCapture := &captureJetStreamPublisher{}
	executionevent.SetPublisher(executionevent.NewPublisher(auditCapture))
	t.Cleanup(func() { executionevent.SetPublisher(nil) })
	previousProvider := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previousProvider)
	})

	ctx := contextWithMCPConnectionSelectors(context.Background())
	_, err := ExecuteResolvedPhysicalJSON(ctx, engine.NewDispatcher(), identity, operation, PhysicalExecutionRequest{Transport: models.EngineExecutionTransportMCP})
	// The auth router rejects before HTTP construction, so both the error and zero provider calls are authoritative.
	if !errors.Is(err, errMCPEndUserRefRequired) || providerCalls.Load() != 0 {
		t.Fatalf("execution error = %v, provider calls = %d", err, providerCalls.Load())
	}
	assertMCPEndUserRefBridgeRecovery(t, err)
	assertMCPEndUserRefExecutionEvidence(t, auditCapture, recorder)
}

// TestMCPCallHandlerReturnsEndUserRefRequired proves the live bridge marker reaches the canonical execution and response boundaries.
func TestMCPCallHandlerReturnsEndUserRefRequired(t *testing.T) {
	var providerCalls atomic.Int32
	vendor := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		providerCalls.Add(1)
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"unexpected":true}`))
	}))
	defer vendor.Close()

	sessionID, operationID, _ := configureMCPPhysicalCallTest(t, vendor.URL)
	cache := globalObjectCache.(*richMockCache)
	auths, requirements := testConnectedAuthContract()
	cache.obj.AuthConfigs = auths
	cache.securityRequirements = requirements
	globalSecretResolver = &mockSecretResolver{}
	identity := auth.RuntimeIdentity{
		AccountID: uuid.New(), AppFamilyID: uuid.New(), TokenID: uuid.New(), AppVersion: "1.0.0",
		Kind: store.AppKindMCP, Status: store.AppStatusActive, BindingMode: store.AppTokenBindingDynamic,
		TokenPolicy: store.AppTokenPolicy{AllowAll: true},
	}
	globalTokenValidator = &mcpDynamicTokenValidator{identity: identity}
	withEntitlement(t, models.RuntimeEntitlement{MaxSandboxConcurrency: models.IntPtr(5)})
	activeExecutions.Delete(identity.AccountID)
	t.Cleanup(func() { activeExecutions.Delete(identity.AccountID) })

	body, err := json.Marshal(mcpCallRequest{OperationID: operationID, Params: json.RawMessage(`{}`)})
	// The bridge envelope must be valid before the handler can exercise auth routing.
	if err != nil {
		t.Fatalf("encode MCP call: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp/call", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer "+sessionID)
	response := httptest.NewRecorder()
	mcpCallHandler(response, request)

	var failure mcpCallResponse
	// A decodable response is required before stable recovery fields can be evaluated.
	if decodeErr := json.Unmarshal(response.Body.Bytes(), &failure); decodeErr != nil {
		t.Fatalf("decode MCP handler response: %v", decodeErr)
	}
	// The handler must reject before provider dispatch while preserving the caller-correctable status.
	if response.Code != http.StatusBadRequest || providerCalls.Load() != 0 {
		t.Fatalf("MCP handler response = status:%d provider calls:%d", response.Code, providerCalls.Load())
	}
	assertMCPEndUserRefHandlerRecovery(t, failure)
}

// assertMCPEndUserRefHandlerRecovery verifies the live bridge response retains every closed recovery field.
func assertMCPEndUserRefHandlerRecovery(t *testing.T, failure mcpCallResponse) {
	t.Helper()
	// Exact recovery metadata lets an MCP host correct connection context without guessing whether provider work started.
	if failure.Code != mcpEndUserRefRequiredCode || failure.Error != mcpEndUserRefRequiredMessage || failure.RecoveryAction != "correct_execute_arguments" || failure.ExecuteRequest != "correct_arguments" || failure.ProviderExecution != "not_started" || failure.AutomaticReplay == nil || *failure.AutomaticReplay {
		t.Fatalf("MCP handler recovery = %+v", failure)
	}
}

// testConnectedAuthContract returns one operation-local OAuth route shared by focused classifier tests.
func testConnectedAuthContract() (fusedobject.AuthConfigs, authrouting.Requirements) {
	auths := fusedobject.AuthConfigs{{Name: "jiraOAuth", Type: "oauth2"}}
	requirements := authrouting.Requirements{{Schemes: []authrouting.Requirement{{Scheme: "jiraOAuth"}}}}
	return auths, requirements
}

// mcpConnectedAuthPhysicalOperation builds a dynamic MCP operation whose empty resolver forces canonical auth routing to reject before dispatch.
func mcpConnectedAuthPhysicalOperation(providerURL string) (auth.RuntimeIdentity, ResolvedPhysicalOperation) {
	identity, operation := physicalExecutionTestOperation(providerURL)
	identity.Kind = store.AppKindMCP
	identity.BindingMode = store.AppTokenBindingDynamic
	auths, requirements := testConnectedAuthContract()
	operation.match.service.AuthConfigs = auths
	operation.match.endpoint.SecurityRequirements = requirements
	operation.match.selection.AuthType = "oauth"
	operation.match.selection.AuthName = "jiraOAuth"
	return identity, operation
}

// assertMCPEndUserRefBridgeRecovery verifies the private bridge exposes one closed pre-provider correction.
func assertMCPEndUserRefBridgeRecovery(t *testing.T, err error) {
	t.Helper()
	statusCode, response := boundedMCPPhysicalCallResponse("jira.searchProjects", err)
	// Guidance is connection-scoped and must not reveal operation or user identities.
	if statusCode != http.StatusBadRequest || response.Code != mcpEndUserRefRequiredCode || response.Error != mcpEndUserRefRequiredMessage || response.RecoveryAction != "correct_execute_arguments" || response.ExecuteRequest != "correct_arguments" || response.ProviderExecution != "not_started" || response.AutomaticReplay == nil || *response.AutomaticReplay {
		t.Fatalf("MCP end-user recovery = status:%d response:%+v", statusCode, response)
	}
	if strings.Contains(response.Error, "jira.searchProjects") {
		t.Fatalf("MCP end-user recovery exposed operation identity: %s", response.Error)
	}
}

// assertMCPEndUserRefExecutionEvidence verifies OTEL and Activity receive only the bounded missing-selector classification.
func assertMCPEndUserRefExecutionEvidence(t *testing.T, capture *captureJetStreamPublisher, recorder *tracetest.SpanRecorder) {
	t.Helper()
	event := decodeExecutionAuditEvent(t, capture.message)
	// No provider status is fabricated for a request rejected during Engine auth routing.
	if event.Transport != models.EngineExecutionTransportMCP || event.FailureCategory != "auth" || event.FailureCode != "end_user_ref_required" || event.FailureReason != "end_user_ref_required" || event.ProviderHTTPStatus != nil {
		t.Fatalf("MCP end-user audit = %#v", event)
	}
	span := executionSpanByName(t, recorder.Ended(), "engine.dispatch.execute")
	attributes := span.Attributes()
	// The execution span carries a closed code rather than the agent-facing message or user reference.
	if stringSpanAttribute(attributes, "execution.failure_category") != "auth" || stringSpanAttribute(attributes, "execution.failure_code") != "end_user_ref_required" || strings.Contains(strings.ToLower(span.Status().Description), "x-fused") {
		t.Fatalf("MCP end-user OTEL = status:%#v attributes:%#v", span.Status(), attributes)
	}
}
