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

type mockStore struct {
	store.Store
	authorizeAppFn func(ctx context.Context, appID uuid.UUID, tokenHash string) (*store.AuthProjection, error)
}

func (m *mockStore) AuthorizeApp(ctx context.Context, appID uuid.UUID, tokenHash string) (*store.AuthProjection, error) {
	if m.authorizeAppFn != nil {
		return m.authorizeAppFn(ctx, appID, tokenHash)
	}
	return nil, errors.New("not implemented")
}

func TestTokenValidator(t *testing.T) {
	ctx := context.Background()
	appID := uuid.New()
	accountID := uuid.New()
	appFamilyID := uuid.New()
	appToken := "fused_app_test_token"

	hashedAppToken := HashToken(appToken)

	tests := []struct {
		name          string
		token         string
		mock          *mockStore
		expectedErr   error
		expectedAccID uuid.UUID
	}{
		{
			name:  "Valid App Token",
			token: appToken,
			mock: &mockStore{
				authorizeAppFn: func(ctx context.Context, id uuid.UUID, hash string) (*store.AuthProjection, error) {
					if id == appID && hash == hashedAppToken {
						return &store.AuthProjection{AccountID: accountID, AppFamilyID: appFamilyID, AppID: appID, Version: "1.0.0", Kind: "sdk", AppStatus: "active"}, nil
					}
					return nil, errors.New("not found")
				},
			},
			expectedErr:   nil,
			expectedAccID: accountID,
		},
		{
			name:  "Invalid Token",
			token: "invalid_nonsense",
			mock: &mockStore{
				authorizeAppFn: func(ctx context.Context, id uuid.UUID, hash string) (*store.AuthProjection, error) {
					return nil, errors.New("not found")
				},
			},
			expectedErr:   ErrUnauthorized,
			expectedAccID: uuid.Nil,
		},
		{
			name:  "App Not Found",
			token: appToken,
			mock: &mockStore{
				authorizeAppFn: func(ctx context.Context, id uuid.UUID, hash string) (*store.AuthProjection, error) {
					return nil, errors.New("not found")
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
			identity, err := v.Validate(ctx, appID, tt.token)
			if err != tt.expectedErr {
				t.Errorf("expected error %v, got %v", tt.expectedErr, err)
			}
			if identity.AccountID != tt.expectedAccID {
				t.Errorf("expected account id %v, got %v", tt.expectedAccID, identity.AccountID)
			}
		})
	}
}

func TestTokenValidatorResolvesAppToken(t *testing.T) {
	ctx := context.Background()
	appID := uuid.New()
	accountID := uuid.New()
	appFamilyID := uuid.New()
	rawToken := "fused_runtime_token"
	hashedToken := HashToken(rawToken)

	v := NewTokenValidator(&mockStore{
		authorizeAppFn: func(ctx context.Context, id uuid.UUID, hash string) (*store.AuthProjection, error) {
			if id == appID && hash == hashedToken {
				return &store.AuthProjection{AccountID: accountID, AppFamilyID: appFamilyID, AppID: appID, Version: "1.0.0", Kind: "sdk", AppStatus: "active"}, nil
			}
			return nil, errors.New("not found")
		},
	})
	identity, err := v.Validate(ctx, appID, rawToken)
	if err != nil {
		t.Fatalf("expected raw app token to validate against stored hash, got %v", err)
	}
	if identity.AccountID != accountID || identity.AppFamilyID != appFamilyID || identity.AppID != appID || identity.AppVersion != "1.0.0" {
		t.Fatalf("unexpected runtime identity: %#v", identity)
	}
}

func TestRuntimeIdentityAllowsOnlyTokenPolicyOperations(t *testing.T) {
	strict := RuntimeIdentity{TokenPolicy: store.AppTokenPolicy{AllowedOperations: []string{"users.get"}}}
	if !strict.AllowsOperation("users.get") {
		t.Fatal("strict token denied its allowed operation")
	}
	if strict.AllowsOperation("users.delete") {
		t.Fatal("strict token allowed an operation outside its policy")
	}

	full := RuntimeIdentity{TokenPolicy: store.AppTokenPolicy{AllowAll: true}}
	if !full.AllowsOperation("users.delete") {
		t.Fatal("full-access token denied an operation")
	}

	if (RuntimeIdentity{}).AllowsOperation("users.get") {
		t.Fatal("missing token policy must fail closed")
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
