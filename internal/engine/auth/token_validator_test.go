package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// mockStore only needs to fake ValidateToken -- that's the sole store method
// Validate() calls (token_validator.go:38). SDK-token-vs-account-API-key
// resolution, and the "API key belongs to a different account" check, both
// live inside the real ValidateToken's single SQL query now (see
// postgres_store.go's ValidateToken: one query joins fused_artifact_tokens and
// fused_api_keys, scoped by the account that owns artifactID) -- they're no
// longer separate steps orchestrated here, so the mock doesn't model them
// as separate calls either.
type mockStore struct {
	store.Store
	validateTokenFn func(ctx context.Context, artifactID uuid.UUID, tokenHash string) (uuid.UUID, error)
}

func (m *mockStore) ValidateToken(ctx context.Context, artifactID uuid.UUID, tokenHash string) (uuid.UUID, error) {
	if m.validateTokenFn != nil {
		return m.validateTokenFn(ctx, artifactID, tokenHash)
	}
	return uuid.Nil, errors.New("not implemented")
}

func TestTokenValidator(t *testing.T) {
	ctx := context.Background()
	artifactID := uuid.New()
	accountID := uuid.New()
	sdkToken := "fused_sdk_test_token"
	apiKey := "fsk_test_api_key"

	hashedSDKToken := HashToken(sdkToken)
	hashedAPIKey := HashToken(apiKey)

	tests := []struct {
		name          string
		token         string
		mock          *mockStore
		expectedErr   error
		expectedAccID uuid.UUID
	}{
		{
			// Real ValidateToken matches the SDK token's own hash against
			// fused_artifact_tokens for this artifactID.
			name:  "Valid SDK Token",
			token: sdkToken,
			mock: &mockStore{
				validateTokenFn: func(ctx context.Context, id uuid.UUID, hash string) (uuid.UUID, error) {
					if id == artifactID && hash == hashedSDKToken {
						return accountID, nil
					}
					return uuid.Nil, errors.New("not found")
				},
			},
			expectedErr:   nil,
			expectedAccID: accountID,
		},
		{
			// Real ValidateToken also matches an account-level API key hash,
			// scoped to whichever account owns artifactID.
			name:  "Valid API Key Fallback",
			token: apiKey,
			mock: &mockStore{
				validateTokenFn: func(ctx context.Context, id uuid.UUID, hash string) (uuid.UUID, error) {
					if id == artifactID && hash == hashedAPIKey {
						return accountID, nil
					}
					return uuid.Nil, errors.New("not found")
				},
			},
			expectedErr:   nil,
			expectedAccID: accountID,
		},
		{
			// An API key that resolves to a different account than artifactID
			// belongs to never matches the real query's account-scoped join
			// -- zero rows, same as any other unrecognized token.
			name:  "API Key Belongs To Different Account",
			token: apiKey,
			mock: &mockStore{
				validateTokenFn: func(ctx context.Context, id uuid.UUID, hash string) (uuid.UUID, error) {
					return uuid.Nil, errors.New("not found")
				},
			},
			expectedErr:   ErrUnauthorized,
			expectedAccID: uuid.Nil,
		},
		{
			name:  "Invalid Token",
			token: "invalid_nonsense",
			mock: &mockStore{
				validateTokenFn: func(ctx context.Context, id uuid.UUID, hash string) (uuid.UUID, error) {
					return uuid.Nil, errors.New("not found")
				},
			},
			expectedErr:   ErrUnauthorized,
			expectedAccID: uuid.Nil,
		},
		{
			// An artifactID with no matching fused_artifact_scopes row is indistinguishable
			// from any other non-match at the query level -- zero rows either way.
			name:  "SDK Not Found",
			token: sdkToken,
			mock: &mockStore{
				validateTokenFn: func(ctx context.Context, id uuid.UUID, hash string) (uuid.UUID, error) {
					return uuid.Nil, errors.New("not found")
				},
			},
			expectedErr:   ErrUnauthorized,
			expectedAccID: uuid.Nil,
		},
		{
			// Empty token is rejected by Validate() itself before the store
			// is ever called (token_validator.go:33-35), so the mock never
			// needs validateTokenFn wired up here.
			name:          "Empty Token",
			token:         "",
			mock:          &mockStore{},
			expectedErr:   ErrUnauthorized,
			expectedAccID: uuid.Nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := NewTokenValidator(tt.mock)
			gotAccID, err := v.Validate(ctx, artifactID, tt.token)
			if err != tt.expectedErr {
				t.Errorf("expected error %v, got %v", tt.expectedErr, err)
			}
			if gotAccID != tt.expectedAccID {
				t.Errorf("expected account id %v, got %v", tt.expectedAccID, gotAccID)
			}
		})
	}
}

func TestTokenValidatorResolvesSDKToken(t *testing.T) {
	ctx := context.Background()
	artifactID := uuid.New()
	accountID := uuid.New()
	rawToken := "fused_runtime_token"
	hashedToken := HashToken(rawToken)

	v := NewTokenValidator(&mockStore{
		validateTokenFn: func(ctx context.Context, id uuid.UUID, hash string) (uuid.UUID, error) {
			if id == artifactID && hash == hashedToken {
				return accountID, nil
			}
			return uuid.Nil, errors.New("not found")
		},
	})
	gotAccID, err := v.Validate(ctx, artifactID, rawToken)
	if err != nil {
		t.Fatalf("expected raw SDK token to validate against stored hash, got %v", err)
	}
	if gotAccID != accountID {
		t.Fatalf("expected account %s, got %s", accountID, gotAccID)
	}
}

// sanity check that HashToken (production hashing helper) matches a plain
// sha256 hex digest, so the two tests above using it as their oracle aren't
// silently trivially true.
func TestHashToken_MatchesPlainSHA256(t *testing.T) {
	sum := sha256.Sum256([]byte("some-token"))
	want := hex.EncodeToString(sum[:])
	if got := HashToken("some-token"); got != want {
		t.Fatalf("HashToken produced %q, want %q", got, want)
	}
}
