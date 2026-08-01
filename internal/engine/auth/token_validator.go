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
	ErrUnauthorized = errors.New("unauthorized: invalid SDK token")
)

type TokenValidator interface {
	// Validate resolves a token string to an AccountID, given the SDK ID context.
	// Control-plane credentials do not cross this runtime boundary; only a token
	// issued for the selected artifact is accepted.
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
