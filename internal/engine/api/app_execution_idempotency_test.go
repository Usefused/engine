package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
)

// TestRESTIdempotencyRejectionsExplainOutcome verifies the public HTTP envelope, not just an internal mapping.
func TestRESTIdempotencyRejectionsExplainOutcome(t *testing.T) {
	tests := []struct {
		name, code, explanation, guidance string
		cause                             error
	}{
		{"different request", "idempotency_conflict", "already used for a different request", "new key only when you intend", store.ErrIdempotencyKeyConflict},
		{"unresolved request", "idempotency_unresolved", "may still be running, or its provider outcome may be unknown", "Do not change the key", store.ErrIdempotencyInProgress},
	}
	for _, test := range tests {
		// Wrapped causes model the real cache and dispatch boundaries without exposing their private diagnostics.
		t.Run(test.name, func(t *testing.T) {
			runtime := &restRuntimeTestDouble{physicalFound: true, physicalErr: fmt.Errorf("private-dispatch-detail: %w", &sandbox.IdempotencyRejectionError{Cause: test.cause})}
			server, appID := newRESTPhysicalServer(runtime)
			response := performRESTExecution(t, server, appID, "fsk_test", `{"operation":"items.create","input":{"private-input":"secret-value"}}`, "private-business-key")
			assertRESTErrorCode(t, response, http.StatusConflict, test.code)
			var envelope struct {
				Error struct {
					Message string            `json:"message"`
					Details map[string]string `json:"details"`
				} `json:"error"`
			}
			// Malformed JSON must not pass a text-only response assertion.
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			// People need the cause, the provider effect, and a safe next action alongside the code.
			if !strings.Contains(envelope.Error.Message, test.explanation) || envelope.Error.Details["provider_execution"] != "not_started" || !strings.Contains(envelope.Error.Details["remediation"], test.guidance) {
				t.Fatalf("missing idempotency explanation: %s", response.Body.String())
			}
			// The persisted pending flag cannot prove whether the earlier worker is still alive.
			if test.cause == store.ErrIdempotencyInProgress && envelope.Error.Details["previous_outcome"] != "pending_or_unknown" {
				t.Fatalf("unresolved outcome misrepresented: %s", response.Body.String())
			}
			for _, private := range []string{"private-dispatch-detail", "private-business-key", "secret-value"} {
				// Public guidance is fixed Engine text, never a reflection of keys, input, or raw failures.
				if strings.Contains(response.Body.String(), private) {
					t.Fatalf("response leaked %q: %s", private, response.Body.String())
				}
			}
		})
	}
}

// TestRESTUnclassifiedReservationFailureDoesNotClaimNoDispatch protects post-provider save failures from false retry advice.
func TestRESTUnclassifiedReservationFailureDoesNotClaimNoDispatch(t *testing.T) {
	runtime := &restRuntimeTestDouble{physicalFound: true, physicalErr: fmt.Errorf("save after dispatch: %w", store.ErrIdempotencyInProgress)}
	server, appID := newRESTPhysicalServer(runtime)
	response := performRESTExecution(t, server, appID, "fsk_test", `{"operation":"items.create","input":{}}`, "key")
	assertRESTErrorCode(t, response, http.StatusBadGateway, "execution_failed")
	// The same store sentinel can occur after dispatch; only the typed replay gate proves no new call.
	if strings.Contains(response.Body.String(), "not_started") || strings.Contains(response.Body.String(), "Retry later") {
		t.Fatalf("post-dispatch failure received unsafe pre-dispatch guidance: %s", response.Body.String())
	}
}
