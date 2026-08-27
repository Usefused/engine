package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestManagedIdentityRegistryClientUsesEngineIdentityAndDecodesContracts(t *testing.T) {
	transactionID, accountID := uuid.New(), uuid.New()
	installationID, runtimeID := uuid.New(), uuid.New()
	expiresAt := time.Now().UTC().Add(10 * time.Minute).Truncate(time.Second)
	logoutExpiresAt := expiresAt.Add(7 * time.Hour)
	requests := 0
	client := &HTTPRegistryClient{
		endpoint: "https://registry.example/graphql", licenseKey: "engine-license",
		installationID: installationID, runtimeInstanceID: runtimeID,
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests++
			assertManagedIdentityRequestHeaders(t, request, installationID, runtimeID)
			switch request.URL.Path {
			case "/api/engine/identity/transactions":
				var body map[string]string
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Fatalf("decode transaction request: %v", err)
				}
				if body["purpose"] != "browser_login" || body["engine_verifier"] != "registry-verifier" || body["enrollment_ref"] != "local-transaction" {
					t.Fatalf("unexpected transaction body: %#v", body)
				}
				payload := `{"transaction_id":"` + transactionID.String() + `","verification_url":"https://auth.usefused.test/login","expires_at":"` + expiresAt.Format(time.RFC3339) + `"}`
				return managedIdentityResponse(http.StatusCreated, payload), nil
			case "/api/engine/identity/transactions/" + transactionID.String() + "/exchange":
				payload := `{"schema_version":1,"transaction_id":"` + transactionID.String() + `","account_id":"` + accountID.String() + `","installation_id":"` + installationID.String() + `","purpose":"browser_login","provider":"logto","issuer":"https://tenant.logto.test/oidc","external_subject":"subject-1","verified_email":"person@example.com","auth_method":"email_code","enrollment_ref":"local-transaction","authenticated_at":"` + expiresAt.Add(-time.Minute).Format(time.RFC3339) + `","expires_at":"` + expiresAt.Format(time.RFC3339) + `","logout_token":"opaque-logout","logout_expires_at":"` + logoutExpiresAt.Format(time.RFC3339) + `"}`
				return managedIdentityResponse(http.StatusOK, payload), nil
			case "/api/engine/identity/logout":
				var body map[string]string
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Fatalf("decode logout request: %v", err)
				}
				if body["logout_token"] != "opaque-logout" || body["return_url"] != "https://engine.test/login" {
					t.Fatalf("unexpected logout body: %#v", body)
				}
				return managedIdentityResponse(http.StatusOK, `{"logout_url":"https://tenant.logto.test/oidc/session/end?state=safe"}`), nil
			default:
				t.Fatalf("unexpected managed identity path: %s", request.URL.Path)
				return nil, nil
			}
		})},
	}

	transaction, err := client.CreateManagedLoginTransaction(context.Background(), "registry-verifier", "local-transaction")
	if err != nil || transaction.TransactionID != transactionID || transaction.ExpiresAt != expiresAt {
		t.Fatalf("CreateManagedLoginTransaction = %#v, %v", transaction, err)
	}
	assertion, err := client.ExchangeManagedLoginTransaction(context.Background(), transactionID, "registry-verifier")
	if err != nil || assertion.AccountID != accountID || assertion.InstallationID != installationID || assertion.Purpose != "browser_login" || assertion.LogoutToken != "opaque-logout" {
		t.Fatalf("ExchangeManagedLoginTransaction = %#v, %v", assertion, err)
	}
	logoutURL, err := client.StartManagedLogout(context.Background(), assertion.LogoutToken, "https://engine.test/login")
	if err != nil || !strings.HasPrefix(logoutURL, "https://tenant.logto.test/") {
		t.Fatalf("StartManagedLogout = %q, %v", logoutURL, err)
	}
	if requests != 3 {
		t.Fatalf("managed identity request count = %d, want 3", requests)
	}
}

// TestManagedIdentityRegistryClientReturnsTypedPendingWithoutProviderBody
// verifies nested Registry metadata survives without reflecting error prose.
func TestManagedIdentityRegistryClientReturnsTypedPendingWithoutProviderBody(t *testing.T) {
	transactionID := uuid.New()
	secret := "provider-body-must-not-leak"
	traceID := "1234567890abcdef1234567890abcdef"
	client := &HTTPRegistryClient{
		endpoint: "https://registry.example/graphql", licenseKey: "engine-license",
		// The fixture includes hostile prose to prove only allowlisted fields are retained.
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return managedIdentityResponse(http.StatusNotFound, `{"error":{"code":"transaction_unavailable","message":"`+secret+`","category":"not_found","retryable":false,"request_id":"registry/identity-42","trace_id":"`+traceID+`","detail":"`+secret+`"}}`), nil
		})},
	}
	_, err := client.ExchangeManagedLoginTransaction(context.Background(), transactionID, "registry-verifier")
	// The stable code remains the sole state-machine signal for a pending exchange.
	if !IsManagedLoginPending(err) {
		t.Fatalf("pending error = %v", err)
	}
	var registryErr ManagedIdentityRegistryError
	// Typed metadata allows operators to correlate the exact Registry failure.
	if !errors.As(err, &registryErr) || registryErr.Code != "transaction_unavailable" || registryErr.RequestID != "registry/identity-42" || registryErr.TraceID != traceID {
		t.Fatalf("typed managed identity error = %#v, %v", registryErr, err)
	}
	// Registry message/detail fields and request secrets never enter Engine error prose.
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "registry-verifier") {
		t.Fatalf("managed identity error leaked secret: %v", err)
	}
}

// TestManagedIdentityRegistryClientRejectsLegacyTopLevelCode proves the old
// private response shape cannot drive the managed-login state machine.
func TestManagedIdentityRegistryClientRejectsLegacyTopLevelCode(t *testing.T) {
	secret := "legacy-provider-detail"
	client := &HTTPRegistryClient{
		endpoint: "https://registry.example/graphql", licenseKey: "engine-license",
		// The legacy fixture carries the former signal plus prose that must remain ignored.
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return managedIdentityResponse(http.StatusNotFound, `{"code":"transaction_unavailable","detail":"`+secret+`"}`), nil
		})},
	}
	_, err := client.ExchangeManagedLoginTransaction(context.Background(), uuid.New(), "registry-verifier")
	// Only the shared nested envelope may classify an exchange as still pending.
	if IsManagedLoginPending(err) {
		t.Fatalf("legacy response was classified as pending: %v", err)
	}
	var registryErr ManagedIdentityRegistryError
	// HTTP status remains diagnostic, but the legacy classifier is discarded.
	if !errors.As(err, &registryErr) || registryErr.Status != http.StatusNotFound || registryErr.Code != "" {
		t.Fatalf("legacy managed identity error = %#v, %v", registryErr, err)
	}
	// Unknown legacy fields never become Engine-visible error prose.
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "registry-verifier") {
		t.Fatalf("legacy managed identity error leaked secret: %v", err)
	}
}

// TestSafeManagedIdentityRegistryCorrelationIDPreservesGrammar locks the
// bounded ASCII allowlist used before remote identifiers reach diagnostics.
func TestSafeManagedIdentityRegistryCorrelationIDPreservesGrammar(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{name: "allowed punctuation", value: " registry/Identity-42:part.one ", want: "registry/Identity-42:part.one"},
		{name: "empty", value: "   "},
		{name: "embedded space", value: "registry identity"},
		{name: "Unicode alias", value: "registry/identité"},
		{name: "prose punctuation", value: "registry?id=42"},
		{name: "oversized", value: strings.Repeat("a", 129)},
	}
	for _, testCase := range cases {
		// Each case validates the complete sanitizer rather than its internal helper.
		t.Run(testCase.name, func(t *testing.T) {
			got := safeManagedIdentityRegistryCorrelationID(testCase.value)
			// Invalid remote values must collapse to the empty diagnostic field.
			if got != testCase.want {
				t.Fatalf("safeManagedIdentityRegistryCorrelationID(%q) = %q, want %q", testCase.value, got, testCase.want)
			}
		})
	}
}

func assertManagedIdentityRequestHeaders(t *testing.T, request *http.Request, installationID, runtimeID uuid.UUID) {
	t.Helper()
	if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer engine-license" || request.Header.Get("X-API-Key") != "engine-license" {
		t.Fatalf("managed identity request did not use Engine licence identity: %s %#v", request.Method, request.Header)
	}
	if request.Header.Get("X-Fused-Installation-ID") != installationID.String() || request.Header.Get("X-Fused-Runtime-Instance-ID") != runtimeID.String() {
		t.Fatalf("managed identity request did not include Engine identity: %#v", request.Header)
	}
}

func managedIdentityResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
