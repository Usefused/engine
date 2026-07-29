package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/store"
)

var errMissingAPIKey = errors.New("missing X-API-Key header")

// validateAPIKey resolves a raw X-API-Key header to an account ID. It hashes
// the key the same way auth.TokenValidator's API-key fallback path does
// (internal/engine/auth/token_validator.go), so a key that authenticates one
// Engine endpoint authenticates this one too -- one hashing scheme, not two.
//
// This is intentionally not routed through auth.TokenValidator itself:
// TokenValidator.Validate is scoped to (artifactID, token) pairs for SDK/MCP
// execution auth, and the proxy handlers here have no artifactID in scope --
// they're gating raw account-level API keys before relaying to the Registry.
//
// Shared by graphql_proxy.go and rest_proxy.go so proxy auth validation
// lives in exactly one place, per this package's DRY standard.
func validateAPIKey(ctx context.Context, s store.Store, apiKey string) (uuid.UUID, error) {
	if apiKey == "" {
		return uuid.Nil, errMissingAPIKey
	}
	hash := sha256.Sum256([]byte(apiKey))
	hashed := hex.EncodeToString(hash[:])
	return s.GetAccountByAPIKey(ctx, hashed)
}
