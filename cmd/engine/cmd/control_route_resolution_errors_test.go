package cmd

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type configResolutionCase struct {
	name        string
	permissions []accesscontrol.Permission
	storeErr    error
	status      int
	code        string
	outcome     accesscontrol.AuditOutcome
	spanOutcome string
}

// TestConfigPlanResolutionDiagnostics exercises the shared SDK/MCP/webhook
// boundary without executing a plan or disclosing unreadable workspace names.
func TestConfigPlanResolutionDiagnostics(t *testing.T) {
	all := []accesscontrol.Permission{accesscontrol.PermissionAppCreate, accesscontrol.PermissionServiceRead, accesscontrol.PermissionBucketRead}
	cases := []configResolutionCase{
		{name: "unresolved", permissions: all, status: 400, code: "config_service_unresolved", outcome: accesscontrol.AuditFailed, spanOutcome: "validation_failed"},
		{name: "no app grant", permissions: all[1:], status: 403, code: "permission_denied", outcome: accesscontrol.AuditDenied, spanOutcome: "denied"},
		{name: "no service grant", permissions: []accesscontrol.Permission{all[0], all[2]}, status: 403, code: "permission_denied", outcome: accesscontrol.AuditDenied, spanOutcome: "denied"},
		{name: "no bucket grant", permissions: all[:2], status: 403, code: "permission_denied", outcome: accesscontrol.AuditDenied, spanOutcome: "denied"},
		{name: "store outage", permissions: all, storeErr: errors.New("postgres://private-name:fsk_never_expose@database"), status: 503, code: "config_service_resolution_unavailable", outcome: accesscontrol.AuditFailed, spanOutcome: "resolution_unavailable"},
	}
	// All three config surfaces must expose the same reviewed resolution contract.
	for _, path := range []string{"/sdk-config/plan", "/mcp-config/plan", "/webhook-config/plan"} {
		// Grant failures must win over a typo in another selection.
		for _, test := range cases {
			// Each case owns its store, span, and audit sink to detect duplicate reporting.
			t.Run(path+"/"+test.name, func(t *testing.T) { assertConfigResolutionCase(t, path, test) })
		}
	}
}

// assertConfigResolutionCase checks the real middleware response and canonical
// audit/trace records, including the guarantee that downstream execution stops.
func assertConfigResolutionCase(t *testing.T, path string, test configResolutionCase) {
	t.Helper()
	workspaceID, serviceID, bucketID := uuid.New(), uuid.New(), uuid.New()
	stores := &controlRequirementStoreStub{
		localServiceIDs: map[string]uuid.UUID{"googledrive": serviceID}, serviceResolveErr: test.storeErr,
		buckets:      []store.Bucket{{ID: bucketID, Name: "production"}},
		displayNames: map[accesscontrol.ResourceRef]string{{Type: accesscontrol.ResourceService, ID: serviceID}: "private-name"},
	}
	var grants []accesscontrol.Grant
	// Workspace-scoped fixture grants exercise the same snapshot authorizer as production.
	for _, permission := range test.permissions {
		grants = append(grants, accesscontrol.Grant{Permission: permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}})
	}
	actor := actorWithGrants(t, workspaceID, grants...)
	recorder := &controlAuditRecorderStub{}
	spans := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	t.Cleanup(func() { _ = provider.Shutdown(t.Context()) }) // Release the test-only trace provider.
	body := `{"config_key":"mcp:fixture:1.0.0","config":{"bucket":"production","services":{"googledrive":{},"google-drive":{}}}}`
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	ctx, span := provider.Tracer("test").Start(accesscontrol.ContextWithActor(request.Context(), actor), "control")
	request = request.WithContext(ctx)
	handler := controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, newControlRequirementResolver(stores, &controlConfigRepositoryStub{}), recorder)(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("resolution rejection reached plan handler") }), // Any call would permit a partial plan.
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	span.End()
	// Status and stable code must distinguish input, grants, and infrastructure.
	if response.Code != test.status || !strings.Contains(response.Body.String(), test.code) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	assertConfigResolutionBody(t, response.Body.Bytes(), test.status)
	assertConfigResolutionAudit(t, recorder.events, test)
	assertConfigResolutionTrace(t, spans.Ended(), test.spanOutcome)
	// All service references must resolve in one query, including the failing path.
	if stores.slugLoads != 1 {
		t.Fatalf("service lookup count = %d", stores.slugLoads)
	}
}

// assertConfigResolutionBody keeps caller references useful without returning
// candidate names, database errors, or unsafe values on denial/dependency paths.
func assertConfigResolutionBody(t *testing.T, body []byte, status int) {
	t.Helper()
	text := string(body)
	// Only a successful lookup with unresolved keys may echo the submitted reference.
	if status == http.StatusBadRequest {
		var envelope struct {
			Error controlResolutionError `json:"error"`
		}
		// This is the same structured envelope understood by the CLI parser.
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Fatal(err)
		}
		// The remediation uses discovery, never a guessed slug or a role change.
		if !strings.Contains(envelope.Error.Details.ServerDetail, `"google-drive"`) || !strings.Contains(envelope.Error.Remediation, "fused-cli workspace services list") || envelope.Error.Retryable {
			t.Fatalf("validation diagnostic = %#v", envelope.Error)
		}
	} else if strings.Contains(text, "google-drive") { // Denials and outages must not reuse validation detail.
		t.Fatalf("unexpected reference disclosure: %s", text)
	}
	// These values are neither safe response context nor authorized candidate suggestions.
	for _, forbidden := range []string{"private-name", "fsk_", "postgres://"} {
		// A hidden display name must stay hidden even when another key is invalid.
		if strings.Contains(text, forbidden) {
			t.Fatalf("response disclosed %q: %s", forbidden, text)
		}
	}
}

// assertConfigResolutionAudit ensures validation is recorded as a failed
// operation, not a denied grant, with bounded metadata and no input identifiers.
func assertConfigResolutionAudit(t *testing.T, events []accesscontrol.AuditEvent, test configResolutionCase) {
	t.Helper()
	// Exactly one final record is expected because no mutation attempt was admitted.
	if len(events) != 1 {
		t.Fatalf("audit count = %d", len(events))
	}
	event := events[0]
	// The stable resolver code must survive without changing real permission denials.
	if event.Outcome != test.outcome || event.ReasonCode != test.code || event.StatusCode != test.status {
		t.Fatalf("audit = %#v", event)
	}
	encoded, err := json.Marshal(event)
	// Marshal failures cannot be ignored in the telemetry disclosure assertion.
	if err != nil {
		t.Fatal(err)
	}
	// Caller keys and dependency strings belong in neither audit fields nor metadata.
	for _, forbidden := range []string{"google-drive", "googledrive", "production", "private-name", "fsk_", "postgres://"} {
		// Search the whole record, not just metadata, to cover accidental field reuse.
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("audit disclosed %q", forbidden)
		}
	}
}

// assertConfigResolutionTrace requires one route event with the exact existing
// allowlist, proving diagnostics do not add request text to telemetry.
func assertConfigResolutionTrace(t *testing.T, spans []sdktrace.ReadOnlySpan, outcome string) {
	t.Helper()
	// The fixture creates exactly one logical control span.
	if len(spans) != 1 {
		t.Fatalf("span count = %d", len(spans))
	}
	event := configResolutionRouteEvent(t, spans[0].Events())
	attrs := map[string]any{}
	// Capture every attribute so additions fail the exact allowlist below.
	for _, attr := range event.Attributes {
		attrs[string(attr.Key)] = attr.Value.AsInterface()
	}
	// Only fixed route/method/outcome values and a bounded requirement count are allowed.
	if len(attrs) != 4 || attrs["engine.authorization.outcome"] != outcome || attrs["engine.authorization.method"] != "POST" || attrs["engine.authorization.requirements"] == nil || attrs["engine.authorization.policy"] == nil {
		t.Fatalf("route attributes = %#v", attrs)
	}
}

// configResolutionRouteEvent permits existing grant-check events while requiring
// exactly one route result and excluding input text from the entire trace.
func configResolutionRouteEvent(t *testing.T, events []sdktrace.Event) sdktrace.Event {
	t.Helper()
	var route []sdktrace.Event
	// Individual grant checks are canonical events, not duplicate route decisions.
	for _, event := range events {
		// Any newly introduced telemetry path needs explicit review here.
		switch event.Name {
		case "engine.authorization.route":
			route = append(route, event)
		case "engine.authorization.check":
		default:
			t.Fatalf("unexpected event %q", event.Name)
		}
	}
	encoded, err := json.Marshal(events)
	// A failed safety projection must not silently skip disclosure checks.
	if err != nil {
		t.Fatal(err)
	}
	// Check every event, including grant checks emitted during denial enrichment.
	for _, forbidden := range []string{"google-drive", "googledrive", "production", "private-name", "fsk_", "postgres://"} {
		// Raw input or dependency material must never reach span attributes.
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("trace disclosed %q", forbidden)
		}
	}
	// Multiple route events would double-report the final authorization outcome.
	if len(route) != 1 {
		t.Fatalf("route event count = %d", len(route))
	}
	return route[0]
}

// TestDesiredServiceRequirementsRejectsMissingKeysDespiteAliasCounts guards the
// original row-count bug and preserves one grant per resolved service identity.
func TestDesiredServiceRequirementsRejectsMissingKeysDespiteAliasCounts(t *testing.T) {
	id := uuid.New()
	requirements, missing := desiredServiceRequirements([]string{"@provider/drive", "drive", "google-drive"}, map[string]uuid.UUID{"@provider/drive": id, "drive": id}, accesscontrol.PermissionServiceRead)
	// Alias cardinality must never substitute for per-key resolution.
	if len(requirements) != 1 || requirements[0].Resource.ID != id || len(missing) != 1 || missing[0] != "google-drive" {
		t.Fatalf("requirements/missing = %#v/%v", requirements, missing)
	}
}

// TestConfigPlanCanonicalLookupRetainsLegacyCandidateGrants prevents a slug
// match from bypassing read access to a colliding legacy display-name selection.
func TestConfigPlanCanonicalLookupRetainsLegacyCandidateGrants(t *testing.T) {
	workspaceID, canonicalID, legacyID := uuid.New(), uuid.New(), uuid.New()
	stores := &controlRequirementStoreStub{
		localServiceIDs: map[string]uuid.UUID{"drive": canonicalID},
		services:        []store.WorkspaceService{{ServiceID: canonicalID, ServiceSlug: "drive"}, {ServiceID: legacyID, ServiceName: "drive"}},
	}
	actor := actorWithGrants(t, workspaceID,
		accesscontrol.Grant{Permission: accesscontrol.PermissionAppCreate, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: canonicalID}},
	)
	request := httptest.NewRequest(http.MethodPost, "/mcp-config/plan", strings.NewReader(`{"config_key":"mcp:fixture:1.0.0","config":{"services":{"drive":{}}}}`))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	// Dispatch would mean a display-name target escaped the authorization boundary.
	handler := controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, newControlRequirementResolver(stores, &controlConfigRepositoryStub{}), &controlAuditRecorderStub{})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("unauthorized legacy candidate reached plan handler")
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	// The colliding service retains its own grant even when canonical lookup succeeds.
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), legacyID.String()) {
		t.Fatalf("candidate authorization = %d %s", response.Code, response.Body.String())
	}
}

// TestUnresolvedConfigServicesBoundsAndRedactsReferences prevents error output
// from becoming a credential, terminal-control, or unbounded input echo.
func TestUnresolvedConfigServicesBoundsAndRedactsReferences(t *testing.T) {
	unsafe := []string{"fsk_secret", "https://example.test/private", "\x1b[31mred", "password=value", strings.Repeat("a", 129), "-----BEGIN PRIVATE KEY-----"}
	// Each prohibited key must leave a useful generic diagnostic without its value.
	for _, key := range unsafe {
		diagnostic := unresolvedConfigServices([]string{key})
		// Omission is safer than a lossy rendering of credential-shaped input.
		if diagnostic.Details.ServerDetail != "" {
			t.Fatalf("unsafe reference echoed: %q", key)
		}
	}
	detail := unresolvedConfigServices([]string{"one", "two", "three", "four", "five", "six"}).Details.ServerDetail
	// A deterministic sample avoids unbounded diagnostics while acknowledging truncation.
	if strings.Contains(detail, "six") || !strings.Contains(detail, "five") || len(detail) > 1024 {
		t.Fatalf("unbounded detail: %q", detail)
	}
}
