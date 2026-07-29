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
	ErrUnauthorized = errors.New("unauthorized: invalid token or license key")
)

type TokenValidator interface {
	// Validate resolves a token string to an AccountID, given the SDK ID context.
	// The token can either be the specific SDK token, or an Account-level API key.
	Validate(ctx context.Context, artifactID uuid.UUID, token string) (uuid.UUID, error)
}

type tokenValidator struct {
	store store.Store
}

func NewTokenValidator(s store.Store) TokenValidator {
	return &tokenValidator{store: s}
}

func (v *tokenValidator) Validate(ctx context.Context, artifactID uuid.UUID, token string) (uuid.UUID, error) {
	if token == "" {
		return uuid.Nil, ErrUnauthorized
	}

	tokenHash := HashToken(strings.TrimSpace(token))
	accountID, err := v.store.ValidateToken(ctx, artifactID, tokenHash)
	if err != nil {
		return uuid.Nil, ErrUnauthorized
	}
	return accountID, nil
}

func HashToken(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}
