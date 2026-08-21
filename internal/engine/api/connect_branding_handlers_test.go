package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type connectBrandingTestStore struct {
	store.Store
	branding    store.ConnectBranding
	loadErr     error
	updateErr   error
	updateCalls int
}

// GetConnectBranding returns one configured singleton projection for handler tests.
func (s *connectBrandingTestStore) GetConnectBranding(context.Context) (store.ConnectBranding, error) {
	return s.branding, s.loadErr
}

// UpsertConnectBranding records one replacement without introducing test-only filtering.
func (s *connectBrandingTestStore) UpsertConnectBranding(_ context.Context, branding store.ConnectBranding) (store.ConnectBranding, error) {
	s.updateCalls++
	if s.updateErr != nil {
		// Injected persistence failures stop before changing the test projection.
		return store.ConnectBranding{}, s.updateErr
	}
	s.branding = branding
	return branding, nil
}

// connectBrandingActorContext supplies the authenticated control identity that
// production middleware places on every authorized settings request.
func connectBrandingActorContext() context.Context {
	return accesscontrol.ContextWithActor(context.Background(), accesscontrol.Actor{
		AccountID: uuid.New(), WorkspaceID: uuid.New(), SubjectID: uuid.New(), Kind: accesscontrol.SubjectUser,
	})
}

// TestConnectBrandingHandlersReadDefaultsAndNormalizeReplacement verifies the
// REST contract returns and persists only the public branding projection.
func TestConnectBrandingHandlersReadDefaultsAndNormalizeReplacement(t *testing.T) {
	testStore := &connectBrandingTestStore{branding: store.DefaultConnectBranding()}
	get := httptest.NewRecorder()
	GetConnectBrandingHandler(testStore).ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/workspace/connect-branding", nil))
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"display_name":"Fused"`) {
		t.Fatalf("GET branding = %d %s", get.Code, get.Body.String())
	}

	ctx := accesscontrol.ContextWithMutationAuditEvidence(connectBrandingActorContext())
	body := `{"display_name":"  Acme Connect  ","logo_url":"https://assets.example.com/brand/logo.png?v=secret","primary_color":"#AABBCC","support_url":"https://help.example.com/connect","privacy_url":""}`
	request := httptest.NewRequest(http.MethodPut, "/workspace/connect-branding", strings.NewReader(body)).WithContext(ctx)
	put := httptest.NewRecorder()
	UpsertConnectBrandingHandler(testStore).ServeHTTP(put, request)
	if put.Code != http.StatusOK || testStore.updateCalls != 1 {
		t.Fatalf("PUT branding = %d calls=%d body=%s", put.Code, testStore.updateCalls, put.Body.String())
	}
	var response store.ConnectBranding
	if err := json.NewDecoder(put.Body).Decode(&response); err != nil {
		t.Fatalf("decode branding response: %v", err)
	}
	if response.DisplayName != "Acme Connect" || response.PrimaryColor != "#AABBCC" {
		t.Fatalf("normalized branding = %#v", response)
	}
	evidence, ok := accesscontrol.MutationAuditEvidenceFromContext(ctx)
	if !ok || !evidence.ConnectBrandingChanges.Present || evidence.ConnectBrandingChanges.Count != 4 || !evidence.ConnectBrandingChanges.LogoURL || evidence.ConnectBrandingChanges.PrivacyURL {
		t.Fatalf("branding audit evidence = %#v, present=%v", evidence, ok)
	}
}

// TestConnectBrandingPUTRejectsUnsafeValues covers every browser-bound field
// before any database mutation can make it durable.
func TestConnectBrandingPUTRejectsUnsafeValues(t *testing.T) {
	unsafeURLs := []string{
		"http://assets.example.com/logo.png",
		"https://user@assets.example.com/logo.png",
		"https://assets.example.com/logo.png\nimg-src *",
		"https://assets.example.com';img-src */logo.png",
		"https://assets.example.com:99999/logo.png",
		"//assets.example.com/logo.png",
		"data:image/png;base64,AAAA",
	}
	for _, unsafeURL := range unsafeURLs {
		t.Run(unsafeURL, func(t *testing.T) {
			testStore := &connectBrandingTestStore{branding: store.DefaultConnectBranding()}
			body := `{"display_name":"Acme","logo_url":` + jsonString(unsafeURL) + `,"primary_color":"#112233","support_url":"","privacy_url":""}`
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPut, "/workspace/connect-branding", strings.NewReader(body)).WithContext(connectBrandingActorContext())
			UpsertConnectBrandingHandler(testStore).ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || testStore.updateCalls != 0 {
				t.Fatalf("unsafe URL %q = %d calls=%d body=%s", unsafeURL, response.Code, testStore.updateCalls, response.Body.String())
			}
		})
	}
	for _, body := range []string{
		`{"display_name":"","logo_url":"","primary_color":"#112233","support_url":"","privacy_url":""}`,
		`{"display_name":"Acme","logo_url":"","primary_color":"red","support_url":"","privacy_url":""}`,
		`{"display_name":"Acme","logo_url":"","primary_color":"#112233","support_url":"","privacy_url":"","unknown":true}`,
	} {
		testStore := &connectBrandingTestStore{branding: store.DefaultConnectBranding()}
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPut, "/workspace/connect-branding", strings.NewReader(body)).WithContext(connectBrandingActorContext())
		UpsertConnectBrandingHandler(testStore).ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || testStore.updateCalls != 0 {
			t.Fatalf("unsafe branding = %d calls=%d body=%s", response.Code, testStore.updateCalls, response.Body.String())
		}
	}
}

// jsonString quotes a test URL through the production JSON grammar.
func jsonString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// TestConnectBrandingMutationTelemetryUsesOnlyBoundedChangeFacts asserts the
// span cannot retain customer text, URLs, or colors.
func TestConnectBrandingMutationTelemetryUsesOnlyBoundedChangeFacts(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	testStore := &connectBrandingTestStore{branding: store.DefaultConnectBranding()}
	body := `{"display_name":"Sentinel Customer","logo_url":"https://secret-assets.example/logo.png?tenant=sentinel","primary_color":"#123456","support_url":"","privacy_url":""}`
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/workspace/connect-branding", strings.NewReader(body)).WithContext(connectBrandingActorContext())
	UpsertConnectBrandingHandler(testStore).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("branding update status = %d body=%s", response.Code, response.Body.String())
	}
	encoded := encodedConnectInputSpan(recorder.Ended(), "engine.connect_branding.update")
	for _, forbidden := range []string{"Sentinel Customer", "secret-assets.example", "tenant=sentinel", "#123456"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("branding telemetry leaked %q: %s", forbidden, encoded)
		}
	}
	for _, expected := range []string{
		"connect_branding.action=connect_branding.update",
		"connect_branding.outcome=succeeded",
		"connect_branding.error_code=none",
		"actor.type=user",
		"connect_branding.changed_field_count=3",
		"connect_branding.logo_url_changed=true",
	} {
		if !strings.Contains(encoded, expected) {
			t.Fatalf("branding telemetry missing %q: %s", expected, encoded)
		}
	}
	assertConnectBrandingSpanAllowlist(t, recorder.Ended())
}

// assertConnectBrandingSpanAllowlist rejects any attribute outside the reviewed
// fixed action/outcome/actor and boolean/count mutation contract.
func assertConnectBrandingSpanAllowlist(t *testing.T, spans []sdktrace.ReadOnlySpan) {
	t.Helper()
	want := map[string]bool{
		"connect_branding.action": true, "connect_branding.outcome": true, "connect_branding.error_code": true,
		"connect_branding.changed_field_count": true, "connect_branding.display_name_changed": true,
		"connect_branding.logo_url_changed": true, "connect_branding.primary_color_changed": true,
		"connect_branding.support_url_changed": true, "connect_branding.privacy_url_changed": true, "actor.type": true,
	}
	found := false
	for _, span := range spans {
		if span.Name() != "engine.connect_branding.update" {
			continue
		}
		found = true
		if len(span.Attributes()) != len(want) {
			t.Fatalf("branding span attributes = %#v, want exact keys %#v", span.Attributes(), want)
		}
		for _, item := range span.Attributes() {
			if !want[string(item.Key)] {
				t.Fatalf("branding span contains unreviewed attribute %q", item.Key)
			}
		}
	}
	if !found {
		t.Fatal("branding mutation span is missing")
	}
}

// TestConnectBrandingFailureTelemetryClassifiesStableErrors covers denied,
// invalid, read-failure, and write-failure paths without raw error attributes.
func TestConnectBrandingFailureTelemetryClassifiesStableErrors(t *testing.T) {
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
	validBody := `{"display_name":"Acme","logo_url":"","primary_color":"#112233","support_url":"","privacy_url":""}`
	tests := []struct {
		name      string
		body      string
		withActor bool
		store     *connectBrandingTestStore
		outcome   string
		code      string
	}{
		{name: "unauthorized", body: validBody, store: &connectBrandingTestStore{branding: store.DefaultConnectBranding()}, outcome: "denied", code: "unauthorized"},
		{name: "invalid", body: `{}`, withActor: true, store: &connectBrandingTestStore{branding: store.DefaultConnectBranding()}, outcome: "invalid", code: "invalid_request"},
		{name: "load", body: validBody, withActor: true, store: &connectBrandingTestStore{branding: store.DefaultConnectBranding(), loadErr: errors.New("sentinel database URL")}, outcome: "failed", code: "branding_load_failed"},
		{name: "write", body: validBody, withActor: true, store: &connectBrandingTestStore{branding: store.DefaultConnectBranding(), updateErr: errors.New("sentinel row value")}, outcome: "failed", code: "branding_write_failed"},
	}
	for _, test := range tests {
		request := httptest.NewRequest(http.MethodPut, "/workspace/connect-branding", strings.NewReader(test.body))
		if test.withActor {
			request = request.WithContext(connectBrandingActorContext())
		}
		UpsertConnectBrandingHandler(test.store).ServeHTTP(httptest.NewRecorder(), request)
	}
	spans := recorder.Ended()
	if len(spans) != len(tests) {
		t.Fatalf("branding failure spans = %d, want %d", len(spans), len(tests))
	}
	for index, span := range spans {
		encoded := encodedConnectInputSpan([]sdktrace.ReadOnlySpan{span}, "engine.connect_branding.update")
		if !strings.Contains(encoded, "connect_branding.outcome="+tests[index].outcome) || !strings.Contains(encoded, "connect_branding.error_code="+tests[index].code) {
			t.Fatalf("branding failure %s span = %s", tests[index].name, encoded)
		}
		for _, forbidden := range []string{"sentinel database URL", "sentinel row value", "Acme", "#112233"} {
			if strings.Contains(encoded, forbidden) {
				t.Fatalf("branding failure telemetry leaked %q: %s", forbidden, encoded)
			}
		}
	}
	for _, forbidden := range []string{"sentinel database URL", "sentinel row value", "secret-assets.example", "#112233"} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatalf("branding failure logs leaked %q: %s", forbidden, logs.String())
		}
	}
	assertConnectBrandingSpanAllowlist(t, spans)
}

// TestHostedConnectBrandingEscapesHTMLAndPinsLogoCSP verifies customer-owned
// presentation remains data and broadens CSP only to one exact image origin.
func TestHostedConnectBrandingEscapesHTMLAndPinsLogoCSP(t *testing.T) {
	branding, err := validateConnectBranding(store.ConnectBranding{
		DisplayName:  `<script>alert("brand")</script>`,
		LogoURL:      "https://assets.example.com:8443/logo.png?tenant=secret",
		PrimaryColor: "#123456",
		SupportURL:   "https://help.example.com/support",
		PrivacyURL:   "https://legal.example.com/privacy",
	})
	if err != nil {
		t.Fatalf("validate safe branding: %v", err)
	}
	view := hostedConnectBranding{ConnectBranding: branding, LogoOrigin: connectBrandingLogoOrigin(branding.LogoURL)}
	response := httptest.NewRecorder()
	writeConnectInputPage(response, http.StatusOK, connectInputPage{Expired: true, Branding: view})
	body := response.Body.String()
	if strings.Contains(body, `<script>alert`) || !strings.Contains(body, `&lt;script&gt;alert`) {
		t.Fatalf("branding HTML was not escaped: %s", body)
	}
	if !strings.Contains(body, `referrerpolicy="no-referrer"`) || !strings.Contains(body, `width="48" height="48"`) {
		t.Fatalf("logo hardening missing: %s", body)
	}
	csp := response.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "img-src 'self' https://assets.example.com:8443") || strings.Contains(csp, "logo.png") || strings.Contains(csp, "tenant=secret") || strings.Contains(csp, "help.example.com") {
		t.Fatalf("logo CSP is not exact-origin only: %s", csp)
	}
}

// TestHostedConnectBrandingFallsBackOnUnavailableOrCorruptSettings keeps the
// customer connection path usable without trusting invalid persisted values.
func TestHostedConnectBrandingFallsBackOnUnavailableOrCorruptSettings(t *testing.T) {
	tests := []*connectBrandingTestStore{
		{loadErr: errors.New("sentinel unavailable URL")},
		{branding: store.ConnectBranding{DisplayName: "Corrupt", LogoURL: "javascript:alert(1)", PrimaryColor: "red"}},
	}
	for _, testStore := range tests {
		branding := loadHostedConnectBranding(context.Background(), testStore)
		if branding.ConnectBranding != store.DefaultConnectBranding() || branding.LogoOrigin != "" {
			t.Fatalf("fallback branding = %#v", branding)
		}
	}
}

// TestInputAndProviderHandoffRenderConfiguredBranding verifies both pre-auth
// pages carry the customer identity and the hardened external logo contract.
func TestInputAndProviderHandoffRenderConfiguredBranding(t *testing.T) {
	branding := hostedConnectBranding{ConnectBranding: store.ConnectBranding{
		DisplayName: "Acme", LogoURL: "https://assets.example.com/logo.png", PrimaryColor: "#123456",
	}, LogoOrigin: "https://assets.example.com"}
	input := httptest.NewRecorder()
	writeConnectInputPage(input, http.StatusOK, connectInputPage{Branding: branding, Action: "/workspace/connect/input", Fields: []connectInputPageField{{Name: "site", Label: "Site"}}})
	handoff := httptest.NewRecorder()
	writeConnectInputProviderPage(handoff, "https://provider.example/authorize", branding)
	for name, response := range map[string]*httptest.ResponseRecorder{"input": input, "handoff": handoff} {
		body := response.Body.String()
		if !strings.Contains(body, "Acme") || !strings.Contains(body, "https://assets.example.com/logo.png") || !strings.Contains(body, "#123456") {
			t.Fatalf("%s branding page = %s", name, body)
		}
		if !strings.Contains(response.Header().Get("Content-Security-Policy"), "img-src 'self' https://assets.example.com") {
			t.Fatalf("%s branding CSP = %s", name, response.Header().Get("Content-Security-Policy"))
		}
	}
}

// TestCallbackFallbackUsesBrandingForSuccessAndFailure covers both terminal
// Engine-rendered callback surfaces with the same hardened template.
func TestCallbackFallbackUsesBrandingForSuccessAndFailure(t *testing.T) {
	testStore := &connectBrandingTestStore{branding: store.ConnectBranding{
		DisplayName: "Acme", LogoURL: "https://assets.example.com/logo.png", PrimaryColor: "#123456",
	}}
	for _, test := range []struct {
		failed bool
		want   string
	}{
		{failed: false, want: "Connection complete"},
		{failed: true, want: "Connection failed"},
	} {
		response := httptest.NewRecorder()
		writeConnectCallbackFallback(context.Background(), testStore, response, http.StatusOK, "Browser message", test.failed)
		if !strings.Contains(response.Body.String(), test.want) || !strings.Contains(response.Body.String(), "Acme") || !strings.Contains(response.Body.String(), "#123456") {
			t.Fatalf("callback page failed=%v body=%s", test.failed, response.Body.String())
		}
		if !strings.Contains(response.Header().Get("Content-Security-Policy"), "img-src 'self' https://assets.example.com") {
			t.Fatalf("callback CSP failed=%v: %s", test.failed, response.Header().Get("Content-Security-Policy"))
		}
	}
}

// TestCallbackFallbackUsesCanonicalEngineViolet locks the compiled completion
// accent to the same primary token used by the embedded Engine UI.
func TestCallbackFallbackUsesCanonicalEngineViolet(t *testing.T) {
	testStore := &connectBrandingTestStore{branding: store.DefaultConnectBranding()}
	response := httptest.NewRecorder()
	writeConnectCallbackFallback(context.Background(), testStore, response, http.StatusOK, "Browser message", false)
	body := response.Body.String()
	want := "--connect-accent:" + store.DefaultConnectBrandingPrimaryColor
	if !strings.Contains(body, want) {
		t.Fatalf("callback completion accent did not use Engine violet: %s", body)
	}
}

// TestHostedConnectAccentForegroundKeepsActionsReadable covers dark, light,
// canonical, and malformed accents without mutating the configured colour.
func TestHostedConnectAccentForegroundKeepsActionsReadable(t *testing.T) {
	tests := map[string]string{
		"#6941ff": "#ffffff",
		"#000000": "#ffffff",
		"#ffffff": "#000000",
		"#facc15": "#000000",
		"invalid": "#ffffff",
	}
	for accent, want := range tests {
		branding := hostedConnectBranding{ConnectBranding: store.ConnectBranding{PrimaryColor: accent}}
		// Each accent resolves to the one foreground that satisfies the bounded contrast rule.
		if got := branding.AccentForeground(); got != want {
			t.Errorf("AccentForeground(%q) = %q, want %q", accent, got, want)
		}
	}
}

// TestHostedConnectTerminalStatesDoNotRenderStatusDot keeps semantic state in
// explicit text instead of decorative, brand-coloured callback markers.
func TestHostedConnectTerminalStatesDoNotRenderStatusDot(t *testing.T) {
	testStore := &connectBrandingTestStore{branding: store.DefaultConnectBranding()}
	for _, failed := range []bool{false, true} {
		response := httptest.NewRecorder()
		writeConnectCallbackFallback(context.Background(), testStore, response, http.StatusOK, "Browser message", failed)
		body := response.Body.String()
		// Neither terminal branch may retain the old status class or dot markup.
		for _, forbidden := range []string{`class="status"`, `.status{`, `<span class="status"`} {
			if strings.Contains(body, forbidden) {
				t.Errorf("callback failed=%v retained %q: %s", failed, forbidden, body)
			}
		}
		if !strings.Contains(body, `class="connect-eyebrow"`) || !strings.Contains(body, "Connection") {
			t.Errorf("callback failed=%v omitted textual state: %s", failed, body)
		}
	}
}

// TestHostedConnectStatesUseCanonicalLightShell prevents browser dark-mode
// preferences from replacing Engine paper, ink, and line colours.
func TestHostedConnectStatesUseCanonicalLightShell(t *testing.T) {
	branding := hostedConnectBranding{ConnectBranding: store.DefaultConnectBranding()}
	testStore := &connectBrandingTestStore{branding: store.DefaultConnectBranding()}
	pages := map[string]*httptest.ResponseRecorder{}

	normal := httptest.NewRecorder()
	writeConnectInputPage(normal, http.StatusOK, connectInputPage{Branding: branding, Action: "/workspace/connect/input", Fields: []connectInputPageField{{Name: "site", Label: "Site"}}})
	pages["input"] = normal

	invalid := httptest.NewRecorder()
	writeConnectInputPage(invalid, http.StatusBadRequest, connectInputPage{Branding: branding, Action: "/workspace/connect/input", Error: "Invalid value"})
	pages["validation"] = invalid

	expired := httptest.NewRecorder()
	writeConnectInputUnavailable(expired, branding)
	pages["expired"] = expired

	handoff := httptest.NewRecorder()
	writeConnectInputProviderPage(handoff, "https://provider.example/authorize", branding)
	pages["handoff"] = handoff

	for _, failed := range []bool{false, true} {
		callback := httptest.NewRecorder()
		writeConnectCallbackFallback(context.Background(), testStore, callback, http.StatusOK, "Browser message", failed)
		pages[fmt.Sprintf("callback-failed-%t", failed)] = callback
	}

	for name, page := range pages {
		body := page.Body.String()
		// Every state uses the same Engine surface contract and visible accent.
		for _, required := range []string{"color-scheme:only light", "background:#fbfaf8", "color:#15121c", "border:1px solid #e7e2ea", "border-top:3px solid var(--connect-accent)", "--connect-accent:#6941ff"} {
			if !strings.Contains(body, required) {
				t.Errorf("%s page missing %q: %s", name, required, body)
			}
		}
		// Hosted pages remain light even when the browser prefers dark mode.
		for _, forbidden := range []string{"prefers-color-scheme:dark", "color-scheme:light dark", "#09090b", "#18181b"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s page retained dark-shell token %q: %s", name, forbidden, body)
			}
		}
	}
}
