package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const maxUnifiedAppBundleBytes = 2 << 20

var (
	ErrUnifiedAppBundleNotFound       = errors.New("unified app bundle not found")
	ErrUnifiedAppBundleImmutable      = errors.New("unified app bundle is immutable")
	ErrUnifiedAppBundleDigestMismatch = errors.New("unified app bundle digest does not match planned version")
)

var canonicalUnifiedAppBundleDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// IsCanonicalUnifiedAppBundleDigest admits only the exact hash syntax shared by compiler and Engine.
func IsCanonicalUnifiedAppBundleDigest(digest string) bool {
	return canonicalUnifiedAppBundleDigest.MatchString(digest)
}

// UnifiedAppBundleDigest identifies exact compiled bytes, independent of caller source labels or manifest claims.
func UnifiedAppBundleDigest(script []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(script))
}

// UnifiedAppBundle belongs to one exact immutable app version, identified by AppID.
type UnifiedAppBundle struct {
	AppID      uuid.UUID
	SourceHash string
	BundleJS   string
	Manifest   json.RawMessage
	CreatedAt  time.Time
}

// UnifiedAppBundleStore is the narrow persistence contract for hosted capability bundles.
type UnifiedAppBundleStore interface {
	CreateUnifiedAppBundle(context.Context, UnifiedAppBundle) error
	GetUnifiedAppBundle(context.Context, uuid.UUID) (*UnifiedAppBundle, error)
}

// CreateUnifiedAppBundle writes one exact app bundle and treats identical retries as idempotent.
func (s *postgresStore) CreateUnifiedAppBundle(ctx context.Context, bundle UnifiedAppBundle) error {
	// Invalid or oversized bundles must fail before the database allocates immutable storage.
	if err := validateUnifiedAppBundle(bundle); err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	// Bundle storage and traffic promotion must commit together.
	if err != nil {
		return fmt.Errorf("create unified app bundle: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	var familyID uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT family.app_family_id
		FROM fused_apps app
		JOIN fused_app_families family ON family.app_family_id = app.app_family_id
		WHERE app.app_id = $1 AND family.kind = 'unified_app' AND family.archived_at IS NULL
		FOR UPDATE OF family
	`, bundle.AppID).Scan(&familyID)
	// Only an existing Unified App family can receive an authored bundle.
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnifiedAppBundleNotFound
	}
	// Other lookup failures cannot be treated as a missing family.
	if err != nil {
		return fmt.Errorf("create unified app bundle: lock family: %w", err)
	}
	if err := createUnifiedAppBundleTx(ctx, tx, bundle); err != nil {
		return err
	}
	if err := promoteUnifiedAppVersionTx(ctx, tx, familyID, bundle.AppID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// createUnifiedAppBundleTx preserves immutable bytes while the caller owns promotion's family lock.
func createUnifiedAppBundleTx(ctx context.Context, tx pgx.Tx, bundle UnifiedAppBundle) error {
	digest := UnifiedAppBundleDigest([]byte(bundle.BundleJS))
	var inserted uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO fused_unified_app_bundles (app_id, source_hash, bundle_js, manifest)
		SELECT app_id, $2, $3, $4::jsonb
		FROM fused_apps
		WHERE app_id = $1 AND source_hash = $2 AND bundle_digest = $5 AND status = 'active'
		ON CONFLICT (app_id) DO NOTHING
		RETURNING app_id
	`, bundle.AppID, bundle.SourceHash, bundle.BundleJS, bundle.Manifest, digest).Scan(&inserted)
	// The app row predicate keeps active status and source identity authoritative at write time.
	if err == nil {
		return nil
	}
	// Only a unique-key conflict permits an idempotency comparison; SQL failures remain failures.
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("create unified app bundle: %w", err)
	}
	var identical bool
	err = tx.QueryRow(ctx, `
		SELECT artifact.source_hash = $2 AND artifact.bundle_js = $3 AND artifact.manifest = $4::jsonb AND app.bundle_digest = $5
		FROM fused_unified_app_bundles artifact
		JOIN fused_apps app ON app.app_id = artifact.app_id
		WHERE artifact.app_id = $1 AND app.status = 'active'
	`, bundle.AppID, bundle.SourceHash, bundle.BundleJS, bundle.Manifest, digest).Scan(&identical)
	// A concurrently removed exact version cannot be treated as a successful retry.
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnifiedAppBundleNotFound
	}
	// Database comparison errors must not be misreported as immutable conflicts.
	if err != nil {
		return fmt.Errorf("compare unified app bundle: %w", err)
	}
	// A source, compiled bundle, or manifest change requires a new app version.
	if !identical {
		return ErrUnifiedAppBundleImmutable
	}
	return nil
}

// promoteUnifiedAppVersionTx selects one ready immutable version for all new calls in its family.
func promoteUnifiedAppVersionTx(ctx context.Context, tx pgx.Tx, familyID, appID uuid.UUID) error {
	result, err := tx.Exec(ctx, `
		UPDATE fused_app_families family
		SET unified_active_app_id = app.app_id,
		    unified_target_initialized = true,
		    mcp_stable_app_id = CASE WHEN app.hosted_mcp THEN app.app_id ELSE NULL END,
		    mcp_stable_route_initialized = true,
		    updated_at = CASE WHEN family.unified_active_app_id IS DISTINCT FROM app.app_id THEN NOW() ELSE family.updated_at END
		FROM fused_apps app
		JOIN fused_unified_app_bundles bundle ON bundle.app_id = app.app_id
		WHERE family.app_family_id = $1 AND family.kind = 'unified_app'
		  AND family.archived_at IS NULL AND app.app_id = $2
		  AND app.app_family_id = family.app_family_id
		  AND app.status IN ('active', 'deprecated')
	`, familyID, appID)
	// Failed readiness checks must roll back attachment instead of disabling the previous target.
	if err != nil {
		return fmt.Errorf("promote unified app version: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrUnifiedAppBundleNotFound
	}
	// Sessions pinned to a prior version cannot continue using its raw MCP tools.
	if _, err := tx.Exec(ctx, `
		UPDATE fused_mcp_sessions session
		SET ended_at = COALESCE(session.ended_at, NOW())
		FROM fused_apps app
		WHERE app.app_id = session.app_id AND app.app_family_id = $1
		  AND app.app_id <> $2 AND session.ended_at IS NULL
	`, familyID, appID); err != nil {
		return fmt.Errorf("retire previous unified app sessions: %w", err)
	}
	return nil
}

// GetUnifiedAppBundle loads the authored bundle for exactly one app version ID.
func (s *postgresStore) GetUnifiedAppBundle(ctx context.Context, appID uuid.UUID) (*UnifiedAppBundle, error) {
	// Empty identities cannot select a retained app version.
	if appID == uuid.Nil {
		return nil, ErrUnifiedAppBundleNotFound
	}
	var bundle UnifiedAppBundle
	var pinnedDigest string
	err := s.db.QueryRow(ctx, `
		SELECT artifact.app_id, artifact.source_hash, artifact.bundle_js, artifact.manifest, artifact.created_at, app.bundle_digest
		FROM fused_unified_app_bundles artifact
		JOIN fused_apps app ON app.app_id = artifact.app_id
		WHERE artifact.app_id = $1 AND app.bundle_digest IS NOT NULL
	`, appID).Scan(&bundle.AppID, &bundle.SourceHash, &bundle.BundleJS, &bundle.Manifest, &bundle.CreatedAt, &pinnedDigest)
	// Absence is part of the bundle lookup contract, including removed app versions.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnifiedAppBundleNotFound
	}
	// Persistence failures must remain distinguishable from an absent version.
	if err != nil {
		return nil, fmt.Errorf("get unified app bundle: %w", err)
	}
	// Corrupted stored bytes must not execute even if the version and source label still match.
	if UnifiedAppBundleDigest([]byte(bundle.BundleJS)) != pinnedDigest {
		return nil, ErrUnifiedAppBundleDigestMismatch
	}
	return &bundle, nil
}

// validateUnifiedAppBundle rejects malformed immutable material before persistence.
func validateUnifiedAppBundle(bundle UnifiedAppBundle) error {
	// Every bundle must be attached to one exact app version and source identity.
	if bundle.AppID == uuid.Nil || strings.TrimSpace(bundle.SourceHash) == "" {
		return errors.New("unified app bundle identity is incomplete")
	}
	// The compiled script has the same fixed size limit as its database column.
	if len(bundle.BundleJS) == 0 || len(bundle.BundleJS) > maxUnifiedAppBundleBytes {
		return errors.New("unified app bundle exceeds the allowed size or is empty")
	}
	var manifest map[string]json.RawMessage
	// A manifest is an object so runtime admission can inspect its capability declarations.
	if err := json.Unmarshal(bundle.Manifest, &manifest); err != nil || manifest == nil {
		return errors.New("unified app bundle manifest must be a JSON object")
	}
	return nil
}

// CreateUnifiedAppBundle preserves narrow bundle access through the cached store wrapper.
func (s *cachedStore) CreateUnifiedAppBundle(ctx context.Context, bundle UnifiedAppBundle) error {
	repository, ok := s.Store.(UnifiedAppBundleStore)
	// A wrapper without bundle persistence cannot silently discard an immutable app artifact.
	if !ok {
		return errors.New("unified app bundle store unavailable")
	}
	return repository.CreateUnifiedAppBundle(ctx, bundle)
}

// GetUnifiedAppBundle delegates exact version lookup without caching mutable script bytes.
func (s *cachedStore) GetUnifiedAppBundle(ctx context.Context, appID uuid.UUID) (*UnifiedAppBundle, error) {
	repository, ok := s.Store.(UnifiedAppBundleStore)
	// A missing delegate must not be mistaken for an absent app version.
	if !ok {
		return nil, errors.New("unified app bundle store unavailable")
	}
	return repository.GetUnifiedAppBundle(ctx, appID)
}

var _ UnifiedAppBundleStore = (*postgresStore)(nil)
var _ UnifiedAppBundleStore = (*cachedStore)(nil)
