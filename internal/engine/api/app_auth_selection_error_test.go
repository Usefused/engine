package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

const appIncompatibleOAuthReason = `selects auth.type="oauth", auth.name="oauthAuth", but 1 selected operation(s) do not support it. Operations within the same service can require different authentication methods.`

// TestAppAuthMismatchExplainsOperationRequirements preserves policy semantics
// while making the failing operation and its OR-of-AND requirements visible.
func TestAppAuthMismatchExplainsOperationRequirements(t *testing.T) {
	auths := fusedobject.AuthConfigs{artifactOAuth("read"), {Name: "basicAuth", Type: "http", Scheme: "basic"}, {Name: "apiKey", Type: "apiKey"}, {Name: "clientCertificate", Type: "mutualTLS"}}
	operations := []sandbox.OperationSecuritySummary{
		anonymousOperation("health"), securedOperation("readIssue", "oauthAuth"),
		securedOperationAlternatives("either", []string{"basicAuth"}, []string{"oauthAuth"}),
		securedOperationAlternatives("both", []string{"oauthAuth", "clientCertificate"}),
		securedOperationAlternatives("getUserEmail", []string{"basicAuth", "apiKey"}, []string{"clientCertificate"}),
	}
	selection := models.SDKSelection{ServiceID: uuid.New(), AuthType: "oauth", AuthName: "oauthAuth", SelectAll: true}
	err := incompatibleAppAuthError(selection, auths, operations)
	// The primary explanation remains useful even in clients that omit structured detail.
	if err.reason != appIncompatibleOAuthReason || err.code != "auth_selection_incompatible" {
		t.Fatalf("mismatch = %#v", err)
	}
	want := `"getUserEmail" requires ("basicAuth" (basic) AND "apiKey" (api_key)) OR ("clientCertificate" (mtls))`
	// An AND branch must not become a choice, and accepted operations must not be blamed.
	if !strings.Contains(err.detail, want) || strings.Contains(err.detail, "readIssue") || strings.Contains(err.detail, "either") || strings.Contains(err.detail, "both") || strings.Contains(err.detail, "health") {
		t.Fatalf("requirements = %s", err.detail)
	}
	// Auth is a config choice, not necessarily a missing credential or a provider outage.
	if !strings.Contains(err.remedy, "explicit operations list") || !strings.Contains(err.remedy, "declared per-operation requirements") {
		t.Fatalf("remediation = %s", err.remedy)
	}
}

// TestAppAuthMismatchDoesNotInventAScheme distinguishes typos and type-only
// disjoint coverage instead of arbitrarily attributing failures to one scheme.
func TestAppAuthMismatchDoesNotInventAScheme(t *testing.T) {
	auths := fusedobject.AuthConfigs{{Name: "OAuth2", Type: "oauth2"}, {Name: "otherOAuth", Type: "oauth2"}}
	operations := []sandbox.OperationSecuritySummary{securedOperation("one", "OAuth2"), securedOperation("two", "otherOAuth")}
	cases := []struct{ name, authType, authName, code, message, detail string }{
		{"case sensitive", "oauth", "oauth2", "auth_selection_not_found", "case-sensitive", `"OAuth2"`},
		{"wrong type", "basic", "OAuth2", "auth_selection_not_found", "no declared authentication", `"OAuth2"`},
		{"disjoint schemes", "oauth", "", "auth_selection_incompatible", "every secured selected operation", "No single matching scheme covers the selection"},
		{"scope only", "", "", "auth_selection_incompatible", "connect.scopes", "No single matching scheme covers the selection"},
	}
	// Exercise the real policy boundary so diagnostic branches cannot drift from selection rules.
	for _, test := range cases {
		selection := models.SDKSelection{AuthType: test.authType, AuthName: test.authName, ConnectScopes: []string{"read"}}
		policyErr := resolveSelectionAuthPolicy(&selection, executionAuthContract(uuid.New(), auths, operations...), &sdkAuthResolutionTelemetry{})
		var err appServiceValidationError
		// Scope-only and explicit preferences must reach the same typed rejection path.
		if !errors.As(policyErr, &err) {
			t.Fatalf("%s: untyped policy error %v", test.name, policyErr)
		}
		// Unknown selectors and disjoint coverage need different remedies, not an invented match.
		if err.code != test.code || !strings.Contains(err.reason, test.message) || !strings.Contains(err.detail, test.detail) {
			t.Fatalf("%s: %#v", test.name, err)
		}
	}
}

// TestAppAuthMismatchBoundsDiagnostic keeps large select_all failures usable
// without hiding the total or silently flattening complex requirements.
func TestAppAuthMismatchBoundsDiagnostic(t *testing.T) {
	auths := fusedobject.AuthConfigs{artifactOAuth("read"), {Name: "basicAuth", Type: "http", Scheme: "basic"}}
	operations := []sandbox.OperationSecuritySummary{}
	for index := range 20 {
		operations = append(operations, securedOperation(fmt.Sprintf("operation%02d", index), "basicAuth"))
	}
	err := incompatibleAppAuthError(models.SDKSelection{AuthType: "oauth", SelectAll: true}, auths, operations)
	// Count all failures while capping the response sample independently of service size.
	if !strings.Contains(err.reason, "20 selected operation(s)") || strings.Count(err.detail, " requires ") != 5 || strings.Contains(err.detail, "operation05") {
		t.Fatalf("unbounded or inaccurate diagnostic: %#v", err)
	}
	long := strings.Repeat("界", 128)
	complex := securedOperationAlternatives("complex", []string{long, long, long, "hidden"}, []string{long}, []string{long}, []string{"hidden"})
	label := appAuthRequirementLabel(complex.SecurityRequirements, nil)
	// Mandatory requirements and alternate choices need distinct truncation markers.
	if !strings.Contains(label, "additional required schemes omitted") || !strings.Contains(label, "additional alternatives omitted") {
		t.Fatalf("missing truncation markers: %s", label)
	}
	detail := boundedAppAuthDetail(strings.Repeat("界", 1100))
	// Preserve Unicode and leave room under the shared CLI detail ceiling.
	if len([]rune(detail)) > 1024 || !strings.HasSuffix(detail, "[additional detail omitted]") {
		t.Fatalf("detail budget = %d", len([]rune(detail)))
	}
}

// TestAppAuthMismatchOmitsUnsafeMetadata applies the shared display boundary
// to selector, operation, and provider-scheme names without echoing secrets.
func TestAppAuthMismatchOmitsUnsafeMetadata(t *testing.T) {
	unsafe := "fsk_do_not_echo"
	selection := models.SDKSelection{AuthType: "oauth", AuthName: unsafe}
	auths := fusedobject.AuthConfigs{{Name: unsafe, Type: "oauth2"}, {Name: "password=hidden", Type: "apiKey"}}
	err := incompatibleAppAuthError(selection, auths, []sandbox.OperationSecuritySummary{securedOperation("\x1b[31mforged", "password=hidden")})
	encoded := fmt.Sprintf("%s %s %s", err.reason, err.detail, err.remedy)
	// No unsafe metadata belongs in either the primary text or server-detail field.
	for _, forbidden := range []string{unsafe, "password=hidden", "forged", "\x1b"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("diagnostic disclosed %q", forbidden)
		} // Check all response fields together, not just the service label.
	}
}

type appAuthMismatchRegistry struct {
	*mockRegistryClient
	contracts []sandbox.ServiceVersionExecutionAuthContract
	calls     int
}

// FetchServiceVersionExecutionAuthContracts overrides only the auth batch so
// HTTP tests still exercise the normal workspace and version-resolution paths.
func (registry *appAuthMismatchRegistry) FetchServiceVersionExecutionAuthContracts(ctx context.Context, requests []sandbox.ServiceVersionExecutionAuthSelection, key string) ([]sandbox.ServiceVersionExecutionAuthContract, error) {
	registry.calls++
	return (&sdkAuthContractRegistry{contracts: registry.contracts}).FetchServiceVersionExecutionAuthContracts(ctx, requests, key)
}

// TestAppAuthMismatchReachesSDKAndMCPHTTP exercises both real planning handlers
// and proves a rejected selection never creates a plan or extra metadata reads.
func TestAppAuthMismatchReachesSDKAndMCPHTTP(t *testing.T) {
	// Kind-specific identity fragments keep each request valid until it reaches the shared auth decision under test.
	for _, test := range []struct{ kind, language, descriptionField string }{
		{"sdk", "typescript", ""},
		{"mcp", "", `,"description":"Find and manage Jira work."`},
	} {
		t.Run(test.kind, func(t *testing.T) { // Both entry points must preserve the same shared diagnostic.
			serviceID, versionID := uuid.New(), uuid.New()
			s := &workspaceTestStore{accountID: uuid.New(),
				workspaceServices:        []store.WorkspaceService{{ServiceID: serviceID, ServiceName: "Jira", Version: "v1"}},
				workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{serviceID: {{ServiceID: serviceID, ServiceVersionID: versionID, Version: "v1"}}},
			}
			registry := &appAuthMismatchRegistry{mockRegistryClient: &mockRegistryClient{slugIDs: map[string]uuid.UUID{"jira": serviceID}},
				contracts: []sandbox.ServiceVersionExecutionAuthContract{executionAuthContract(serviceID,
					fusedobject.AuthConfigs{artifactOAuth("read"), {Name: "basicAuth", Type: "http", Scheme: "basic"}},
					securedOperation("readIssue", "oauthAuth"), securedOperation("getUserEmail", "basicAuth"))},
			}
			configStore := &mockConfigStore{}
			router := newControlTestRouter(s.accountID)
			router.Post("/sdk-config/plan", SDKConfigPlanHandler(configStore, s, registry))
			router.Post("/mcp-config/plan", MCPConfigPlanHandler(configStore, s, registry))
			body := fmt.Sprintf(`{"source_hash":"fixture","config_key":"%s:fixture:1.0.0","config":{"apiVersion":"fused/v1","kind":%q,"name":"fixture","version":"1.0.0"%s,"language":%q,"bucket":"default","services":{"jira":{"version":"v1","select_all":true,"auth":{"type":"oauth","name":"oauthAuth"}}}}}`, test.kind, test.kind, test.descriptionField, test.language)
			request := httptest.NewRequest(http.MethodPost, "/"+test.kind+"-config/plan", strings.NewReader(body))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			var envelope workspaceConfigErrorResponse
			// An HTTP regression must surface before content assertions or plan-state checks.
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || response.Code != http.StatusBadRequest {
				t.Fatalf("response = %d %s: %v", response.Code, response.Body.String(), err)
			}
			// Human service identity, selected auth, and concrete requirements must reach both APIs.
			if envelope.Error.Code != "auth_selection_incompatible" || !strings.Contains(envelope.Error.Message, `service "Jira" (config key "jira") `+appIncompatibleOAuthReason) || !strings.Contains(fmt.Sprint(envelope.Error.Details["server_detail"]), `"getUserEmail" requires ("basicAuth" (basic))`) {
				t.Fatalf("diagnostic = %#v", envelope.Error)
			}
			// Diagnostics are derived from the one existing auth batch, before persistence.
			if registry.calls != 1 || registry.fetchMetadataCalls != 0 || configStore.createdPlan != nil {
				t.Fatalf("unexpected side effects: auth calls=%d metadata calls=%d plan=%#v", registry.calls, registry.fetchMetadataCalls, configStore.createdPlan)
			}
		})
	}
}
