package sandbox

import (
	"context"
	"testing"

	"github.com/Usefused/engine/internal/engine"
)

func TestContextWithMCPIdempotencyIdentity_SetsKeyAndPresence(t *testing.T) {
	ctx := contextWithMCPIdempotencyIdentity(context.Background(), map[string]any{"a": 1})

	if idempotencyKeyFromContext(ctx) == "" {
		t.Error("expected a generated idempotency key in context")
	}
	if requestBodyHashFromContext(ctx) == "" {
		t.Error("expected a request body hash in context")
	}
	if !engine.IdempotencyKeyPresentInContext(ctx) {
		t.Error("expected IdempotencyKeyPresentInContext to be true, enabling POST/PATCH retry safety")
	}
}

func TestContextWithMCPIdempotencyIdentity_FreshKeyPerCall(t *testing.T) {
	ctx1 := contextWithMCPIdempotencyIdentity(context.Background(), map[string]any{"a": 1})
	ctx2 := contextWithMCPIdempotencyIdentity(context.Background(), map[string]any{"a": 1})

	if idempotencyKeyFromContext(ctx1) == idempotencyKeyFromContext(ctx2) {
		t.Error("expected two separate tool calls to get different idempotency keys")
	}
}

func TestHashRequestBody_DeterministicRegardlessOfMapOrder(t *testing.T) {
	h1 := hashRequestBody(map[string]any{"a": 1, "b": "two", "c": true})
	h2 := hashRequestBody(map[string]any{"c": true, "a": 1, "b": "two"})

	if h1 == "" {
		t.Fatal("expected a non-empty hash")
	}
	if h1 != h2 {
		t.Errorf("expected the same hash regardless of map construction order: %q vs %q", h1, h2)
	}

	h3 := hashRequestBody(map[string]any{"a": 1, "b": "two", "c": false})
	if h1 == h3 {
		t.Error("expected different params to hash differently")
	}
}
