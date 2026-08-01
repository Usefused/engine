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
					"plan":                          "commercial",
					"heartbeat_required":            true,
					"usage_reporting":               "aggregate",
					"heartbeat_interval_seconds":    30,
					"heartbeat_stale_after_seconds": 90,
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
	if result.Entitlements != models.DefaultRuntimeEntitlement() {
		t.Fatalf("expected default entitlement, got %#v", result.Entitlements)
	}
}

func TestHTTPRegistryClient_SendHeartbeat(t *testing.T) {
	os.Setenv("FUSED_ENV", "development")
	defer os.Unsetenv("FUSED_ENV")

	var sawHeartbeat bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/engine/heartbeat" {
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
		var req EngineHeartbeatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode heartbeat: %v", err)
		}
		if req.EngineVersion != "1.2.3" || req.EngineBuildHash != "abc123" {
			t.Fatalf("unexpected heartbeat identity: %+v", req)
		}
		sawHeartbeat = true
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewHTTPRegistryClient(ts.URL+"/graphql", "valid_license_key")
	err := client.SendHeartbeat(context.Background(), "1.2.3", "abc123", time.Now())
	if err != nil {
		t.Fatalf("SendHeartbeat: %v", err)
	}
	if !sawHeartbeat {
		t.Fatal("expected heartbeat request")
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
