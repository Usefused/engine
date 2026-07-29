package engine

import (
	"net/http"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
)

func TestMethodRequiresIdempotencyKeyForRetry(t *testing.T) {
	nonIdempotent := []string{http.MethodPost, http.MethodPatch}
	for _, m := range nonIdempotent {
		if !methodRequiresIdempotencyKeyForRetry(m) {
			t.Errorf("%s: expected to require an idempotency key for retry", m)
		}
	}

	idempotent := []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodOptions}
	for _, m := range idempotent {
		if methodRequiresIdempotencyKeyForRetry(m) {
			t.Errorf("%s: expected to be safe to retry without an idempotency key", m)
		}
	}
}

func TestResolveRetryPolicy_NoConfigNoOverride(t *testing.T) {
	if got := resolveRetryPolicy(nil, nil); got != nil {
		t.Fatalf("expected nil policy (no retry), got %+v", got)
	}
}

func TestResolveRetryPolicy_ServiceConfigOnly(t *testing.T) {
	service := &models.RetryConfig{Strategy: "exponential", MaxRetries: 4, BackoffMs: 200}
	got := resolveRetryPolicy(service, nil)
	if got != service {
		t.Fatalf("expected the service config unchanged, got %+v", got)
	}
}

func TestResolveRetryPolicy_OverrideOnly_ClampedToHardCeiling(t *testing.T) {
	max := 100
	backoff := 999999
	got := resolveRetryPolicy(nil, &RetryOverride{MaxRetries: &max, BackoffMs: &backoff})
	if got == nil {
		t.Fatal("expected a resolved policy")
	}
	if got.MaxRetries != sdkRetryCeilingMaxRetries {
		t.Errorf("MaxRetries: got %d, want %d (hard ceiling)", got.MaxRetries, sdkRetryCeilingMaxRetries)
	}
	if got.BackoffMs != sdkRetryCeilingBackoffMs {
		t.Errorf("BackoffMs: got %d, want %d (hard ceiling)", got.BackoffMs, sdkRetryCeilingBackoffMs)
	}
	if got.Strategy != defaultRetryStrategy {
		t.Errorf("Strategy: got %q, want %q (no service policy to take it from)", got.Strategy, defaultRetryStrategy)
	}
}

func TestResolveRetryPolicy_OverrideNarrowsServiceCeiling(t *testing.T) {
	service := &models.RetryConfig{Strategy: "exponential", MaxRetries: 5, BackoffMs: 1000}
	max := 2
	backoff := 100
	got := resolveRetryPolicy(service, &RetryOverride{MaxRetries: &max, BackoffMs: &backoff})
	if got.MaxRetries != 2 || got.BackoffMs != 100 {
		t.Errorf("expected the narrower override values, got MaxRetries=%d BackoffMs=%d", got.MaxRetries, got.BackoffMs)
	}
	if got.Strategy != "exponential" {
		t.Errorf("Strategy: got %q, want service's own %q (caller cannot override it)", got.Strategy, "exponential")
	}
}

func TestResolveRetryPolicy_OverrideCannotWidenServiceCeiling(t *testing.T) {
	service := &models.RetryConfig{Strategy: "fixed", MaxRetries: 1, BackoffMs: 50}
	max := 10
	backoff := 5000
	got := resolveRetryPolicy(service, &RetryOverride{MaxRetries: &max, BackoffMs: &backoff})
	if got.MaxRetries != 1 {
		t.Errorf("MaxRetries: got %d, want clamped to service's 1", got.MaxRetries)
	}
	if got.BackoffMs != 50 {
		t.Errorf("BackoffMs: got %d, want clamped to service's 50", got.BackoffMs)
	}
}

func TestResolveRetryPolicy_OverrideZeroMeansOptOut(t *testing.T) {
	service := &models.RetryConfig{Strategy: "fixed", MaxRetries: 5, BackoffMs: 100}
	zero := 0
	got := resolveRetryPolicy(service, &RetryOverride{MaxRetries: &zero})
	if got.MaxRetries != 0 {
		t.Errorf("expected MaxRetries=0 (single attempt), got %d", got.MaxRetries)
	}
}
