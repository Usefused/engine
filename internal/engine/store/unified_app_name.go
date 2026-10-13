package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// UnifiedAppNameResolver resolves a name only within the family selected by a live execution token.
type UnifiedAppNameResolver interface {
	ResolveUnifiedAppName(context.Context, string, string) (uuid.UUID, error)
}

// ResolveUnifiedAppName uses the unique token hash to bound lookup before comparing the canonical app name.
func (s *postgresStore) ResolveUnifiedAppName(ctx context.Context, name, tokenHash string) (uuid.UUID, error) {
	var familyID uuid.UUID
	err := s.db.QueryRow(ctx, `
 SELECT f.app_family_id FROM fused_app_tokens t
 JOIN fused_app_families f ON f.app_family_id=t.app_family_id
 WHERE t.token_hash=$1 AND f.canonical_name=$2 AND f.kind='unified_app'
 AND f.archived_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>NOW())
 `, tokenHash, name).Scan(&familyID)
	// Unknown names, foreign tokens, expired tokens, and archived families have one indistinguishable outcome.
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrAppFamilyNotFound
	}
	return familyID, err
}

// ResolveUnifiedAppName bypasses cache so revocation, archive, and name reuse are observed on each lookup.
func (s *cachedStore) ResolveUnifiedAppName(ctx context.Context, name, tokenHash string) (uuid.UUID, error) {
	repository, ok := s.Store.(UnifiedAppNameResolver)
	// Missing persistence support cannot turn a name into an unscoped or guessed family.
	if !ok {
		return uuid.Nil, fmt.Errorf("Unified App name resolution unavailable")
	}
	return repository.ResolveUnifiedAppName(ctx, name, tokenHash)
}
