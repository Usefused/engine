package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

const upsertSecretSQL = `
	INSERT INTO fused_workspace_secrets
	(bucket_id, service_id, key_name, credential_type, encrypted_dek, encrypted_value, expires_at)
	SELECT b.id, $2, $3, $4, $5, $6, $7
	FROM fused_buckets b
	WHERE b.id = $1
	ON CONFLICT ON CONSTRAINT uq_workspace_secrets
	DO UPDATE SET
		credential_type = EXCLUDED.credential_type,
		encrypted_dek = EXCLUDED.encrypted_dek,
		encrypted_value = EXCLUDED.encrypted_value,
		expires_at = EXCLUDED.expires_at,
		updated_at = NOW()
`

func (s *postgresStore) UpsertSecret(ctx context.Context, secret WorkspaceSecret) error {
	return execUpsertSecret(ctx, s.db, secret)
}

// UpsertSecrets commits a whole credential family in one transaction so a
// failed cert/key or username/password update cannot leave runtime auth split
// between old and new material.
func (s *postgresStore) UpsertSecrets(ctx context.Context, secrets []WorkspaceSecret) error {
	if len(secrets) == 0 {
		return nil
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, secret := range secrets {
		if err := execUpsertSecret(ctx, tx, secret); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// DeleteSecrets applies one dependency decision to the whole requested family.
func (s *postgresStore) DeleteSecrets(ctx context.Context, bucketID uuid.UUID, serviceID uuid.UUID, keyNames []string) error {
	return s.deleteWorkspaceSecrets(ctx, bucketID, serviceID, keyNames)
}

// DeleteSecret shares the family-aware dependency guard used by batch deletes
// so single-key admin actions cannot orphan a live reference.
func (s *postgresStore) DeleteSecret(ctx context.Context, bucketID uuid.UUID, serviceID uuid.UUID, keyName string) error {
	return s.deleteWorkspaceSecrets(ctx, bucketID, serviceID, []string{keyName})
}

const workspaceAuthReferenceDependencySQL = `
	SELECT EXISTS (
		SELECT 1
		FROM fused_workspace_auth_references reference
		WHERE reference.bucket_id = $1
		  AND reference.source_service_id = $2
		  AND (
			reference.source_auth_name = ANY($3)
			OR reference.source_auth_type = 'basic' AND (
				reference.source_auth_name || '_username' = ANY($3)
				OR reference.source_auth_name || '_password' = ANY($3)
			)
			OR reference.source_auth_type = 'mtls' AND (
				reference.source_auth_name || '_cert' = ANY($3)
				OR reference.source_auth_name || '_key' = ANY($3)
			)
		  )
	)`

// deleteWorkspaceSecrets serializes deletion with reference apply and blocks
// removal of any credential family that still has dependents.
func (s *postgresStore) deleteWorkspaceSecrets(ctx context.Context, bucketID, serviceID uuid.UUID, keyNames []string) error {
	keyNames = uniqueSecretKeyNames(keyNames)
	// Empty admin selections are a no-op and should not acquire a bucket lock.
	if len(keyNames) == 0 {
		return nil
	}
	tx, err := s.db.Begin(ctx)
	// Without a transaction, the reference check and deletion could observe
	// different dependency states.
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Reference apply takes the same bucket lock, closing the check/delete race
	// without one lock or dependency query per credential key.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, bucketID); err != nil {
		return err
	}
	var referenced bool
	// The set query evaluates all requested keys together rather than checking
	// one secret row at a time.
	if err := tx.QueryRow(ctx, workspaceAuthReferenceDependencySQL, bucketID, serviceID, keyNames).Scan(&referenced); err != nil {
		return err
	}
	// A dependent resolves dynamically on every dispatch; deleting its source
	// would turn an intentional rotation path into a delayed provider failure.
	if referenced {
		return ErrWorkspaceAuthReferenceInUse
	}
	// A single exact-set delete preserves the no-N+1 boundary for paired auth.
	if _, err := tx.Exec(ctx, `DELETE FROM fused_workspace_secrets WHERE bucket_id = $1 AND service_id = $2 AND key_name = ANY($3)`, bucketID, serviceID, keyNames); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type secretExec interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// execUpsertSecret keeps the single-row statement shared between direct and
// transactional callers so conflict behavior stays identical across paths.
func execUpsertSecret(ctx context.Context, exec secretExec, secret WorkspaceSecret) error {
	// Bucket ownership is authoritative. Callers do not get to supply a
	// duplicate workspace identity that could disagree with the singleton Engine.
	tag, err := exec.Exec(ctx, upsertSecretSQL, secret.BucketID, secret.ServiceID, secret.KeyName, secret.CredentialType, secret.EncryptedDEK, secret.EncryptedValue, secret.ExpiresAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrBucketNotFound, secret.BucketID)
	}
	return nil
}
