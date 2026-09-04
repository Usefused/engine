package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
)

// TestWriteRefreshServiceContractErrorUsesSharedEnvelope verifies every refresh
// class is correlated, stable, and hides internal or downstream failure prose.
func TestWriteRefreshServiceContractErrorUsesSharedEnvelope(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		status      int
		code        string
		message     string
		retryable   bool
		phase       string
		commitState string
	}{
		{name: "invalid service", err: refreshHTTPError{status: http.StatusBadRequest, message: "service id must be a valid UUID"}, status: http.StatusBadRequest, code: "invalid_service_id", message: "service id must be a valid UUID", phase: "runtime_contract_refresh", commitState: "not_committed"},
		{name: "invalid service version", err: refreshHTTPError{status: http.StatusBadRequest, message: "service_version_id must be a valid UUID"}, status: http.StatusBadRequest, code: "invalid_service_version_id", message: "service_version_id must be a valid UUID"},
		{name: "unknown validation", err: refreshHTTPError{status: http.StatusBadRequest, message: "invalid value secret=fsk_never_return"}, status: http.StatusBadRequest, code: "invalid_runtime_contract_refresh_request", message: "The runtime contract refresh request is invalid."},
		{name: "inactive version", err: refreshHTTPError{status: http.StatusNotFound, message: "workspace service version is not active"}, status: http.StatusNotFound, code: "runtime_contract_not_active", message: "The selected workspace service version is not active."},
		{name: "registry unavailable", err: refreshHTTPError{status: http.StatusBadGateway, message: "registry https://private.registry.test secret=fsk_never_return"}, status: http.StatusBadGateway, code: "runtime_contract_dependency_unavailable", message: "The Engine could not fetch the runtime contract.", retryable: true},
		{name: "rejected contract", err: refreshHTTPError{status: http.StatusUnprocessableEntity, rejectedVersion: uuid.New()}, status: http.StatusUnprocessableEntity, code: "runtime_contract_rejected", message: "Registry rejected the runtime contract for this service version."},
		{name: "unknown internal", err: errors.New("database password=fsk_never_return"), status: http.StatusInternalServerError, code: "runtime_contract_refresh_failed", message: "The Engine could not refresh the runtime contract.", retryable: true},
		{name: "ambiguous store write", err: refreshHTTPError{status: http.StatusInternalServerError, message: "failed to store runtime contract snapshot", phase: "runtime_contract_refresh_commit", commitState: "unknown"}, status: http.StatusInternalServerError, code: "runtime_contract_refresh_failed", message: "The Engine could not refresh the runtime contract.", phase: "runtime_contract_refresh_commit", commitState: "unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// RequestID supplies the same correlation context installed by the Engine router.
			handler := chimiddleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeRefreshServiceContractError(w, r.Context(), test.err)
			}))
			request := httptest.NewRequest(http.MethodPost, "/workspace/services/id/versions/version/refresh", nil)
			request.Header.Set(chimiddleware.RequestIDHeader, "request-refresh")
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			var envelope workspaceConfigErrorResponse
			// The complete stable contract must agree on status, code, message, and retry policy.
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || response.Code != test.status || envelope.Error.Code != test.code || envelope.Error.Message != test.message || envelope.Error.Retryable != test.retryable {
				t.Fatalf("refresh envelope = %#v, status=%d, decode error=%v", envelope, response.Code, err)
			}
			// Refresh is a local snapshot mutation whose rejected paths prove no commit.
			phase, commitState := test.phase, test.commitState
			// Cases without an explicit override remain pre-write failures.
			if phase == "" {
				phase, commitState = "runtime_contract_refresh", "not_committed"
			}
			if envelope.Error.Phase != phase || envelope.Error.CommitState != commitState || envelope.Error.RequestID != "request-refresh" {
				t.Fatalf("refresh mutation metadata = %#v", envelope.Error)
			}
			// Internal URLs, credentials, and store prose remain outside the public response.
			if strings.Contains(response.Body.String(), "private.registry.test") || strings.Contains(response.Body.String(), "fsk_never_return") || strings.Contains(response.Body.String(), "database password") {
				t.Fatalf("refresh response leaked internal prose: %s", response.Body.String())
			}
		})
	}
}
