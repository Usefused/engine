package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestAppAuthPlanErrorsUseServiceLabels covers shared SDK/MCP validation through
// its real contract resolver and HTTP envelope, without changing auth decisions.
func TestAppAuthPlanErrorsUseServiceLabels(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	oauth := artifactOAuth("read")
	otherOAuth := oauth
	otherOAuth.Name = "otherOAuth"
	cases := []struct {
		name      string
		selection models.SDKSelection
		contracts []sandbox.ServiceVersionExecutionAuthContract
		reason    string
		code      string
	}{
		{name: "incompatible", selection: models.SDKSelection{AuthType: "oauth", AuthName: "oauthAuth", SelectAll: true}, contracts: []sandbox.ServiceVersionExecutionAuthContract{executionAuthContract(serviceID, fusedobject.AuthConfigs{oauth, {Name: "apiKey", Type: "apiKey"}}, securedOperation("listIssues", "apiKey"))}, reason: appIncompatibleOAuthReason, code: "auth_selection_incompatible"},
		{name: "ambiguous", selection: models.SDKSelection{AuthType: "oauth", SelectAll: true}, contracts: []sandbox.ServiceVersionExecutionAuthContract{executionAuthContract(serviceID, fusedobject.AuthConfigs{oauth, otherOAuth}, securedOperationAlternatives("listIssues", []string{"oauthAuth"}, []string{"otherOAuth"}))}, reason: "auth selection is ambiguous; set auth.name"},
		{name: "scope", selection: models.SDKSelection{AuthType: "oauth", ConnectScopes: []string{"admin"}, SelectAll: true}, contracts: []sandbox.ServiceVersionExecutionAuthContract{executionAuthContract(serviceID, fusedobject.AuthConfigs{oauth}, securedOperation("listIssues", "oauthAuth"))}, reason: `connect scope "admin" is not provider-approved`},
		{name: "operation", selection: models.SDKSelection{OperationNames: []string{"missing"}}, contracts: []sandbox.ServiceVersionExecutionAuthContract{executionAuthContract(serviceID, nil, anonymousOperation("present"))}, reason: `selected operation "missing" was not found`},
		{name: "contract", selection: models.SDKSelection{SelectAll: true}, reason: "version auth contract was not found"},
	}
	// Each failure must be associated with this exact resolved service/config key.
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) { // Isolate response and telemetry evidence per failure kind.
			test.selection.ServiceID, test.selection.ServiceVersionID = serviceID, versionID
			service := sdkResolvedService{ServiceID: serviceID, ServiceVersionID: versionID, Version: "v1", ServiceName: "Jira", PublicTarget: "jira"}
			recorder := tracetest.NewSpanRecorder()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
			t.Cleanup(func() { _ = provider.Shutdown(context.Background()) }) // Close the isolated test tracer.
			ctx, span := provider.Tracer("test").Start(context.Background(), "engine.app.plan")
			err := resolveAppAuthPolicies(ctx, &sdkAuthContractRegistry{contracts: test.contracts}, "fixture-key", []sdkResolvedService{service}, []models.SDKSelection{test.selection})
			span.End()
			assertNamedAppAuthResponse(t, err, serviceID, test.reason, test.code)
			assertNamedAppAuthTelemetry(t, recorder)
		})
	}
}

// assertNamedAppAuthResponse checks actionable labels and stable failure codes
// while retaining the opaque service ID only as machine detail.
func assertNamedAppAuthResponse(t *testing.T, err error, serviceID uuid.UUID, reason, code string) {
	t.Helper()
	var httpErr workspaceConfigHTTPError
	// The typed public error must survive the shared resolver boundary.
	if !errors.As(err, &httpErr) {
		t.Fatalf("untyped plan failure: %v", err)
	}
	response := httptest.NewRecorder()
	writeWorkspaceConfigError(response, err, context.Background())
	var envelope workspaceConfigErrorResponse
	// Exercise actual JSON serialization rather than only the in-memory error.
	if decodeErr := json.Unmarshal(response.Body.Bytes(), &envelope); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	want := `service "Jira" (config key "jira") ` + reason
	// Existing validation cases retain their code while auth mismatches become distinguishable.
	if code == "" {
		code = "invalid_request"
	}
	// Both human and machine projections must survive the shared HTTP writer.
	if response.Code != http.StatusBadRequest || envelope.Error.Code != code || envelope.Error.Message != want {
		t.Fatalf("response = %d %#v", response.Code, envelope.Error)
	}
	// IDs remain available to tools without replacing the human-readable subject.
	if httpErr.details["service_id"] != serviceID.String() || strings.Contains(envelope.Error.Message, serviceID.String()) {
		t.Fatalf("service identity projection = %#v", envelope.Error)
	}
}

// assertNamedAppAuthTelemetry proves display labels are confined to the error
// response and do not enter the existing bounded auth-decision instrumentation.
func assertNamedAppAuthTelemetry(t *testing.T, recorder *tracetest.SpanRecorder) {
	t.Helper()
	attrs := endedSDKAuthAttributes(t, recorder)
	// Human error improvements must not alter the recorded decision category.
	if attrs["sdk.auth.decision_outcome"] != "invalid_selection" {
		t.Fatalf("auth telemetry = %#v", attrs)
	}
	encoded, err := json.Marshal(attrs)
	// A failed safety projection cannot silently skip the disclosure assertions.
	if err != nil {
		t.Fatal(err)
	}
	// Metadata stays response-only; even config keys and operation names are excluded.
	for _, forbidden := range []string{"Jira", "jira", "listIssues", "oauthAuth", "apiKey", "fixture-key", "provider-approved", "Incompatible operations"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("telemetry disclosed %q", forbidden)
		} // Inspect the full attribute map.
	}
}

// TestAppAuthPlanErrorLabelsFailingSelection verifies a multi-service plan names
// the rejected service, not the first successfully validated neighbour.
func TestAppAuthPlanErrorLabelsFailingSelection(t *testing.T) {
	gmailID, jiraID, versionID := uuid.New(), uuid.New(), uuid.New()
	registry := &sdkAuthContractRegistry{contracts: []sandbox.ServiceVersionExecutionAuthContract{
		executionAuthContract(gmailID, nil, anonymousOperation("health")),
		executionAuthContract(jiraID, fusedobject.AuthConfigs{artifactOAuth("read"), {Name: "apiKey", Type: "apiKey"}}, securedOperation("listIssues", "apiKey")),
	}}
	services := []sdkResolvedService{
		{ServiceID: gmailID, ServiceVersionID: versionID, Version: "v1", ServiceName: "Gmail", PublicTarget: "gmail"},
		{ServiceID: jiraID, ServiceVersionID: versionID, Version: "v1", ServiceName: "Jira", PublicTarget: "jira"},
	}
	selections := []models.SDKSelection{
		{ServiceID: gmailID, ServiceVersionID: versionID, SelectAll: true},
		{ServiceID: jiraID, ServiceVersionID: versionID, SelectAll: true, AuthType: "oauth", AuthName: "oauthAuth"},
	}
	err := resolveAppAuthPolicies(context.Background(), registry, "fixture-key", services, selections)
	assertNamedAppAuthResponse(t, err, jiraID, appIncompatibleOAuthReason, "auth_selection_incompatible")
}

// TestAppValidationServiceLabelFallbacks ensures names never override identity
// or cause an unavailable, unsafe, or mismatched label to be fabricated.
func TestAppValidationServiceLabelFallbacks(t *testing.T) {
	id := uuid.New()
	cases := []struct{ name, key, want string }{
		{name: "Jira", key: "jira", want: `service "Jira" (config key "jira")`},
		{key: "jira", want: `service "jira"`},
		{name: "Jira", want: `service "Jira"`},
		{name: "jira", key: "jira", want: `service "jira"`},
		{want: "service " + id.String()},
		{name: "fsk_secret", key: "jira", want: `service "jira"`},
		{name: "Jira", key: "\x1b[31mforged", want: `service "Jira"`},
	}
	// Display preferences are deterministic and use only already-resolved metadata.
	for _, test := range cases {
		got := appValidationServiceLabel(sdkResolvedService{ServiceID: id, ServiceName: test.name, PublicTarget: test.key}, id)
		if got != test.want {
			t.Fatalf("label = %q, want %q", got, test.want)
		} // Exact output prevents hidden UUID regressions.
	}
	wrong := appValidationServiceLabel(sdkResolvedService{ServiceID: uuid.New(), ServiceName: "Wrong service", PublicTarget: "wrong"}, id)
	// An ID mismatch must not misattribute the failing service to a neighbouring selection.
	if wrong != "service "+id.String() {
		t.Fatalf("mismatched label = %q", wrong)
	}
}

// TestAppValidationLabelsRejectUnsafeMetadata keeps service display names and
// config keys from becoming credential or terminal-control echo channels.
func TestAppValidationLabelsRejectUnsafeMetadata(t *testing.T) {
	unsafe := []string{"fsk_secret", "https://user:password@example.test", "Authorization: Bearer secret", "password=secret", "-----BEGIN PRIVATE KEY-----", "\x1b[31mJira", "Jira\nforged", "Jira\u202eforged", strings.Repeat("x", 129)}
	// Both name and config-key projections use this same admission gate.
	for _, value := range unsafe {
		if got := safeAppValidationLabel(value); got != "" {
			t.Fatalf("unsafe label returned: %q", got)
		} // Omit rather than rewrite unsafe text.
	}
}
