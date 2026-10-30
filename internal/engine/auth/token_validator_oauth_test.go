package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// TestOAuthRuntimeValidationDoesNotCacheRights proves revocation cannot be hidden by the ordinary token cache.
func TestOAuthRuntimeValidationDoesNotCacheRights(t *testing.T) {
	calls := 0
	validator := NewTokenValidator(&mockStore{authorizeAppFn: func(_ context.Context, appID uuid.UUID, _ string) (*store.AuthProjection, error) {
		calls++
		// The next database decision represents disconnect or removal of the user's delegation rights.
		if calls > 1 {
			return nil, errors.New("revoked")
		}
		return tokenProjection(appID, uuid.New(), nil), nil
	}})
	id := uuid.New()
	// The first request establishes a valid credential before rights are removed.
	if _, err := validator.Validate(t.Context(), id, "foat_test"); err != nil {
		t.Fatal(err)
	}
	// The next request must consult current authority instead of returning a cached identity.
	if _, err := validator.Validate(t.Context(), id, "foat_test"); !errors.Is(err, ErrUnauthorized) || calls != 2 {
		t.Fatalf("cached OAuth authority: calls=%d err=%v", calls, err)
	}
}
