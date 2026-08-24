package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/observability"
	"github.com/google/uuid"
	"log/slog"
)

// captureLog redirects slog global output to a buffer for the duration of fn.
// This lets S8.1 tests assert that no credential value leaks into structured logs.
func captureLog(fn func()) string {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

// makePassthroughCache builds a richMockCache that serves a single-service scope
// with the given base URL — shared by all passthrough audit tests.
func makePassthroughCache(t *testing.T, vendorURL string) (*richMockCache, string) {
	t.Helper()
	svcID := uuid.New()
	serviceVersionID := uuid.New()
	epID := uuid.New()
	obj := &fusedobject.ServiceMetadata{
		ID:          svcID,
		Name:        "AuditSvc",
		BaseURL:     vendorURL,
		AuthConfigs: fusedobject.AuthConfigs{{Name: "bearerAuth", Type: "http", Scheme: "bearer"}},
	}
	// Runtime dispatch is version-pinned, so the shared fixture must model the
	// same immutable selection contract used by generated SDKs and MCP servers.
	selections := []models.SDKSelection{{
		ServiceID: svcID, ServiceVersionID: serviceVersionID,
		SchemaVersion: models.AppSelectionSchemaVersion, EndpointIDs: []uuid.UUID{epID},
	}}
	scopeJSON, _ := json.Marshal(selections)
	return &richMockCache{
		scopeJSON: scopeJSON, obj: obj, epID: epID,
		securityRequirements: authrouting.Requirements{{Schemes: []authrouting.Requirement{{Scheme: "bearerAuth"}}}},
	}, "do_thing"
}

func makeAnonymousPassthroughCache(t *testing.T, vendorURL string) (*richMockCache, string) {
	cache, endpoint := makePassthroughCache(t, vendorURL)
	cache.obj.AuthConfigs = nil
	cache.securityRequirements = authrouting.Requirements{{Schemes: []authrouting.Requirement{}}}
	return cache, endpoint
}

// ─── AC1: Credential reaches vendor ──────────────────────────────────────────

// TestCredentialPassthrough_ReachesVendorHeader is the authoritative S8.1
// acceptance test: the credential forwarded by the SDK must appear verbatim in
// the outbound HTTP request to the provider. If this test fails, passthrough is
// broken and credentials would silently not authenticate.
func TestCredentialPassthrough_ReachesVendorHeader(t *testing.T) {
	const secretToken = "super-secret-api-key"
	var capturedAuth string

	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer vendor.Close()

	cache, endpointName := makePassthroughCache(t, vendor.URL)
	creds := map[string]any{"bearerAuth": secretToken}
	buf := engine.NewBufferStream()

	err := engineExecuteCore(
		context.Background(), cache, engine.NewDispatcher(),
		&dummyTokenValidator{}, uuid.New().String(), "tok",
		endpointName, map[string]any{}, creds, "", buf,
	)
	if err != nil {
		t.Fatalf("execute error: %v", err)
	}

	// The dispatcher wraps bearer tokens with "Bearer " prefix.
	want := "Bearer " + secretToken
	if capturedAuth != want {
		t.Errorf("passthrough failed: vendor got Authorization=%q, want %q", capturedAuth, want)
	}
}

// ─── AC2: Credential never appears in log output ─────────────────────────────

// TestCredentialPassthrough_NotLoggedByDispatcher asserts that no credential
// value appears in structured log output during an Execute call.
// This is the S8.1 redaction regression guard: if a future change accidentally
// logs the credentials map, this test will catch it before it ships.
func TestCredentialPassthrough_NotLoggedByDispatcher(t *testing.T) {
	const secretToken = "must-not-appear-in-logs"

	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer vendor.Close()

	cache, endpointName := makePassthroughCache(t, vendor.URL)
	creds := map[string]any{"bearerAuth": secretToken}
	buf := engine.NewBufferStream()

	logOutput := captureLog(func() {
		_ = engineExecuteCore(
			context.Background(), cache, engine.NewDispatcher(),
			&dummyTokenValidator{}, uuid.New().String(), "tok",
			endpointName, map[string]any{}, creds, "", buf,
		)
	})

	// The secret value must never appear anywhere in log output.
	if strings.Contains(logOutput, secretToken) {
		t.Errorf("credential leaked into log output:\n%s", logOutput)
	}
}

// ─── AC3: OTEL thread refs contain no credential keys or values ───────────────

// TestCredentialPassthrough_OTELHasNoCredentials asserts that the OTEL thread
// opened by engineExecuteCore does not include any credential keys
// (e.g. "Authorization", "api_key") or their values in AddRefs calls.
//
// Why: OTEL traces are forwarded to observability backends (Grafana, Honeycomb,
// Datadog). A credential appearing in span attrs or refs would be a secret
// exposure even if application logs are clean.
func TestCredentialPassthrough_OTELHasNoCredentials(t *testing.T) {
	const secretToken = "otel-must-not-see-this"

	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer vendor.Close()

	cache, endpointName := makePassthroughCache(t, vendor.URL)

	// Capture all refs that flow into OTEL via a spy on AddRefs.
	var capturedRefs []map[string]string
	spy := &spyThread{onAddRefs: func(refs map[string]string) {
		capturedRefs = append(capturedRefs, refs)
	}}

	// Override observabilityStartFunc so the test controls the thread.
	// This replaces the OTEL exporter without needing a live collector.
	origStart := observabilityStartFunc
	observabilityStartFunc = func(ctx context.Context, name, parentID string, tags ...string) (observability.Thread, error) {
		return spy, nil
	}
	defer func() { observabilityStartFunc = origStart }()

	creds := map[string]any{
		"bearerAuth": secretToken,
		"x-api-key":  secretToken,
	}
	buf := engine.NewBufferStream()

	_ = engineExecuteCore(
		context.Background(), cache, engine.NewDispatcher(),
		&dummyTokenValidator{}, uuid.New().String(), "tok",
		endpointName, map[string]any{}, creds, "", buf,
	)

	// Verify no ref key or value matches any credential key or value.
	forbidden := []string{"Authorization", "x-api-key", secretToken}
	for _, refs := range capturedRefs {
		for k, v := range refs {
			for _, bad := range forbidden {
				if strings.EqualFold(k, bad) {
					t.Errorf("credential key %q found in OTEL span refs", k)
				}
				if strings.EqualFold(v, bad) {
					t.Errorf("credential value found in OTEL span refs under key %q", k)
				}
			}
		}
	}
}

// ─── Spy helpers ─────────────────────────────────────────────────────────────

// spyThread is a mock observability.Thread that records AddRefs calls so tests
// can assert that no credential data flows into the OTEL pipeline.
type spyThread struct {
	onAddRefs func(map[string]string)
}

func (s *spyThread) AddRefs(ctx context.Context, refs map[string]string) observability.Thread {
	if s.onAddRefs != nil {
		s.onAddRefs(refs)
	}
	return s
}
func (s *spyThread) Step(_ string) observability.Step     { return &spyStep{} }
func (s *spyThread) Complete(_ context.Context, _ string) {}
func (s *spyThread) Close(_ context.Context, _ string)    {}

// spyStep is a no-op observability.Step for use with spyThread in tests.
type spyStep struct{}

func (s *spyStep) AddContext(_ map[string]any) observability.Step                     { return s }
func (s *spyStep) AddPrivateContext(_ map[string]any) observability.Step              { return s }
func (s *spyStep) SubStep(_ string, _ map[string]any, _ ...string) observability.Step { return s }
func (s *spyStep) Success(_ context.Context)                                          {}
func (s *spyStep) Failed(_ context.Context, _ string)                                 {}
func (s *spyStep) Error(_ context.Context, _ error)                                   {}

// ─── E2E: Credential Resolution ──────────────────────────────────────────────

// TestCredentialResolution_E2E tests that engineExecuteCore uses the globalSecretResolver
// to fetch workspace credentials and injects them into the outbound request.
func TestCredentialResolution_E2E(t *testing.T) {
	const secretValue = "resolved-workspace-secret"
	var capturedAuth string

	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer vendor.Close()

	cache, endpointName := makePassthroughCache(t, vendor.URL)
	buf := engine.NewBufferStream()

	// Mock resolver
	origResolver := globalSecretResolver
	defer func() { globalSecretResolver = origResolver }()

	globalSecretResolver = &mockSecretResolver{
		creds: map[string]any{"bearerAuth": secretValue},
	}

	err := engineExecuteCore(
		context.Background(), cache, engine.NewDispatcher(),
		&dummyTokenValidator{}, uuid.New().String(), "tok",
		endpointName, map[string]any{}, map[string]any{}, "", buf,
	)
	if err != nil {
		t.Fatalf("execute error: %v", err)
	}

	want := "Bearer " + secretValue
	if capturedAuth != want {
		t.Errorf("credential resolution failed: vendor got Authorization=%q, want %q", capturedAuth, want)
	}
}

type mockSecretResolver struct {
	creds       map[string]any
	passthrough map[string]any
}

func (m *mockSecretResolver) ResolveExecutionCredentials(_ context.Context, request CredentialRequest) (map[string]any, []store.BucketValue, error) {
	// Capture a copy so middleware tests can assert routing metadata without
	// observing later request-local credential enrichment.
	m.passthrough = copyCredentialEnvelope(request.Passthrough)
	out := make(map[string]any)
	for k, v := range request.Passthrough {
		out[k] = v
	}
	for k, v := range m.creds {
		out[k] = v
	}
	return out, nil, nil
}

func (m *mockSecretResolver) GetWebhookSecret(ctx context.Context, accountID, bucketID uuid.UUID, secretRef string) (string, error) {
	return "", nil
}
