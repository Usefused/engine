package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func artifactOAuth(scopes ...string) fusedobject.AuthConfig {
	declared := make(map[string]string, len(scopes))
	for _, scope := range scopes {
		declared[scope] = ""
	}
	return fusedobject.AuthConfig{Name: "oauthAuth", Type: "oauth2", OAuth2Flows: fusedobject.OAuth2Flows{"authorizationCode": {
		AuthorizationURL: "https://auth.example/authorize", TokenURL: "https://auth.example/token", Scopes: declared,
	}}}
}

func TestResolveSelectionAuthPolicyPinsProviderScheme(t *testing.T) {
	selection := models.SDKSelection{ServiceID: uuid.New(), AuthType: "oauth", ConnectScopes: []string{"read"}}
	contract := executionAuthContract(selection.ServiceID,
		fusedobject.AuthConfigs{{Name: "basicAuth", Type: "http", Scheme: "basic", BasicPasswordMode: authrouting.BasicPasswordRequired}, artifactOAuth("read", "write")},
		securedOperation("listItems", "oauthAuth"),
	)

	if err := resolveSelectionAuthPolicy(&selection, contract, &sdkAuthResolutionTelemetry{}); err != nil {
		t.Fatalf("resolveSelectionAuthPolicy() error = %v", err)
	}
	if selection.AuthType != "oauth" || selection.AuthName != "oauthAuth" {
		t.Fatalf("unexpected resolved auth policy: %#v", selection)
	}
	if len(selection.RequiredAuth) != 1 || selection.RequiredAuth[0].AuthName != "oauthAuth" {
		t.Fatalf("required auth was not persisted: %#v", selection.RequiredAuth)
	}
}

// TestResolveSelectionAuthPolicyDefaultsOmittedBasicPasswordMode proves planned requirements persist the effective standard shape.
func TestResolveSelectionAuthPolicyDefaultsOmittedBasicPasswordMode(t *testing.T) {
	selection := models.SDKSelection{ServiceID: uuid.New()}
	contract := executionAuthContract(selection.ServiceID,
		fusedobject.AuthConfigs{{Name: "basicAuth", Type: "http", Scheme: "basic"}},
		securedOperation("listItems", "basicAuth"),
	)
	if err := resolveSelectionAuthPolicy(&selection, contract, &sdkAuthResolutionTelemetry{}); err != nil {
		t.Fatalf("resolveSelectionAuthPolicy() error = %v", err)
	}
	// The applied selection must be self-describing even though the provider contract omitted the Fused extension.
	if len(selection.RequiredAuth) != 1 || selection.RequiredAuth[0].BasicPasswordMode != authrouting.BasicPasswordRequired {
		t.Fatalf("required auth did not normalize omitted Basic mode: %#v", selection.RequiredAuth)
	}
}

func TestResolveSelectionAuthPolicyRejectsBroaderScope(t *testing.T) {
	selection := models.SDKSelection{ServiceID: uuid.New(), AuthType: "oauth", ConnectScopes: []string{"admin"}}
	contract := executionAuthContract(selection.ServiceID,
		fusedobject.AuthConfigs{artifactOAuth("read")},
		securedOperation("listItems", "oauthAuth"),
	)
	err := resolveSelectionAuthPolicy(&selection, contract, &sdkAuthResolutionTelemetry{})
	if err == nil || !strings.Contains(err.Error(), "not provider-approved") {
		t.Fatalf("expected provider scope ceiling error, got %v", err)
	}
}

func TestResolveSelectionAuthPolicyLeavesAnonymousSelectionUnpinned(t *testing.T) {
	selection := models.SDKSelection{ServiceID: uuid.New(), AuthType: "oauth", AuthName: "oauthAuth", ConnectScopes: []string{"read"}, OperationNames: []string{"health"}}
	contract := executionAuthContract(selection.ServiceID,
		fusedobject.AuthConfigs{artifactOAuth("read")},
		anonymousOperation("health"),
	)
	telemetry := sdkAuthResolutionTelemetry{}
	if err := resolveSelectionAuthPolicy(&selection, contract, &telemetry); err != nil {
		t.Fatalf("resolveSelectionAuthPolicy() error = %v", err)
	}
	if selection.AuthType != "" || selection.AuthName != "" || len(selection.ConnectScopes) != 0 {
		t.Fatalf("anonymous selection retained auth policy: %#v", selection)
	}
	if telemetry.anonymousOnly != 1 || telemetry.none != 1 {
		t.Fatalf("unexpected telemetry: %#v", telemetry)
	}
}

func TestResolveSelectionAuthPolicyPinsOneSchemeAcrossMixedSelection(t *testing.T) {
	selection := models.SDKSelection{ServiceID: uuid.New(), OperationNames: []string{"health", "listItems"}}
	contract := executionAuthContract(selection.ServiceID,
		fusedobject.AuthConfigs{{Name: "basicAuth", Type: "http", Scheme: "basic", BasicPasswordMode: authrouting.BasicPasswordRequired}, {Name: "oauthAuth", Type: "oauth2"}},
		anonymousOperation("health"), securedOperation("listItems", "oauthAuth"),
	)
	telemetry := sdkAuthResolutionTelemetry{}
	if err := resolveSelectionAuthPolicy(&selection, contract, &telemetry); err != nil {
		t.Fatalf("resolveSelectionAuthPolicy() error = %v", err)
	}
	if selection.AuthType != "oauth" || selection.AuthName != "oauthAuth" || telemetry.mixed != 1 || telemetry.inferred != 1 {
		t.Fatalf("unexpected mixed auth resolution: selection=%#v telemetry=%#v", selection, telemetry)
	}
}

func TestResolveSelectionAuthPolicySupportsDifferentSchemesAcrossSelectedOperations(t *testing.T) {
	selection := models.SDKSelection{ServiceID: uuid.New(), OperationNames: []string{"one", "two"}}
	contract := executionAuthContract(selection.ServiceID,
		fusedobject.AuthConfigs{{Name: "basicAuth", Type: "http", Scheme: "basic", BasicPasswordMode: authrouting.BasicPasswordRequired}, {Name: "oauthAuth", Type: "oauth2"}},
		securedOperation("one", "basicAuth"), securedOperation("two", "oauthAuth"),
	)
	if err := resolveSelectionAuthPolicy(&selection, contract, &sdkAuthResolutionTelemetry{}); err != nil {
		t.Fatalf("resolveSelectionAuthPolicy() error = %v", err)
	}
	if selection.AuthType != "" || selection.AuthName != "" || len(selection.RequiredAuth) != 2 {
		t.Fatalf("mixed operation auth policy = %#v", selection)
	}
}

func TestResolveSelectionAuthPolicyPersistsOAuthAndMTLSAlternative(t *testing.T) {
	selection := models.SDKSelection{ServiceID: uuid.New(), ConnectScopes: []string{"read"}}
	contract := executionAuthContract(selection.ServiceID,
		fusedobject.AuthConfigs{
			artifactOAuth("read"),
			{Name: "clientCertificate", Type: "mutualTLS"},
		},
		securedOperationAlternatives("transfer", []string{"oauthAuth", "clientCertificate"}),
	)
	if err := resolveSelectionAuthPolicy(&selection, contract, &sdkAuthResolutionTelemetry{}); err != nil {
		t.Fatalf("resolveSelectionAuthPolicy() error = %v", err)
	}
	if selection.AuthName != "oauthAuth" || len(selection.RequiredAuth) != 2 {
		t.Fatalf("OAuth+mTLS policy = %#v", selection)
	}
}

func TestResolveSelectionAuthPolicyPersistsAPIKeyAndAPIToken(t *testing.T) {
	selection := models.SDKSelection{ServiceID: uuid.New()}
	contract := executionAuthContract(selection.ServiceID,
		fusedobject.AuthConfigs{{Name: "apiKey", Type: "apiKey"}, {Name: "apiToken", Type: "apiKey"}},
		securedOperationAlternatives("write", []string{"apiKey", "apiToken"}),
	)
	if err := resolveSelectionAuthPolicy(&selection, contract, &sdkAuthResolutionTelemetry{}); err != nil {
		t.Fatalf("resolveSelectionAuthPolicy() error = %v", err)
	}
	if len(selection.RequiredAuth) != 2 || selection.RequiredAuth[0].AuthName != "apiKey" || selection.RequiredAuth[1].AuthName != "apiToken" {
		t.Fatalf("API key AND token policy = %#v", selection)
	}
}

func TestResolveSelectionAuthPolicyUsesSourceOrderAndExplicitORChoice(t *testing.T) {
	serviceID := uuid.New()
	contract := executionAuthContract(serviceID,
		fusedobject.AuthConfigs{{Name: "apiKey", Type: "apiKey"}, {Name: "oauthAuth", Type: "oauth2"}},
		securedOperationAlternatives("read", []string{"apiKey"}, []string{"oauthAuth"}),
	)
	inferred := models.SDKSelection{ServiceID: serviceID}
	if err := resolveSelectionAuthPolicy(&inferred, contract, &sdkAuthResolutionTelemetry{}); err != nil {
		t.Fatal(err)
	}
	if len(inferred.RequiredAuth) != 1 || inferred.RequiredAuth[0].AuthName != "apiKey" {
		t.Fatalf("source-order branch = %#v", inferred)
	}
	explicit := models.SDKSelection{ServiceID: serviceID, AuthType: "oauth", AuthName: "oauthAuth"}
	if err := resolveSelectionAuthPolicy(&explicit, contract, &sdkAuthResolutionTelemetry{}); err != nil {
		t.Fatal(err)
	}
	if len(explicit.RequiredAuth) != 1 || explicit.RequiredAuth[0].AuthName != "oauthAuth" {
		t.Fatalf("explicit OR branch = %#v", explicit)
	}
}

// TestResolveSelectionAuthPolicyRejectsExplicitSelectorMissingFromOneOperation
// keeps rejection intact while naming the incompatible portion of the selection.
func TestResolveSelectionAuthPolicyRejectsExplicitSelectorMissingFromOneOperation(t *testing.T) {
	selection := models.SDKSelection{ServiceID: uuid.New(), AuthType: "oauth", AuthName: "oauthAuth"}
	contract := executionAuthContract(selection.ServiceID,
		fusedobject.AuthConfigs{{Name: "apiKey", Type: "apiKey"}, {Name: "oauthAuth", Type: "oauth2"}},
		securedOperationAlternatives("one", []string{"oauthAuth"}, []string{"apiKey"}),
		securedOperation("two", "apiKey"),
	)
	err := resolveSelectionAuthPolicy(&selection, contract, &sdkAuthResolutionTelemetry{})
	// A partial auth match must not be accepted or reported as a missing credential.
	if err == nil || !strings.Contains(err.Error(), "1 selected operation(s) do not support it") {
		t.Fatalf("expected explicit selector incompatibility, got %v", err)
	}
}

func TestResolveSelectionAuthPolicyLeavesWebhookOnlySelectionUnpinned(t *testing.T) {
	selection := models.SDKSelection{ServiceID: uuid.New(), WebhookNames: []string{"created"}, AuthType: "bearer", AuthName: "bearerAuth"}
	contract := executionAuthContract(selection.ServiceID, fusedobject.AuthConfigs{{Name: "bearerAuth", Type: "http", Scheme: "bearer"}})
	telemetry := sdkAuthResolutionTelemetry{}
	if err := resolveSelectionAuthPolicy(&selection, contract, &telemetry); err != nil {
		t.Fatalf("resolveSelectionAuthPolicy() error = %v", err)
	}
	if selection.AuthType != "" || selection.AuthName != "" || telemetry.webhookOnly != 1 {
		t.Fatalf("unexpected webhook-only auth resolution: selection=%#v telemetry=%#v", selection, telemetry)
	}
}

func TestResolveAppAuthPoliciesRejectsMissingOperation(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	registry := &sdkAuthContractRegistry{contracts: []sandbox.ServiceVersionExecutionAuthContract{{
		ServiceID: serviceID, Version: "v1", ServiceVersionID: versionID,
		Operations: []sandbox.OperationSecuritySummary{anonymousOperation("present")},
	}}}
	selections := []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: versionID, OperationNames: []string{"missing"}}}
	err := resolveAppAuthPolicies(context.Background(), registry, "key", []sdkResolvedService{{ServiceID: serviceID, ServiceVersionID: versionID, Version: "v1"}}, selections)
	if err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("expected missing operation error, got %v", err)
	}
}

// TestResolveAppAuthPoliciesClassifiesInvalidProviderContract keeps malformed auth metadata out of credential remediation.
func TestResolveAppAuthPoliciesClassifiesInvalidProviderContract(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	ctx, span := provider.Tracer("test").Start(context.Background(), "engine.sdk_config.plan")
	registry := &sdkAuthContractRegistry{contracts: []sandbox.ServiceVersionExecutionAuthContract{executionAuthContract(serviceID,
		fusedobject.AuthConfigs{{Name: "basicAuth", Type: "http", Scheme: "basic", BasicPasswordMode: "unknown"}},
		securedOperation("listItems", "basicAuth"),
	)}}
	selections := []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: versionID, OperationNames: []string{"listItems"}}}
	err := resolveAppAuthPolicies(ctx, registry, "key", []sdkResolvedService{{ServiceID: serviceID, ServiceVersionID: versionID, Version: "v1"}}, selections)
	span.End()
	var httpErr workspaceConfigHTTPError
	// Planning must preserve the typed error so HTTP and CLI callers receive the stable classification.
	if !errors.As(err, &httpErr) {
		t.Fatalf("invalid provider contract did not return a structured plan error: %v", err)
	}
	// The stable public response remains actionable without exposing internal scheme metadata.
	if httpErr.code != "invalid_service_auth_contract" || httpErr.category != "validation" || httpErr.remediation == "" || strings.Contains(httpErr.message, "basicAuth") {
		t.Fatalf("invalid provider contract response = %#v", httpErr)
	}
	attributes := endedSDKAuthAttributes(t, recorder)
	// OTEL records only the bounded failure class, never the provider scheme or service identity.
	if attributes["sdk.auth.decision_outcome"] != "invalid_contract" {
		t.Fatalf("invalid provider contract telemetry = %#v", attributes)
	}
}

// TestRecordSDKAuthResolutionUsesSafeAggregateAttributes keeps auth decisions
// observable without copying response-only service or scheme metadata into OTEL.
func TestRecordSDKAuthResolutionUsesSafeAggregateAttributes(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	ctx, span := provider.Tracer("test").Start(context.Background(), "engine.sdk_config.plan")
	recordSDKAuthResolution(ctx, sdkAuthResolutionTelemetry{anonymousOnly: 1, securedOnly: 2, explicit: 1, none: 1, required: 3, multiScheme: 1}, "success")
	span.End()

	attributes := endedSDKAuthAttributes(t, recorder)
	want := map[string]string{
		"sdk.auth.anonymous_only_count": "1", "sdk.auth.secured_only_count": "2",
		"sdk.auth.required_scheme_count": "3", "sdk.auth.multi_scheme_selection_count": "1",
		"sdk.auth.decision_source": "explicit", "sdk.auth.decision_outcome": "success",
	}
	// A table keeps each fixed aggregate assertion independent of optional display diagnostics.
	for key, value := range want {
		if attributes[key] != value {
			t.Fatalf("unexpected SDK auth telemetry: %#v", attributes)
		} // Missing and incorrect values must fail identically.
	}
	// Field names themselves must not introduce identity or credential-bearing telemetry dimensions.
	for key := range attributes {
		if strings.Contains(key, "name") || strings.Contains(key, "url") || strings.Contains(key, "secret") {
			t.Fatalf("unsafe auth attribute key %q", key)
		} // Inspect all emitted attributes, not only the expected aggregate subset.
	}
}

// endedSDKAuthAttributes projects one completed test span into a map so telemetry assertions stay focused and secret-safe.
func endedSDKAuthAttributes(t *testing.T, recorder *tracetest.SpanRecorder) map[string]string {
	t.Helper()
	spans := recorder.Ended()
	// Each focused auth decision test owns exactly one plan span.
	if len(spans) != 1 {
		t.Fatalf("expected one plan span, got %d", len(spans))
	}
	attributes := map[string]string{}
	for _, item := range spans[0].Attributes() {
		attributes[string(item.Key)] = item.Value.Emit()
	}
	return attributes
}

type sdkAuthContractRegistry struct {
	sandbox.RegistryClient
	contracts []sandbox.ServiceVersionExecutionAuthContract
}

func (r *sdkAuthContractRegistry) FetchServiceVersionExecutionAuthContracts(_ context.Context, selections []sandbox.ServiceVersionExecutionAuthSelection, _ string) ([]sandbox.ServiceVersionExecutionAuthContract, error) {
	contracts := append([]sandbox.ServiceVersionExecutionAuthContract(nil), r.contracts...)
	for index := range contracts {
		if index < len(selections) {
			contracts[index].OperationNames = selections[index].OperationNames
			contracts[index].SelectAll = selections[index].SelectAll
		}
	}
	return contracts, nil
}

func executionAuthContract(serviceID uuid.UUID, auths fusedobject.AuthConfigs, operations ...sandbox.OperationSecuritySummary) sandbox.ServiceVersionExecutionAuthContract {
	return sandbox.ServiceVersionExecutionAuthContract{ServiceID: serviceID, Version: "v1", AuthConfigs: auths, Operations: operations}
}

func anonymousOperation(name string) sandbox.OperationSecuritySummary {
	return sandbox.OperationSecuritySummary{Name: name, SecurityRequirements: authrouting.Requirements{{Schemes: []authrouting.Requirement{}}}}
}

func securedOperation(name, scheme string) sandbox.OperationSecuritySummary {
	return sandbox.OperationSecuritySummary{Name: name, SecurityRequirements: authrouting.Requirements{{Schemes: []authrouting.Requirement{{Scheme: scheme}}}}}
}

func securedOperationAlternatives(name string, alternatives ...[]string) sandbox.OperationSecuritySummary {
	requirements := make(authrouting.Requirements, 0, len(alternatives))
	for _, names := range alternatives {
		alternative := authrouting.Alternative{Schemes: make([]authrouting.Requirement, 0, len(names))}
		for _, authName := range names {
			alternative.Schemes = append(alternative.Schemes, authrouting.Requirement{Scheme: authName})
		}
		requirements = append(requirements, alternative)
	}
	return sandbox.OperationSecuritySummary{Name: name, SecurityRequirements: requirements}
}
