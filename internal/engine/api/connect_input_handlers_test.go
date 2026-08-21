package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// CreateConnectInputSession captures the hashed pre-authorisation record so
// handler tests can assert that no provider state exists before form submit.
func (s *connectAdminMockStore) CreateConnectInputSession(_ context.Context, session store.ConnectInputSession) (*store.ConnectInputSession, error) {
	session.ID = uuid.New()
	session.CreatedAt = time.Now().UTC()
	s.inputSessions = append(s.inputSessions, session)
	return &session, nil
}

// GetActiveConnectInputSessionByTokenHash performs the same exact active-row
// lookup shape as PostgreSQL, avoiding test-only Go filtering after return.
func (s *connectAdminMockStore) GetActiveConnectInputSessionByTokenHash(_ context.Context, tokenHash string) (*store.ConnectInputSession, error) {
	for index := range s.inputSessions {
		session := &s.inputSessions[index]
		if session.TokenHash == tokenHash && session.UsedAt == nil && time.Now().UTC().Before(session.ExpiresAt) {
			return session, nil
		}
	}
	return nil, nil
}

// CompleteConnectInputSession models the production one-time transaction by
// consuming exactly one pending row before exposing the provider session.
func (s *connectAdminMockStore) CompleteConnectInputSession(_ context.Context, tokenHash, contractHash string, usedAt time.Time, session store.ConnectSession) (*store.ConnectSession, error) {
	for index := range s.inputSessions {
		pending := &s.inputSessions[index]
		if pending.TokenHash != tokenHash || pending.ContractHash != contractHash || pending.UsedAt != nil || !usedAt.Before(pending.ExpiresAt) {
			continue
		}
		pending.UsedAt = &usedAt
		session.ID = uuid.New()
		session.CreatedAt = usedAt
		s.createdSessions = append(s.createdSessions, session)
		return &session, nil
	}
	return nil, store.ErrConnectSessionUnavailable
}

// TestConnectStartUsesHostedInputOnlyWhenRequiredDataIsMissing proves the API
// remains direct when complete and becomes interactive only for omissions.
func TestConnectStartUsesHostedInputOnlyWhenRequiredDataIsMissing(t *testing.T) {
	missing := newResourceInputRuntimeFixture(t)
	missingResponse := startResourceInputConnect(t, missing, `{}`)
	missingURL, err := url.Parse(missingResponse.AuthorizeURL)
	if err != nil {
		t.Fatalf("parse hosted input URL: %v", err)
	}
	if missingURL.Host != "engine.example.com" || missingURL.Path != "/workspace/connect/input" || missingURL.Query().Get("token") == "" {
		t.Fatalf("hosted input URL = %s", missingResponse.AuthorizeURL)
	}
	if len(missing.store.inputSessions) != 1 || len(missing.store.createdSessions) != 0 {
		t.Fatalf("missing input created pending=%d provider=%d", len(missing.store.inputSessions), len(missing.store.createdSessions))
	}

	complete := newResourceInputRuntimeFixture(t)
	completeResponse := startResourceInputConnect(t, complete, `{"subdomain":"acme"}`)
	completeURL, err := url.Parse(completeResponse.AuthorizeURL)
	if err != nil {
		t.Fatalf("parse provider URL: %v", err)
	}
	if completeURL.Host != "auth.example" || len(complete.store.inputSessions) != 0 || len(complete.store.createdSessions) != 1 {
		t.Fatalf("complete input URL=%s pending=%d provider=%d", completeResponse.AuthorizeURL, len(complete.store.inputSessions), len(complete.store.createdSessions))
	}
}

// TestConnectStartRejectsInvalidSuppliedInput keeps malformed automation
// fail-fast instead of hiding it behind a customer-facing collection page.
func TestConnectStartRejectsInvalidSuppliedInput(t *testing.T) {
	fixture := newResourceInputRuntimeFixture(t)
	body := strings.NewReader(`{"end_user_ref":"user_123","resource_input":{"subdomain":"not.valid"}}`)
	req := httptest.NewRequest(http.MethodPost, fixture.startPath(), body)
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	buildConnectRuntimeRouter(fixture).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || len(fixture.store.inputSessions) != 0 || len(fixture.store.createdSessions) != 0 {
		t.Fatalf("invalid input status=%d pending=%d provider=%d body=%s", rr.Code, len(fixture.store.inputSessions), len(fixture.store.createdSessions), rr.Body.String())
	}
}

// TestConnectStartRejectsInputWithoutADeclaredContract prevents arbitrary
// caller maps from being treated as harmless metadata on services that do not
// define customer routing fields.
func TestConnectStartRejectsInputWithoutADeclaredContract(t *testing.T) {
	fixture := newConnectRuntimeFixture(t)
	body := strings.NewReader(`{"end_user_ref":"user_123","resource_input":{"subdomain":"acme"}}`)
	req := httptest.NewRequest(http.MethodPost, fixture.startPath(), body)
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	buildConnectRuntimeRouter(fixture).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || len(fixture.store.inputSessions) != 0 || len(fixture.store.createdSessions) != 0 {
		t.Fatalf("unsupported input status=%d pending=%d provider=%d body=%s", rr.Code, len(fixture.store.inputSessions), len(fixture.store.createdSessions), rr.Body.String())
	}
}

// TestHostedConnectInputSubmissionCreatesProviderStateAfterValidation covers
// form rendering, customer submission, redirect, and replay denial as one flow.
func TestHostedConnectInputSubmissionCreatesProviderStateAfterValidation(t *testing.T) {
	fixture := newResourceInputRuntimeFixture(t)
	response := startResourceInputConnect(t, fixture, `{}`)
	hostedURL, _ := url.Parse(response.AuthorizeURL)
	token := hostedURL.Query().Get("token")
	router := buildConnectRuntimeRouter(fixture)

	view := httptest.NewRecorder()
	router.ServeHTTP(view, httptest.NewRequest(http.MethodGet, hostedURL.RequestURI(), nil))
	if view.Code != http.StatusOK || !strings.Contains(view.Body.String(), "Zendesk subdomain") || strings.Contains(view.Body.String(), "user_123") || strings.Contains(view.Body.String(), "pattern=") {
		t.Fatalf("form view status=%d body=%s", view.Code, view.Body.String())
	}
	viewCSP := view.Header().Get("Content-Security-Policy")
	if !strings.Contains(viewCSP, "form-action 'self' https://engine.example.com") || strings.Contains(viewCSP, "auth.example") {
		t.Fatalf("form CSP must admit only the Engine origin: %s", viewCSP)
	}
	if len(fixture.store.createdSessions) != 0 {
		t.Fatal("form view must not create provider OAuth state")
	}

	form := url.Values{"subdomain": {"acme"}, "ignored": {"attacker"}}
	submit := httptest.NewRequest(http.MethodPost, "/workspace/connect/input?token="+url.QueryEscape(token), strings.NewReader(form.Encode()))
	submit.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	completed := httptest.NewRecorder()
	router.ServeHTTP(completed, submit)
	if completed.Code != http.StatusOK || completed.Header().Get("Location") != "" || !strings.Contains(completed.Body.String(), "https://auth.example/authorize?") || completed.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("form completion status=%d location=%s body=%s", completed.Code, completed.Header().Get("Location"), completed.Body.String())
	}
	if !strings.Contains(completed.Body.String(), `http-equiv="refresh"`) || !strings.Contains(completed.Body.String(), "Continue to provider") {
		t.Fatalf("provider handoff page is incomplete: %s", completed.Body.String())
	}
	if strings.Contains(completed.Header().Get("Content-Security-Policy"), "auth.example") {
		t.Fatalf("provider handoff CSP retained a form redirect origin: %s", completed.Header().Get("Content-Security-Policy"))
	}
	if len(fixture.store.createdSessions) != 1 || fixture.store.inputSessions[0].UsedAt == nil {
		t.Fatalf("completion pending=%#v provider=%#v", fixture.store.inputSessions, fixture.store.createdSessions)
	}
	var stored map[string]string
	if err := json.Unmarshal(fixture.store.createdSessions[0].ResourceInputJSON, &stored); err != nil || stored["subdomain"] != "acme" || stored["ignored"] != "" {
		t.Fatalf("stored input=%#v err=%v", stored, err)
	}

	replayRequest := httptest.NewRequest(http.MethodPost, "/workspace/connect/input?token="+url.QueryEscape(token), strings.NewReader(form.Encode()))
	replayRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	replay := httptest.NewRecorder()
	router.ServeHTTP(replay, replayRequest)
	if replay.Code != http.StatusGone || len(fixture.store.createdSessions) != 1 {
		t.Fatalf("replay status=%d provider sessions=%d", replay.Code, len(fixture.store.createdSessions))
	}
}

// TestHostedConnectInputRendersClosedFieldTypes verifies that profile metadata
// becomes only the reviewed text and select controls with accessible guidance.
func TestHostedConnectInputRendersClosedFieldTypes(t *testing.T) {
	config := &fusedobject.ResourceInputConfig{Fields: []fusedobject.ResourceInputField{
		{
			Name: "subdomain", Label: "Jira <site>", Placeholder: "acme", Description: "Enter the provider site.",
			Required: true, Pattern: `^[a-z0-9-]+$`,
		},
		{
			Name: "region", Type: "select", Label: "Data region", Placeholder: "Choose a region",
			Description: "Controls regional routing.", Options: []fusedobject.ResourceInputOption{
				{Value: "eu", Label: "Europe & UK"}, {Value: "us", Label: "United States"},
			},
		},
	}}
	page := newConnectInputPage("capability", config, "https://engine.example/connect/callback", map[string]string{"region": "eu"}, "", hostedConnectBranding{ConnectBranding: store.DefaultConnectBranding()})
	response := httptest.NewRecorder()
	writeConnectInputPage(response, http.StatusOK, page)
	body := response.Body.String()

	// Text remains the legacy default and RE2 never crosses into browser grammar.
	for _, expected := range []string{
		`<input type="text" id="field-subdomain"`, `placeholder="acme"`, `required`,
		`<select id="field-region"`, `Choose a region`, `<option value="eu" selected>Europe &amp; UK</option>`,
		`Jira &lt;site&gt;`, `Enter the provider site.`, `Controls regional routing.`, `Required`, `Optional`,
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("hosted form missing %q: %s", expected, body)
		}
	}
	for _, forbidden := range []string{`pattern=`, `type="password"`, `type="number"`, `type="url"`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("hosted form exposed unsupported browser contract %q: %s", forbidden, body)
		}
	}
}

// TestConnectInputFormOriginRejectsNonBrowserSchemes keeps malformed or
// active-content callback endpoints outside the hosted page's CSP.
func TestConnectInputFormOriginRejectsNonBrowserSchemes(t *testing.T) {
	for _, raw := range []string{
		"", "://invalid", "javascript://auth.example/authorize",
		"http://auth.example/authorize", "https://user@auth.example/authorize",
		"https://auth.example;script/authorize", "https://auth.example/authorize\nInjected: value",
	} {
		if origin := connectInputFormOrigin(raw); origin != "" {
			t.Fatalf("provider origin for %q = %q", raw, origin)
		}
	}
	if origin := connectInputFormOrigin("https://engine.example:8443/callback?secret=excluded"); origin != "https://engine.example:8443" {
		t.Fatalf("provider origin = %q", origin)
	}
	if origin := connectInputFormOrigin("http://127.0.0.1:8080/callback"); origin != "http://127.0.0.1:8080" {
		t.Fatalf("loopback provider origin = %q", origin)
	}
}

// TestHostedConnectInputRejectsUnsafeProviderBeforeConsumption ensures an
// invalid authorization endpoint cannot burn the form token or mint OAuth state.
func TestHostedConnectInputRejectsUnsafeProviderBeforeConsumption(t *testing.T) {
	fixture := newResourceInputRuntimeFixture(t)
	flow := fixture.verifier.serviceMetadata.AuthConfigs[0].OAuth2Flows["authorizationCode"]
	flow.AuthorizationURL = "https://user@auth.example/authorize"
	fixture.verifier.serviceMetadata.AuthConfigs[0].OAuth2Flows["authorizationCode"] = flow
	response := startResourceInputConnect(t, fixture, `{}`)
	hostedURL, _ := url.Parse(response.AuthorizeURL)
	form := url.Values{"subdomain": {"acme"}}
	request := httptest.NewRequest(http.MethodPost, hostedURL.RequestURI(), strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	buildConnectRuntimeRouter(fixture).ServeHTTP(rr, request)
	if rr.Code != http.StatusGone || fixture.store.inputSessions[0].UsedAt != nil || len(fixture.store.createdSessions) != 0 {
		t.Fatalf("unsafe provider status=%d used=%v sessions=%d", rr.Code, fixture.store.inputSessions[0].UsedAt, len(fixture.store.createdSessions))
	}
}

// TestHostedConnectInputRejectsSameNameContractDrift ensures a pending form
// cannot inherit changed fields, hosts, scopes, or authorization metadata even
// when the selected auth type and name remain unchanged.
func TestHostedConnectInputRejectsSameNameContractDrift(t *testing.T) {
	fixture := newResourceInputRuntimeFixture(t)
	response := startResourceInputConnect(t, fixture, `{}`)
	hostedURL, _ := url.Parse(response.AuthorizeURL)
	fixture.verifier.serviceMetadata.ConnectConfig.ResourceInput.AllowedHosts = []string{"*.changed.example"}

	rr := httptest.NewRecorder()
	buildConnectRuntimeRouter(fixture).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, hostedURL.RequestURI(), nil))
	if rr.Code != http.StatusGone || len(fixture.store.createdSessions) != 0 {
		t.Fatalf("drifted form status=%d provider sessions=%d body=%s", rr.Code, len(fixture.store.createdSessions), rr.Body.String())
	}
}

// TestHostedConnectInputKeepsCapabilitySeparateFromTokenNamedResource verifies
// a legitimate provider field named token cannot read or persist the one-time
// browser capability used to authorize form completion.
func TestHostedConnectInputKeepsCapabilitySeparateFromTokenNamedResource(t *testing.T) {
	fixture := newConnectRuntimeFixture(t)
	fixture.verifier.serviceMetadata.ConnectConfig = &fusedobject.ServiceConnectConfig{ResourceInput: &fusedobject.ResourceInputConfig{
		Fields:          []fusedobject.ResourceInputField{{Name: "token", Label: "Tenant token", Required: true, Pattern: `^[a-z]+$`}},
		BaseURLTemplate: "https://{token}.example.com", ResourceType: "tenant", AllowedHosts: []string{"*.example.com"},
	}}
	response := startResourceInputConnect(t, fixture, `{}`)
	hostedURL, _ := url.Parse(response.AuthorizeURL)
	capability := hostedURL.Query().Get("token")
	form := url.Values{"token": {"acme"}}
	request := httptest.NewRequest(http.MethodPost, hostedURL.RequestURI(), strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	buildConnectRuntimeRouter(fixture).ServeHTTP(rr, request)
	if rr.Code != http.StatusOK || len(fixture.store.createdSessions) != 1 {
		t.Fatalf("token field completion status=%d sessions=%d body=%s", rr.Code, len(fixture.store.createdSessions), rr.Body.String())
	}
	stored := string(fixture.store.createdSessions[0].ResourceInputJSON)
	if !strings.Contains(stored, `"token":"acme"`) || strings.Contains(stored, capability) {
		t.Fatalf("stored token-named resource input leaked capability: %s", stored)
	}
}

// TestHostedConnectInputTelemetryUsesOnlyBoundedAttributes verifies customer
// values, raw tokens, hashes, URLs, and end-user references never enter OTEL.
func TestHostedConnectInputTelemetryUsesOnlyBoundedAttributes(t *testing.T) {
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
		slog.SetDefault(previousLogger)
	})

	fixture := newResourceInputRuntimeFixture(t)
	response := startResourceInputConnect(t, fixture, `{}`)
	hostedURL, _ := url.Parse(response.AuthorizeURL)
	request := httptest.NewRequest(http.MethodGet, hostedURL.RequestURI(), nil)
	buildConnectRuntimeRouter(fixture).ServeHTTP(httptest.NewRecorder(), request)

	spans := recorder.Ended()
	viewSpan := encodedConnectInputSpan(spans, "engine.connect.input.view")
	startSpan := encodedConnectInputSpan(spans, "engine.connect.session.start")
	if viewSpan == "" || startSpan == "" {
		t.Fatal("expected connect input view span")
	}
	encoded := viewSpan + startSpan + logs.String()
	for _, forbidden := range []string{"subdomain", "user_123", hostedURL.Query().Get("token"), connectHash(hostedURL.Query().Get("token")), "engine.example.com"} {
		if forbidden != "" && strings.Contains(encoded, forbidden) {
			t.Fatalf("telemetry leaked %q: %s", forbidden, encoded)
		}
	}
	for _, expected := range []string{
		"connect.input.action=view", "connect.input.outcome=rendered", "connect.input.field_count=1",
		"connect.input.route=hosted_form", "connect.input.missing_field_count=1", "outcome=success",
		`"msg":"connect input request completed"`, `"action":"view"`, `"field_count":1`,
	} {
		if !strings.Contains(encoded, expected) {
			t.Fatalf("telemetry missing %q: %s", expected, encoded)
		}
	}
}

// encodedConnectInputSpan selects one fixed span name and flattens its bounded
// attributes so telemetry allowlist and denylist assertions stay readable.
func encodedConnectInputSpan(spans []sdktrace.ReadOnlySpan, name string) string {
	for _, span := range spans {
		if span.Name() != name {
			continue
		}
		encoded := span.Name()
		for _, item := range span.Attributes() {
			encoded += string(item.Key) + "=" + item.Value.Emit()
		}
		return encoded
	}
	return ""
}

// newResourceInputRuntimeFixture attaches a declared tenant field to the
// ordinary OAuth fixture so tests exercise the real conditional start path.
func newResourceInputRuntimeFixture(t *testing.T) connectRuntimeFixture {
	fixture := newConnectRuntimeFixture(t)
	fixture.verifier.serviceMetadata.ConnectConfig = &fusedobject.ServiceConnectConfig{ResourceInput: &fusedobject.ResourceInputConfig{
		Fields:          []fusedobject.ResourceInputField{{Name: "subdomain", Label: "Zendesk subdomain", Required: true, Pattern: `^[a-z0-9-]+$`}},
		BaseURLTemplate: "https://{subdomain}.zendesk.com/api/v2", ResourceType: "zendesk_subdomain", AllowedHosts: []string{"*.zendesk.com"},
	}}
	return fixture
}

// startResourceInputConnect invokes the authenticated start endpoint and
// returns its next browser hop for direct-versus-hosted assertions.
func startResourceInputConnect(t *testing.T, fixture connectRuntimeFixture, resourceInputJSON string) connectSessionStartResponse {
	t.Helper()
	body := strings.NewReader(`{"end_user_ref":"user_123","resource_input":` + resourceInputJSON + `}`)
	req := httptest.NewRequest(http.MethodPost, fixture.startPath(), body)
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	buildConnectRuntimeRouter(fixture).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("start connect status=%d body=%s", rr.Code, rr.Body.String())
	}
	var response connectSessionStartResponse
	if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	return response
}
