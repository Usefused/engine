package sandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestHTTPRegistryClientGenerateSDKUsesLicensedBoundary proves durable replay
// reaches the REST endpoint with Engine identity and decodes an accepted job.
func TestHTTPRegistryClientGenerateSDKUsesLicensedBoundary(t *testing.T) {
	t.Setenv("FUSED_ENV", "development")
	appID, familyID, accountID := uuid.New(), uuid.New(), uuid.New()
	installationID, runtimeID := uuid.New(), uuid.New()
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Replay must target the Registry REST endpoint rather than the configured GraphQL path.
		if r.URL.Path != "/sdks/generate" || r.Method != http.MethodPost {
			t.Fatalf("generation request = %s %s", r.Method, r.URL.Path)
		}
		// Only the licensed Engine identity may cross this boundary.
		if r.Header.Get("Authorization") != "Bearer engine-license" || r.Header.Get("X-API-Key") != "engine-license" {
			t.Fatalf("licensed headers = Authorization %q, X-API-Key %q", r.Header.Get("Authorization"), r.Header.Get("X-API-Key"))
		}
		// Durable Engine identities bind the replay to this exact installation and runtime.
		if r.Header.Get("X-Fused-Installation-ID") != installationID.String() || r.Header.Get("X-Fused-Runtime-Instance-ID") != runtimeID.String() {
			t.Fatalf("Engine identity headers = %q, %q", r.Header.Get("X-Fused-Installation-ID"), r.Header.Get("X-Fused-Runtime-Instance-ID"))
		}
		var request models.SDKGenerationRequest
		// The exact deterministic app and job request must survive encoding.
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.AppID != appID || request.AppFamilyID != familyID || request.IdempotencyKey != "plan-1" {
			t.Fatalf("generation request = %+v, error = %v", request, err)
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(models.SDKGenerationResult{
			AppID: appID, AppFamilyID: familyID, AccountID: accountID, JobID: "job-1",
			Status: models.SDKGenerationStatusPending, ScopeSchemaVersion: models.AppScopeSchemaVersion,
			GeneratorVersion: models.SDKGeneratorVersion,
		})
	}))
	t.Cleanup(registry.Close)

	client := NewHTTPRegistryClient(registry.URL+"/graphql", "engine-license")
	// Production construction configures these identities immediately after handshake.
	if err := client.ConfigureEngineIdentity(installationID, runtimeID); err != nil {
		t.Fatalf("configure Engine identity: %v", err)
	}
	result, err := client.GenerateSDK(context.Background(), models.SDKGenerationRequest{
		AppID: appID, AppFamilyID: familyID, IdempotencyKey: "plan-1",
	})
	// Accepted async generation must remain pending rather than being treated as an HTTP error.
	if err != nil || result.AppID != appID || result.JobID != "job-1" || result.Status != models.SDKGenerationStatusPending {
		t.Fatalf("GenerateSDK() = %+v, %v", result, err)
	}
}

// TestHTTPRegistryClientGenerateSDKBoundsAndRedactsFailures proves malformed
// Registry output cannot leak generated content or exhaust Engine memory.
func TestHTTPRegistryClientGenerateSDKBoundsAndRedactsFailures(t *testing.T) {
	t.Setenv("FUSED_ENV", "development")
	secret := "should-not-cross-error-boundary"
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(secret))
	}))
	t.Cleanup(registry.Close)
	_, err := NewHTTPRegistryClient(registry.URL, "engine-license").GenerateSDK(context.Background(), models.SDKGenerationRequest{})
	// Only the status classification is safe to expose or log from a Registry rejection.
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "409") {
		t.Fatalf("GenerateSDK() error = %v", err)
	}

	_, err = readBoundedSDKGenerationResponse(strings.NewReader(strings.Repeat("x", maxSDKGenerationResponseBytes+1)))
	// The reader consumes at most one proof byte beyond the configured response ceiling.
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("oversized response error = %v", err)
	}
}
