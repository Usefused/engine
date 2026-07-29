package api

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestRESTProxy_POST_EmitsOTELSpan(t *testing.T) {
	exporter := setupTestTracer(t)
	s := &mockKeyStore{accountID: uuid.New()}
	fwd := &mockForwarder{}
	handler := RESTProxyHandler(fwd, s)

	req := httptest.NewRequest(http.MethodPost, "/integrations", bytes.NewBufferString(`{"name":"stripe"}`))
	req.Header.Set("X-API-Key", "fsk_valid")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span for a POST, got %d", len(spans))
	}
	if spans[0].Name != "engine.proxy.rest_mutation" {
		t.Errorf("unexpected span name: %s", spans[0].Name)
	}
	if !fwd.called {
		t.Error("expected Forward to still be called for a POST")
	}
}

func TestRESTProxy_GET_NoSpan(t *testing.T) {
	exporter := setupTestTracer(t)
	s := &mockKeyStore{accountID: uuid.New()}
	fwd := &mockForwarder{}
	handler := RESTProxyHandler(fwd, s)

	req := httptest.NewRequest(http.MethodGet, "/integrations/svc-123", nil)
	req.Header.Set("X-API-Key", "fsk_valid")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	spans := exporter.GetSpans()
	if len(spans) != 0 {
		t.Errorf("expected no span for a GET, got %d", len(spans))
	}
	if !fwd.called {
		t.Error("expected Forward to still be called for a GET")
	}
}

func TestRESTProxy_InvalidKey_Returns401(t *testing.T) {
	s := &mockKeyStore{err: errors.New("api key not found")}
	fwd := &mockForwarder{}
	handler := RESTProxyHandler(fwd, s)

	req := httptest.NewRequest(http.MethodPost, "/integrations", bytes.NewBufferString(`{}`))
	req.Header.Set("X-API-Key", "fsk_invalid")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if fwd.called {
		t.Error("Forward must not be called for an invalid key")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

// TestRESTProxy_MountPaths_DoNotIncludeWorkspace guards the plane boundary:
// RESTProxyMountPaths is the single source of truth main.go uses to wire
// Registry-proxied routes. /workspace must never appear in it -- that data
// lives in the Engine's own DB (see workspace_handlers.go), so accidentally
// proxying it to the Registry would silently serve the wrong plane's data.
func TestRESTProxy_MountPaths_DoNotIncludeWorkspace(t *testing.T) {
	for _, path := range RESTProxyMountPaths {
		if path == "/workspace" {
			t.Fatalf("RESTProxyMountPaths must not include /workspace: %v", RESTProxyMountPaths)
		}
	}
}
