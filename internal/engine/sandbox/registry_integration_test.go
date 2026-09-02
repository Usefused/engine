package sandbox

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

func TestHTTPRegistryClient_Handshake(t *testing.T) {
	os.Setenv("FUSED_ENV", "development")
	defer os.Unsetenv("FUSED_ENV")
	// 1. Setup mock registry server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer valid_license_key" {
			http.Error(w, "invalid license key", http.StatusUnauthorized)
			return
		}

		if r.URL.Path == "/api/engine/handshake" {
			resp := map[string]any{
				"account_id":     "mock-acc-123",
				"workspace_name": "Integration Test Workspace",
				"entitlements": map[string]any{
					"plan":                            "commercial",
					"heartbeat_required":              true,
					"usage_reporting":                 "aggregate",
					"public_service_insights_enabled": true,
					"heartbeat_interval_seconds":      30,
					"heartbeat_stale_after_seconds":   90,
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}

		if r.URL.Path == "/graphql" {
			resp := map[string]interface{}{
				"data": map[string]interface{}{
					"services": []map[string]string{
						{"id": "svc-1", "name": "Stripe", "description": "Payments"},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}

		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer ts.Close()

	// 2. Initialize engine client
	// Note: We pass the GraphQL endpoint so the client can strip it appropriately.
	graphqlEndpoint := ts.URL + "/graphql"
	client := NewHTTPRegistryClient(graphqlEndpoint, "valid_license_key")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 3. Test Cases
	t.Run("Handshake_Success", func(t *testing.T) {
		accID, wsName, err := client.Handshake(ctx)
		if err != nil {
			t.Fatalf("expected success, got error: %v", err)
		}
		if accID != "mock-acc-123" {
			t.Errorf("expected account ID mock-acc-123, got %s", accID)
		}
		if wsName != "Integration Test Workspace" {
			t.Errorf("expected workspace name Integration Test Workspace, got %s", wsName)
		}
	})

	t.Run("HandshakeWithEntitlements_Success", func(t *testing.T) {
		result, err := client.HandshakeWithEntitlements(ctx)
		if err != nil {
			t.Fatalf("expected success, got error: %v", err)
		}
		if result.Entitlements.HeartbeatIntervalSeconds != 30 || result.Entitlements.HeartbeatStaleAfterSeconds != 90 {
			t.Fatalf("unexpected entitlement bundle: %#v", result.Entitlements)
		}
		assertPublicInsightContract(t, result.Entitlements, true)
	})

	t.Run("Handshake_LocalMode_EmptyKey", func(t *testing.T) {
		localClient := NewHTTPRegistryClient(graphqlEndpoint, "")
		_, _, err := localClient.Handshake(ctx)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if err.Error() != "FUSED_LICENSE_KEY is required but was not provided" {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("Handshake_Failure_InvalidKey", func(t *testing.T) {
		invalidClient := NewHTTPRegistryClient(graphqlEndpoint, "invalid_license_key")
		_, _, err := invalidClient.Handshake(ctx)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if err.Error() != "handshake failed with status 401: invalid license key\n" {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("SearchCatalogue_Success", func(t *testing.T) {
		services, err := client.SearchCatalogue(ctx, "Stripe", 1, 10)
		if err != nil {
			t.Fatalf("expected success, got error: %v", err)
		}
		if len(services) != 1 {
			t.Fatalf("expected 1 service, got %d", len(services))
		}
		if services[0].Name != "Stripe" {
			t.Errorf("expected Stripe, got %s", services[0].Name)
		}
	})
}

func TestHTTPRegistryClient_HandshakeDefaultsEntitlementsForOlderRegistry(t *testing.T) {
	os.Setenv("FUSED_ENV", "development")
	defer os.Unsetenv("FUSED_ENV")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertRegistryLicenseHeaders(t, r, "valid_license_key")
		_ = json.NewEncoder(w).Encode(map[string]string{"account_id": "acc", "workspace_name": "Workspace"})
	}))
	defer ts.Close()

	client := NewHTTPRegistryClient(ts.URL+"/graphql", "valid_license_key")
	result, err := client.HandshakeWithEntitlements(context.Background())
	if err != nil {
		t.Fatalf("HandshakeWithEntitlements: %v", err)
	}
	got := result.Entitlements.Normalized()
	want := models.DefaultRuntimeEntitlement().Normalized()
	if *got.MaxBuckets != *want.MaxBuckets ||
		*got.MaxAPIFamilies != *want.MaxAPIFamilies ||
		*got.MaxSDKFamilies != *want.MaxSDKFamilies ||
		*got.MaxMCPFamilies != *want.MaxMCPFamilies ||
		*got.MaxServices != *want.MaxServices ||
		*got.MaxSandboxConcurrency != *want.MaxSandboxConcurrency ||
		*got.ExecutionRetentionDays != *want.ExecutionRetentionDays ||
		got.Plan != want.Plan ||
		got.HeartbeatRequired != want.HeartbeatRequired ||
		got.UsageReporting != want.UsageReporting ||
		got.HeartbeatIntervalSeconds != want.HeartbeatIntervalSeconds ||
		got.HeartbeatStaleAfterSeconds != want.HeartbeatStaleAfterSeconds ||
		got.DriftMonitoringEnabled != want.DriftMonitoringEnabled ||
		got.WebhookIngestionEnabled != want.WebhookIngestionEnabled ||
		got.SSOEnabled != want.SSOEnabled {
		t.Fatalf("expected default entitlement, got %#v", result.Entitlements)
	}
	assertPublicInsightContract(t, got, want.PublicServiceInsightsEnabled)
}

func assertPublicInsightContract(t *testing.T, entitlement models.RuntimeEntitlement, wantEnabled bool) {
	t.Helper()
	if entitlement.PublicServiceInsightsEnabled != wantEnabled {
		t.Fatalf("PublicServiceInsightsEnabled = %v, want %v", entitlement.PublicServiceInsightsEnabled, wantEnabled)
	}
}

func TestHTTPRegistryClient_SendHeartbeat(t *testing.T) {
	os.Setenv("FUSED_ENV", "development")
	defer os.Unsetenv("FUSED_ENV")

	var sawHeartbeat bool
	installationID, runtimeID := uuid.New(), uuid.New()
	ts := httptest.NewServer(heartbeatFixtureHandler(t, installationID, runtimeID, &sawHeartbeat))
	defer ts.Close()

	client := NewHTTPRegistryClient(ts.URL+"/graphql", "valid_license_key")
	if err := client.ConfigureEngineIdentity(installationID, runtimeID); err != nil {
		t.Fatalf("ConfigureEngineIdentity: %v", err)
	}
	resp, err := client.SendHeartbeat(context.Background(), "1.2.3", "abc123", "scale-up", "revision-1", time.Now())
	if err != nil {
		t.Fatalf("SendHeartbeat: %v", err)
	}
	if resp == nil || resp.Status != "ok" {
		t.Fatalf("unexpected heartbeat response: %+v", resp)
	}
	if !sawHeartbeat {
		t.Fatal("expected heartbeat request")
	}
}

func heartbeatFixtureHandler(t *testing.T, installationID, runtimeID uuid.UUID, sawHeartbeat *bool) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/engine/heartbeat" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		assertRegistryLicenseHeaders(t, r, "valid_license_key")
		if r.Header.Get("X-Fused-Installation-ID") != installationID.String() || r.Header.Get("X-Fused-Runtime-Instance-ID") != runtimeID.String() {
			t.Fatalf("missing Engine process identity headers: %v", r.Header)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if r.Header.Get("X-Engine-Signature") != testHeartbeatSignature("valid_license_key", body) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		var request EngineHeartbeatRequest
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatalf("decode heartbeat: %v", err)
		}
		if request.EngineVersion != "1.2.3" || request.EngineBuildHash != "abc123" {
			t.Fatalf("unexpected heartbeat identity: %+v", request)
		}
		if request.AppliedPlan != "scale-up" || request.AppliedEntitlementRevision != "revision-1" {
			t.Fatalf("unexpected entitlement acknowledgement: %+v", request)
		}
		*sawHeartbeat = true
		_ = json.NewEncoder(w).Encode(EngineHeartbeatResponse{Status: "ok"})
	}
}

func TestHTTPRegistryClient_HandshakeDecodesManagedIdentityCapability(t *testing.T) {
	os.Setenv("FUSED_ENV", "development")
	defer os.Unsetenv("FUSED_ENV")

	installationID := uuid.New()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"account_id": "acc", "workspace_name": "Workspace", "owner_email": "owner@example.com",
			"identity": map[string]any{
				"protocol_version": 1, "available": false,
				"organization_status": "not_configured",
				"installation_id":     installationID.String(),
			},
		})
	}))
	defer ts.Close()

	result, err := NewHTTPRegistryClient(ts.URL+"/graphql", "valid_license_key").HandshakeWithEntitlements(context.Background())
	if err != nil {
		t.Fatalf("HandshakeWithEntitlements: %v", err)
	}
	if result.Identity.ProtocolVersion != 1 || result.Identity.InstallationID != installationID.String() {
		t.Fatalf("unexpected managed identity capability: %#v", result.Identity)
	}
	if result.OwnerEmail != "owner@example.com" {
		t.Fatalf("owner email = %q, want owner@example.com", result.OwnerEmail)
	}
}

func TestHTTPRegistryClient_SendUsageReports(t *testing.T) {
	os.Setenv("FUSED_ENV", "development")
	defer os.Unsetenv("FUSED_ENV")

	var sawUsage bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/engine/usage-reports" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		assertRegistryLicenseHeaders(t, r, "valid_license_key")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if r.Header.Get("X-Engine-Signature") != testHeartbeatSignature("valid_license_key", body) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		var req EngineUsageReportRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode usage report: %v", err)
		}
		if len(req.Reports) != 1 || req.Reports[0].Metric != models.EngineUsageMetricExecutionTotal {
			t.Fatalf("unexpected usage reports: %#v", req.Reports)
		}
		sawUsage = true
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewHTTPRegistryClient(ts.URL+"/graphql", "valid_license_key")
	err := client.SendUsageReports(context.Background(), "1.2.3", "abc123", []models.EngineUsageReport{{
		ReportID:      uuid.New(),
		Metric:        models.EngineUsageMetricExecutionTotal,
		BucketStart:   time.Now().UTC().Truncate(time.Minute),
		BucketSeconds: 60,
		Count:         1,
	}}, time.Now())
	if err != nil {
		t.Fatalf("SendUsageReports: %v", err)
	}
	if !sawUsage {
		t.Fatal("expected usage report request")
	}
}

func assertRegistryLicenseHeaders(t *testing.T, request *http.Request, licenseKey string) {
	t.Helper()
	if request.Header.Get("Authorization") != "Bearer "+licenseKey || request.Header.Get("X-API-Key") != licenseKey {
		t.Fatalf("Registry auth headers = %q / %q", request.Header.Get("Authorization"), request.Header.Get("X-API-Key"))
	}
}

func testHeartbeatSignature(key string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
