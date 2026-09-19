package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/oauthprovider"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
)

type registrationTestService struct {
	OAuthProviderService
	request oauthprovider.RegisterClientRequest
	calls   int
	err     error
}

// RegisterClient records only registration calls so tests detect unexpected auth dependencies.
func (s *registrationTestService) RegisterClient(_ context.Context, request oauthprovider.RegisterClientRequest) (oauthprovider.RegisterClientResult, error) {
	s.request = request
	s.calls++
	return oauthprovider.RegisterClientResult{ClientID: "foc_test", ClientSecret: "fos_test", ClientIDExpiresAt: time.Now().Add(time.Hour)}, s.err
}

// TestOAuthRegisterWithoutCredential exercises the mounted public endpoint and secret response headers.
func TestOAuthRegisterWithoutCredential(t *testing.T) {
	service := &registrationTestService{}
	router := chi.NewRouter()
	MountOAuthProviderRoutes(router, service, nil, nil, nil, "/login", "https://engine.example")
	request := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(`{"name":"Local client","redirect_uri":"http://localhost:4321/callback","scopes":["service.read"]}`))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	// Public registration must work without any browser session or bearer credential.
	if response.Code != http.StatusCreated || service.calls != 1 {
		t.Fatalf("registration status=%d calls=%d", response.Code, service.calls)
	}
	var body map[string]string
	// Decode the actual wire format clients consume.
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// Both parts of the temporary pair and its deadline are required to connect.
	if body["client_id"] != "foc_test" || body["client_secret"] != "fos_test" || body["client_id_expires_at"] == "" {
		t.Fatal("registration response is incomplete")
	}
	// Credentials must not be retained by shared or browser caches.
	if !strings.Contains(response.Header().Get("Cache-Control"), "no-store") {
		t.Fatal("registration credentials are cacheable")
	}
	// Consent metadata must survive the HTTP boundary unchanged.
	if service.request.RedirectURI != "http://localhost:4321/callback" || service.request.Name != "Local client" || len(service.request.Scopes) != 1 {
		t.Fatal("registration metadata was lost")
	}
}

// TestOAuthRegisterRejectsInvalidInput checks strict input and safe failure responses.
func TestOAuthRegisterRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name, body    string
		err           error
		status, calls int
	}{
		{"malformed", `{`, nil, 400, 0},
		{"extra field", `{"client_secret":"caller-chosen"}`, nil, 400, 0},
		{"trailing document", `{} {}`, nil, 400, 0},
		{"invalid metadata", `{}`, oauthprovider.ErrInvalidRequest, 400, 1},
		{"storage failure", `{}`, errors.New("private database details"), 500, 1},
	}
	for _, test := range cases {
		// Each request gets an isolated recorder and service invocation count.
		t.Run(test.name, func(t *testing.T) {
			service := &registrationTestService{err: test.err}
			response := httptest.NewRecorder()
			oauthRegisterHandler(service)(response, httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(test.body)))
			// Invalid requests cannot issue credentials or leak backend diagnostics.
			if response.Code != test.status || service.calls != test.calls || strings.Contains(response.Body.String(), "fos_test") || strings.Contains(response.Body.String(), "private database") {
				t.Fatalf("unexpected failure response: status=%d calls=%d", response.Code, service.calls)
			}
		})
	}
}

// TestOAuthConsentQuotaMessage makes a full quota actionable without leaking backend details.
func TestOAuthConsentQuotaMessage(t *testing.T) {
	message := oauthConsentFailureMessage(store.ErrOAuthDynamicClientLimit)
	// Direct users to the surface that releases a quota slot.
	if !strings.Contains(message, "10 active") || !strings.Contains(message, "Connected Apps") {
		t.Fatal(message)
	}
	// Other failures retain a generic browser-safe explanation.
	if strings.Contains(oauthConsentFailureMessage(errors.New("private database")), "private database") {
		t.Fatal("internal error leaked")
	}
}
