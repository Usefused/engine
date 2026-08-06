package sandbox

import (
	"context"
	"encoding/json"
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
				payload := `{"schema_version":1,"transaction_id":"` + transactionID.String() + `","account_id":"` + accountID.String() + `","installation_id":"` + installationID.String() + `","purpose":"browser_login","provider":"logto","issuer":"https://tenant.logto.test/oidc","external_subject":"subject-1","verified_email":"person@example.com","auth_method":"email_code","enrollment_ref":"local-transaction","authenticated_at":"` + expiresAt.Add(-time.Minute).Format(time.RFC3339) + `","expires_at":"` + expiresAt.Format(time.RFC3339) + `"}`
				return managedIdentityResponse(http.StatusOK, payload), nil
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
	if err != nil || assertion.AccountID != accountID || assertion.InstallationID != installationID || assertion.Purpose != "browser_login" {
		t.Fatalf("ExchangeManagedLoginTransaction = %#v, %v", assertion, err)
	}
	if requests != 2 {
		t.Fatalf("managed identity request count = %d, want 2", requests)
	}
}

func TestManagedIdentityRegistryClientReturnsTypedPendingWithoutProviderBody(t *testing.T) {
	transactionID := uuid.New()
	secret := "provider-body-must-not-leak"
	client := &HTTPRegistryClient{
		endpoint: "https://registry.example/graphql", licenseKey: "engine-license",
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return managedIdentityResponse(http.StatusNotFound, `{"code":"transaction_unavailable","detail":"`+secret+`"}`), nil
		})},
	}
	_, err := client.ExchangeManagedLoginTransaction(context.Background(), transactionID, "registry-verifier")
	if !IsManagedLoginPending(err) {
		t.Fatalf("pending error = %v", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "registry-verifier") {
		t.Fatalf("managed identity error leaked secret: %v", err)
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
