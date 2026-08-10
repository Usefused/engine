package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/models"
)

// TestDispatcher_Integration_VendorCall is a hermetic integration test: it drives
// the full dispatcher HTTP path (build request → apply auth → stream body) against
// a local httptest server that mimics httpbin's /get echo. It is deterministic and
// has no external network dependency (the previous version called live
// httpbin.org, which made the suite flaky when that service was down).
func TestDispatcher_Integration_VendorCall(t *testing.T) {
	// Local vendor that echoes the request path back, httpbin-style.
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"url":  r.Host + r.URL.String(),
			"auth": r.Header.Get("Authorization"),
		})
	}))
	defer vendor.Close()

	obj := &models.IntegrationObject{
		Path: "/get", Method: "GET",
		SecurityRequirements: authrouting.Requirements{{Schemes: []authrouting.Requirement{{Scheme: "bearerAuth"}}}},
	}
	params := map[string]any{}
	// Bearer credential to prove auth injection flows through the dispatcher.
	creds := map[string]any{"bearerAuth": "tok-123"}
	srv := &models.Service{
		BaseURL:     vendor.URL,
		AuthConfigs: models.AuthConfigs{{Name: "bearerAuth", Type: "http", Scheme: "bearer"}},
	}

	stream := NewBufferStream()
	status, err := NewDispatcher().ExecuteStream(context.Background(), srv, explicitAnonymousEndpoint(obj), params, creds, nil, stream)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if status != 200 {
		t.Errorf("expected status 200, got: %d", status)
	}

	var resp map[string]any
	if err := json.Unmarshal(stream.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse vendor response: %v", err)
	}
	if url, _ := resp["url"].(string); !strings.Contains(url, "/get") {
		t.Errorf("expected echoed url to contain /get, got: %v", resp["url"])
	}
	if auth, _ := resp["auth"].(string); auth != "Bearer tok-123" {
		t.Errorf("expected vendor to receive 'Bearer tok-123', got: %v", resp["auth"])
	}
}
