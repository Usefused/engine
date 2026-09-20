// Package managedauthbroker implements the Fused-hosted managed-auth broker
// side of installation enrollment: a *remote* customer Engine, already
// vouched for by Registry via a one-time enrollment ticket, exchanges that
// ticket here for a scoped, rotatable installation credential. This is
// deliberately separate from oauthprovider, which delegates one Fused
// user's own permissions to a third-party client -- there is no Fused user
// or browser session in this flow, only a remote Engine installation.
package managedauthbroker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrTokenInvalid covers an unknown, expired, or revoked credential.
var ErrTokenInvalid = errors.New("managed-auth installation credential is invalid, expired, or revoked")

type Store struct {
	database *pgxpool.Pool
}

// NewStore shares the broker Engine database without a separate identity store.
func NewStore(database *pgxpool.Pool) *Store { return &Store{database: database} }

// Installation is one issued credential pair's metadata.
type Installation struct {
	ID                   uuid.UUID
	RegistryAccountID    uuid.UUID
	EngineInstallationID uuid.UUID
	TokenFamilyID        uuid.UUID
	Scope                []string
	AccessExpiresAt      time.Time
	RefreshExpiresAt     time.Time
}

// IssueInstallation atomically replaces only this verified installation's credential.
// The unique active-installation index serializes concurrent enrollments, including first issuance.
func (s *Store) IssueInstallation(ctx context.Context, identity EnrollmentIdentity, scope []string, accessHash, refreshHash string, familyID uuid.UUID, accessExpiresAt, refreshExpiresAt time.Time) error {
	// No account-wide or anonymous credentials may be introduced by internal callers.
	if identity.AccountID == uuid.Nil || identity.InstallationID == uuid.Nil {
		return ErrInvalidTicket
	}
	_, err := s.database.Exec(ctx, `
 INSERT INTO fused_managed_auth_installations
 (registry_account_id, engine_installation_id, token_family_id, access_token_hash, refresh_token_hash, scope, access_expires_at, refresh_expires_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
 ON CONFLICT (registry_account_id, engine_installation_id) WHERE revoked_at IS NULL
 DO UPDATE SET token_family_id = EXCLUDED.token_family_id,
 access_token_hash = EXCLUDED.access_token_hash, refresh_token_hash = EXCLUDED.refresh_token_hash,
 scope = EXCLUDED.scope, access_expires_at = EXCLUDED.access_expires_at, refresh_expires_at = EXCLUDED.refresh_expires_at`,
		identity.AccountID, identity.InstallationID, familyID, accessHash, refreshHash, scope, accessExpiresAt, refreshExpiresAt)
	// Statement rollback preserves the prior credential if replacement fails.
	if err != nil {
		return fmt.Errorf("issue managed-auth installation: %w", err)
	}
	return nil
}

// RotateRefreshToken replaces both hashes in one compare-and-swap statement.
// Failure leaves the old pair valid; concurrent refreshes cannot both consume the same hash.
func (s *Store) RotateRefreshToken(ctx context.Context, priorHash, accessHash, refreshHash string, identity EnrollmentIdentity, accessExpiresAt, refreshExpiresAt time.Time) error {
	var id uuid.UUID
	err := s.database.QueryRow(ctx, `
 UPDATE fused_managed_auth_installations
 SET access_token_hash = $2, refresh_token_hash = $3, access_expires_at = $4, refresh_expires_at = $5
 WHERE refresh_token_hash = $1 AND revoked_at IS NULL AND refresh_expires_at > NOW()
 AND registry_account_id = $6 AND engine_installation_id = $7
 RETURNING id`, priorHash, accessHash, refreshHash, accessExpiresAt, refreshExpiresAt, identity.AccountID, identity.InstallationID).Scan(&id)
	// A consumed or expired hash cannot revoke or replace a newer credential.
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrTokenInvalid
	}
	// Persistence errors are transient failures, not evidence of an invalid bearer.
	if err != nil {
		return fmt.Errorf("rotate managed-auth installation: %w", err)
	}
	return nil
}

// GetByAccessToken looks up the live installation for an access-token hash,
// used to resolve an authenticated broker caller on each request.
func (s *Store) GetByAccessToken(ctx context.Context, accessHash string) (Installation, error) {
	var row Installation
	err := s.database.QueryRow(ctx, `
		SELECT id, registry_account_id, engine_installation_id, token_family_id, scope, access_expires_at, refresh_expires_at
		FROM fused_managed_auth_installations
		WHERE access_token_hash = $1 AND revoked_at IS NULL AND access_expires_at > NOW() AND engine_installation_id IS NOT NULL`,
		accessHash,
	).Scan(&row.ID, &row.RegistryAccountID, &row.EngineInstallationID, &row.TokenFamilyID, &row.Scope, &row.AccessExpiresAt, &row.RefreshExpiresAt)
	// Only a complete active installation may authorize broker operations.
	if err != nil {
		// Do not distinguish an unknown token from an expired or revoked grant.
		if errors.Is(err, pgx.ErrNoRows) {
			return Installation{}, ErrTokenInvalid
		}
		return Installation{}, fmt.Errorf("look up managed-auth installation: %w", err)
	}
	return row, nil
}

// Revoke accepts expired credentials too and never affects a sibling installation or discloses token existence.
func (s *Store) Revoke(ctx context.Context, refreshHash string) error {
	_, err := s.database.Exec(ctx, `UPDATE fused_managed_auth_installations SET revoked_at = COALESCE(revoked_at, NOW()) WHERE refresh_token_hash = $1`, refreshHash)
	return err
}
