package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type appTokenHandlerStore struct {
	store.Store
	createdIssue    store.AppTokenIssue
	createCalls     int
	createErr       error
	revokedFamilyID uuid.UUID
	revokedName     string
	revokeCalls     int
}

func (s *appTokenHandlerStore) CreateAppToken(_ context.Context, issue store.AppTokenIssue) (*store.AppTokenMetadata, error) {
	s.createdIssue = issue
	s.createCalls++
	if s.createErr != nil {
		return nil, s.createErr
	}
	return &store.AppTokenMetadata{
		ID: issue.ID, AppFamilyID: issue.AppFamilyID, Name: issue.Name, AppTokenPolicy: issue.Policy,
		BindingMode: issue.BindingMode, CreatedAt: time.Now().UTC(),
	}, nil
}

func (s *appTokenHandlerStore) RevokeAppToken(_ context.Context, appFamilyID uuid.UUID, name string) (*store.AppTokenRevocation, error) {
	s.revokedFamilyID = appFamilyID
	s.revokedName = name
	s.revokeCalls++
	return &store.AppTokenRevocation{TokenID: uuid.New(), AppFamilyID: appFamilyID, RevokedAt: time.Now().UTC()}, nil
}

func TestGenerateAppTokenHandlerCreatesScopedExpiringTokenOnce(t *testing.T) {
	exporter := setupTestTracer(t)
	familyID := uuid.New()
	accountID := uuid.New()
	repository := &appTokenHandlerStore{}
	body := bytes.NewBufferString(`{"name":"support-agent","allow":["tickets.read","tickets.close"],"expires_in":300}`)
	request := httptest.NewRequest(http.MethodPost, "/workspace/app-tokens?app_family_id="+familyID.String(), body)
	request = controlTestRequest(request, accountID)
	response := httptest.NewRecorder()

	GenerateAppTokenHandler(repository).ServeHTTP(response, request)
	payload := decodeTokenGenerateResponse(t, response)
	assertScopedTokenIssue(t, response, repository, familyID, payload)
	assertAppTokenSpansAreSafe(t, exporter.GetSpans(), payload.Token, repository.createdIssue.TokenHash)
}

func TestGenerateAppTokenHandlerDefaultsToUnrestrictedNonExpiringToken(t *testing.T) {
	repository := &appTokenHandlerStore{}
	familyID := uuid.New()
	request := httptest.NewRequest(http.MethodPost, "/workspace/app-tokens?app_family_id="+familyID.String(), strings.NewReader(`{"name":"default"}`))
	request = controlTestRequest(request, uuid.New())
	response := httptest.NewRecorder()

	GenerateAppTokenHandler(repository).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if !repository.createdIssue.Policy.AllowAll || len(repository.createdIssue.Policy.AllowedOperations) != 0 || repository.createdIssue.Policy.ExpiresAt != nil {
		t.Fatalf("default policy = %#v, want unrestricted non-expiring", repository.createdIssue.Policy)
	}
	var payload struct {
		Allow     []string   `json:"allow"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Allow) != 1 || payload.Allow[0] != "*" || payload.ExpiresAt != nil {
		t.Fatalf("response policy = %#v/%v, want [*]/nil", payload.Allow, payload.ExpiresAt)
	}
}

type tokenGenerateResponse struct {
	Token        string                    `json:"token"`
	Allow        []string                  `json:"allow"`
	ExpiresAt    *time.Time                `json:"expires_at"`
	BindingMode  store.AppTokenBindingMode `json:"binding_mode"`
	BindingCount int                       `json:"binding_count"`
}

func decodeTokenGenerateResponse(t *testing.T, response *httptest.ResponseRecorder) tokenGenerateResponse {
	t.Helper()
	var payload tokenGenerateResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return payload
}

func assertScopedTokenIssue(t *testing.T, response *httptest.ResponseRecorder, repository *appTokenHandlerStore, familyID uuid.UUID, payload tokenGenerateResponse) {
	t.Helper()
	assertTokenResponseHeaders(t, response)
	assertTokenIssueIdentity(t, repository, familyID)
	assertScopedTokenPolicy(t, repository.createdIssue.Policy)
	assertOneTimeTokenResponse(t, response, repository.createdIssue.TokenHash, payload)
}

func assertTokenResponseHeaders(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("token response status/headers = %d/%#v", response.Code, response.Header())
	}
}

func assertTokenIssueIdentity(t *testing.T, repository *appTokenHandlerStore, familyID uuid.UUID) {
	t.Helper()
	issue := repository.createdIssue
	if repository.createCalls != 1 || issue.AppFamilyID != familyID || issue.Name != "support-agent" || issue.IssuedBySubjectID == nil || issue.IssuedByCredentialID == nil {
		t.Fatalf("create issue = %#v (calls %d)", issue, repository.createCalls)
	}
}

func assertScopedTokenPolicy(t *testing.T, policy store.AppTokenPolicy) {
	t.Helper()
	if policy.AllowAll || len(policy.AllowedOperations) != 2 || policy.ExpiresAt == nil {
		t.Fatalf("persisted policy = %#v, want scoped expiring policy", policy)
	}
	if remaining := time.Until(*policy.ExpiresAt); remaining < 295*time.Second || remaining > 301*time.Second {
		t.Fatalf("persisted expiry remaining = %s, want about 5m", remaining)
	}
}

func assertOneTimeTokenResponse(t *testing.T, response *httptest.ResponseRecorder, tokenHash string, payload tokenGenerateResponse) {
	t.Helper()
	if payload.Token == "" || payload.ExpiresAt == nil || payload.BindingMode != store.AppTokenBindingDynamic || payload.BindingCount != 0 {
		t.Fatalf("response payload = %#v, want one-time dynamic token and expiry", payload)
	}
	digest := sha256.Sum256([]byte(payload.Token))
	if tokenHash != hex.EncodeToString(digest[:]) || tokenHash == payload.Token || strings.Contains(response.Body.String(), tokenHash) {
		t.Fatal("one-time token/hash boundary was not preserved")
	}
	if strings.Join(payload.Allow, ",") != "tickets.close,tickets.read" {
		t.Fatalf("response allow = %#v, want normalized operations", payload.Allow)
	}
}

func TestGenerateAppTokenHandlerRejectsInvalidPolicyAndJSON(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "empty allow", body: `{"name":"agent","allow":[]}`},
		{name: "wildcard mixed with operation", body: `{"name":"agent","allow":["*","tickets.read"]}`},
		{name: "non-positive expiry", body: `{"name":"agent","expires_in":0}`},
		{name: "unknown field", body: `{"name":"agent","permissions":["tickets.read"]}`},
		{name: "legacy service id binding", body: `{"name":"agent","binding_mode":"fixed","bindings":[{"service_id":"de305d54-75b4-431b-adb2-eb6b9e546014","auth_name":"oauth2","end_user_ref":"customer"}]}`},
		{name: "fixed without bindings", body: `{"name":"agent","binding_mode":"fixed"}`},
		{name: "trailing json", body: `{"name":"agent"} {}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := &appTokenHandlerStore{}
			request := httptest.NewRequest(http.MethodPost, "/workspace/app-tokens?app_family_id="+uuid.NewString(), strings.NewReader(test.body))
			request = controlTestRequest(request, uuid.New())
			response := httptest.NewRecorder()

			GenerateAppTokenHandler(repository).ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
			}
			if repository.createCalls != 0 {
				t.Fatalf("CreateAppToken called %d time(s) for invalid request", repository.createCalls)
			}
		})
	}
}

func TestGenerateAppTokenHandlerReportsUnavailableFixedBindingAsInvalid(t *testing.T) {
	repository := &appTokenHandlerStore{createErr: store.ErrAppTokenBindingInvalid}
	request := httptest.NewRequest(http.MethodPost, "/workspace/app-tokens?app_family_id="+uuid.NewString(), strings.NewReader(`{
		"name":"agent","binding_mode":"fixed","bindings":[{
			"service_slug":"google-drive","auth_name":"google","end_user_ref":"customer-1"
		}]
	}`))
	request = controlTestRequest(request, uuid.New())
	response := httptest.NewRecorder()

	GenerateAppTokenHandler(repository).ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest || repository.createCalls != 1 {
		t.Fatalf("status/create calls = %d/%d, want 400/1: %s", response.Code, repository.createCalls, response.Body.String())
	}
	if len(repository.createdIssue.Bindings) != 1 || repository.createdIssue.Bindings[0].ServiceSlug != "google-drive" {
		t.Fatalf("fixed binding forwarded as %#v", repository.createdIssue.Bindings)
	}
}

func TestRevokeAppTokenHandlerUsesGenericRouteParameters(t *testing.T) {
	exporter := setupTestTracer(t)
	repository := &appTokenHandlerStore{}
	familyID := uuid.New()
	request := httptest.NewRequest(http.MethodDelete, "/workspace/app-tokens?app_family_id="+familyID.String()+"&name=agent", nil)
	request = controlTestRequest(request, uuid.New())
	response := httptest.NewRecorder()

	RevokeAppTokenHandler(repository).ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", response.Code, response.Body.String())
	}
	if repository.revokeCalls != 1 || repository.revokedFamilyID != familyID || repository.revokedName != "agent" {
		t.Fatalf("revoke call = %d/%s/%q", repository.revokeCalls, repository.revokedFamilyID, repository.revokedName)
	}
	if !hasSpanWithAttributes(exporter.GetSpans(), "engine.api.app_tokens.revoke", map[string]string{
		"actor.type": string(accesscontrol.SubjectUser), "outcome": "revoked",
	}) {
		t.Fatal("revoke span missing safe actor/outcome attributes")
	}
}

func TestAppTokenHandlersRequireAuthenticatedActor(t *testing.T) {
	repository := &appTokenHandlerStore{}
	request := httptest.NewRequest(http.MethodPost, "/workspace/app-tokens?app_family_id="+uuid.NewString(), strings.NewReader(`{"name":"agent"}`))
	response := httptest.NewRecorder()

	GenerateAppTokenHandler(repository).ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized || repository.createCalls != 0 {
		t.Fatalf("status/create calls = %d/%d, want 401/0", response.Code, repository.createCalls)
	}
}

func assertAppTokenSpansAreSafe(t *testing.T, spans []tracetest.SpanStub, plaintext, tokenHash string) {
	t.Helper()
	for _, span := range spans {
		for _, attr := range span.Attributes {
			value := attr.Value.Emit()
			if strings.Contains(value, plaintext) || strings.Contains(value, tokenHash) || strings.Contains(value, "tickets.") {
				t.Fatalf("span %q attribute %q exposed secret or raw scope data", span.Name, attr.Key)
			}
		}
	}
	if !hasSpanWithAttributes(spans, "engine.api.app_tokens.generate", map[string]string{"actor.type": string(accesscontrol.SubjectUser), "outcome": "created"}) {
		t.Fatal("generate span missing safe actor/outcome attributes")
	}
	if !hasSpanWithAttributes(spans, "engine.applifecycle.generate_token", map[string]string{"app.token.allow_all": "false", "app.token.expiry_present": "true", "outcome": "created"}) {
		t.Fatal("lifecycle span missing bounded policy attributes")
	}
}

func hasSpanWithAttributes(spans []tracetest.SpanStub, name string, want map[string]string) bool {
	for _, span := range spans {
		if span.Name != name {
			continue
		}
		attributes := make(map[string]string, len(span.Attributes))
		for _, attr := range span.Attributes {
			attributes[string(attr.Key)] = attr.Value.Emit()
		}
		matched := true
		for key, value := range want {
			if attributes[key] != value {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func TestTokenMutationFailureTelemetryIsBounded(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})

	_, span := otel.Tracer("engine").Start(context.Background(), "token-mutation")
	recordTokenMutationError(span, "failed")
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("ended span count = %d, want 1", len(ended))
	}
	if len(ended[0].Events()) != 0 {
		t.Fatal("token mutation failures must not record raw error events")
	}
	if got := ended[0].Status().Description; got != "failed" {
		t.Fatalf("status description = %q, want failed", got)
	}
}
