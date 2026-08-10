package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

var (
	ErrUnauthorized = errors.New("unauthorized: invalid app token")
)

type TokenValidator interface {
	// Validate resolves the immutable app and its family from one token lookup.
	// Control-plane credentials do not cross this runtime boundary; only a token
	// issued for the selected app's family is accepted.
	Validate(ctx context.Context, appID uuid.UUID, token string) (RuntimeIdentity, error)
}

// RuntimeIdentity carries only safe identity metadata from authorization into
// execution receipts. Bucket, selection, and token data remain inside the
// authorization/store boundary.
type RuntimeIdentity struct {
	AccountID   uuid.UUID
	AppFamilyID uuid.UUID
	AppID       uuid.UUID
	AppVersion  string
	Kind        store.AppKind
	Status      store.AppStatus
	TokenPolicy store.AppTokenPolicy
}

type tokenValidator struct {
	store store.Store
}

func NewTokenValidator(s store.Store) TokenValidator {
	return &tokenValidator{store: s}
}

func (v *tokenValidator) Validate(ctx context.Context, appID uuid.UUID, token string) (RuntimeIdentity, error) {
	if token == "" {
		return RuntimeIdentity{}, ErrUnauthorized
	}

	tokenHash := HashToken(strings.TrimSpace(token))
	projection, err := v.store.AuthorizeApp(ctx, appID, tokenHash)
	if err != nil {
		return RuntimeIdentity{}, ErrUnauthorized
	}
	return RuntimeIdentity{
		AccountID: projection.AccountID, AppFamilyID: projection.AppFamilyID,
		AppID: projection.AppID, AppVersion: projection.Version,
		Kind: projection.Kind, Status: projection.AppStatus, TokenPolicy: projection.TokenPolicy,
	}, nil
}

// AllowsOperation applies the token's deny-all-except rule. App-version scope
// is checked separately, so this helper cannot grant an unselected operation.
func (identity RuntimeIdentity) AllowsOperation(operation string) bool {
	return identity.TokenPolicy.AllowsOperation(operation)
}

func HashToken(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}
