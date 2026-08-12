package engine

import (
	"net/http"
	"testing"
)

func TestMethodRequiresIdempotencyKeyForRetry(t *testing.T) {
	nonIdempotent := []string{http.MethodPost, http.MethodPatch, http.MethodConnect, "COPY"}
	for _, m := range nonIdempotent {
		if !methodRequiresIdempotencyKeyForRetry(m) {
			t.Errorf("%s: expected to require an idempotency key for retry", m)
		}
	}

	idempotent := []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodOptions, http.MethodTrace, "QUERY"}
	for _, m := range idempotent {
		if methodRequiresIdempotencyKeyForRetry(m) {
			t.Errorf("%s: expected to be safe to retry without an idempotency key", m)
		}
	}
}
