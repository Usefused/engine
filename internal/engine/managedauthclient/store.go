// Package managedauthclient is this Engine's own client side of managed
// auth: it enrolls with the (remote) Fused-hosted managed-auth broker,
// keeps that enrollment's credential fresh, and hands out its currently
// valid broker access token to the connect flow. It is the mirror image of
// managedauthbroker, which lives on the broker side of the same handshake.
package managedauthclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Usefused/engine/internal/engine/store"
)

// ErrNotEnrolled means this Engine has never completed enrollment (or its
// credential was cleared), distinct from a credential that merely expired.
var ErrNotEnrolled = errors.New("managed-auth broker enrollment does not exist")

type Store struct {
	database credentialDatabase
	pool     *pgxpool.Pool
}

// credentialDatabase lets reconciliation read and persist on the connection holding its transaction lock.
type credentialDatabase interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// NewStore uses the installation database for both encrypted credentials and replica coordination.
func NewStore(database *pgxpool.Pool) *Store { return &Store{database: database, pool: database} }

// reconcile serializes enrollment and refresh across processes sharing this Engine database.
// Its transaction-scoped lock is released on cancellation or process loss, including before the first credential exists.
func (s *Store) reconcile(ctx context.Context, work func(context.Context, *Store) error) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	// A failed transaction cannot safely coordinate a remote credential mutation.
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	// A single fixed lock coordinates this database's singleton enrollment; network work is deadline bounded.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(734861920146)`); err != nil {
		return err
	}
	// All reads and saves use this transaction, avoiding stale reads and pool starvation at size one.
	if err := work(ctx, &Store{database: tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Credential is this installation's decrypted broker access/refresh pair.
type Credential struct {
	AccessToken      string
	RefreshToken     string
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
}

// Save replaces the singleton credential row -- enrollment and every
// subsequent refresh call this, so there is always at most one live value.
func (s *Store) Save(ctx context.Context, accessToken, refreshToken string, accessExpiresAt, refreshExpiresAt time.Time, masterKey []byte) error {
	wrappedDEK, dek, err := store.WrapDEK(masterKey)
	if err != nil {
		return fmt.Errorf("wrap managed-auth broker credential: %w", err)
	}
	encryptedAccess, err := store.EncryptWithDEK(dek, accessToken)
	if err != nil {
		return fmt.Errorf("encrypt managed-auth broker access token: %w", err)
	}
	encryptedRefresh, err := store.EncryptWithDEK(dek, refreshToken)
	if err != nil {
		return fmt.Errorf("encrypt managed-auth broker refresh token: %w", err)
	}
	_, err = s.database.Exec(ctx, `
		INSERT INTO fused_managed_auth_broker_credential (id, encrypted_dek, encrypted_access_token, encrypted_refresh_token, access_expires_at, refresh_expires_at)
		VALUES (1, $1, $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE
		SET encrypted_dek = EXCLUDED.encrypted_dek, encrypted_access_token = EXCLUDED.encrypted_access_token,
			encrypted_refresh_token = EXCLUDED.encrypted_refresh_token, access_expires_at = EXCLUDED.access_expires_at,
			refresh_expires_at = EXCLUDED.refresh_expires_at, updated_at = NOW()`,
		wrappedDEK, encryptedAccess, encryptedRefresh, accessExpiresAt, refreshExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("save managed-auth broker credential: %w", err)
	}
	return nil
}

// Get decrypts and returns the current credential.
func (s *Store) Get(ctx context.Context, masterKey []byte) (Credential, error) {
	var wrappedDEK, encryptedAccess, encryptedRefresh string
	var cred Credential
	err := s.database.QueryRow(ctx, `
		SELECT encrypted_dek, encrypted_access_token, encrypted_refresh_token, access_expires_at, refresh_expires_at
		FROM fused_managed_auth_broker_credential WHERE id = 1`,
	).Scan(&wrappedDEK, &encryptedAccess, &encryptedRefresh, &cred.AccessExpiresAt, &cred.RefreshExpiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Credential{}, ErrNotEnrolled
		}
		return Credential{}, fmt.Errorf("load managed-auth broker credential: %w", err)
	}
	dek, err := store.UnwrapDEK(masterKey, wrappedDEK)
	if err != nil {
		return Credential{}, fmt.Errorf("unwrap managed-auth broker credential: %w", err)
	}
	if cred.AccessToken, err = store.DecryptWithDEK(dek, encryptedAccess); err != nil {
		return Credential{}, fmt.Errorf("decrypt managed-auth broker access token: %w", err)
	}
	if cred.RefreshToken, err = store.DecryptWithDEK(dek, encryptedRefresh); err != nil {
		return Credential{}, fmt.Errorf("decrypt managed-auth broker refresh token: %w", err)
	}
	return cred, nil
}

// State reports persisted intent and outstanding revocation work without decrypting credentials.
func (s *Store) State(ctx context.Context) (enabled, hasCredential bool, err error) {
	err = s.database.QueryRow(ctx, `SELECT
 COALESCE((SELECT enabled FROM fused_managed_auth_preferences WHERE id = 1), true),
 EXISTS(SELECT 1 FROM fused_managed_auth_broker_credential WHERE id = 1)`).Scan(&enabled, &hasCredential)
	return
}

// SetEnabled commits explicit user intent under the same lock used by enrollment and renewal.
func (s *Store) SetEnabled(ctx context.Context, enabled bool) error {
	return s.reconcile(ctx, func(ctx context.Context, locked *Store) error {
		prior, _, err := locked.State(ctx)
		// Failed state reads must not replace an unknown saved preference.
		if err != nil {
			return err
		}
		// Re-enabling after disable establishes fresh authority, including after a lost revoke acknowledgement.
		if enabled && !prior {
			if err := locked.clear(ctx); err != nil {
				return err
			}
		}
		_, err = locked.database.Exec(ctx, `INSERT INTO fused_managed_auth_preferences (id, enabled) VALUES (1,$1)
 ON CONFLICT (id) DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = NOW()`, enabled)
		return err
	})
}

// clear removes only this Engine's broker credential after revocation or an explicit re-enable transition.
func (s *Store) clear(ctx context.Context) error {
	_, err := s.database.Exec(ctx, `DELETE FROM fused_managed_auth_broker_credential WHERE id = 1`)
	return err
}
