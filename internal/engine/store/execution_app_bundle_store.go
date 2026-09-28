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

const maxExecutionAppBundleBytes = 2 << 20

var (
	ErrExecutionAppBundleNotFound       = errors.New("execution app bundle not found")
	ErrExecutionAppBundleImmutable      = errors.New("execution app bundle is immutable")
	ErrExecutionAppBundleDigestMismatch = errors.New("execution app bundle digest does not match planned version")
)

var canonicalExecutionAppBundleDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// IsCanonicalExecutionAppBundleDigest admits only the exact hash syntax shared by compiler and Engine.
func IsCanonicalExecutionAppBundleDigest(digest string) bool {
	return canonicalExecutionAppBundleDigest.MatchString(digest)
}

// ExecutionAppBundleDigest identifies exact compiled bytes, independent of caller source labels or manifest claims.
func ExecutionAppBundleDigest(script []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(script))
}

// ExecutionAppBundle belongs to one exact immutable app version, identified by AppID.
type ExecutionAppBundle struct {
	AppID      uuid.UUID
	SourceHash string
	BundleJS   string
	Manifest   json.RawMessage
	CreatedAt  time.Time
}

// ExecutionAppBundleStore is the narrow persistence contract for hosted capability bundles.
type ExecutionAppBundleStore interface {
	CreateExecutionAppBundle(context.Context, ExecutionAppBundle) error
	GetExecutionAppBundle(context.Context, uuid.UUID) (*ExecutionAppBundle, error)
}

// CreateExecutionAppBundle writes one exact app bundle and treats identical retries as idempotent.
func (s *postgresStore) CreateExecutionAppBundle(ctx context.Context, bundle ExecutionAppBundle) error {
	// Invalid or oversized bundles must fail before the database allocates immutable storage.
	if err := validateExecutionAppBundle(bundle); err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	// Bundle storage and traffic promotion must commit together.
	if err != nil {
		return fmt.Errorf("create execution app bundle: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	var familyID uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT family.app_family_id
		FROM fused_apps app
		JOIN fused_app_families family ON family.app_family_id = app.app_family_id
		WHERE app.app_id = $1 AND family.kind = 'execution' AND family.archived_at IS NULL
		FOR UPDATE OF family
	`, bundle.AppID).Scan(&familyID)
	// Only an existing Execution App family can receive an authored bundle.
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrExecutionAppBundleNotFound
	}
	// Other lookup failures cannot be treated as a missing family.
	if err != nil {
		return fmt.Errorf("create execution app bundle: lock family: %w", err)
	}
	if err := createExecutionAppBundleTx(ctx, tx, bundle); err != nil {
		return err
	}
	if err := promoteExecutionAppVersionTx(ctx, tx, familyID, bundle.AppID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// createExecutionAppBundleTx preserves immutable bytes while the caller owns promotion's family lock.
func createExecutionAppBundleTx(ctx context.Context, tx pgx.Tx, bundle ExecutionAppBundle) error {
	digest := ExecutionAppBundleDigest([]byte(bundle.BundleJS))
	var inserted uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO fused_execution_app_bundles (app_id, source_hash, bundle_js, manifest)
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
		return fmt.Errorf("create execution app bundle: %w", err)
	}
	var identical bool
	err = tx.QueryRow(ctx, `
		SELECT artifact.source_hash = $2 AND artifact.bundle_js = $3 AND artifact.manifest = $4::jsonb AND app.bundle_digest = $5
		FROM fused_execution_app_bundles artifact
		JOIN fused_apps app ON app.app_id = artifact.app_id
		WHERE artifact.app_id = $1 AND app.status = 'active'
	`, bundle.AppID, bundle.SourceHash, bundle.BundleJS, bundle.Manifest, digest).Scan(&identical)
	// A concurrently removed exact version cannot be treated as a successful retry.
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrExecutionAppBundleNotFound
	}
	// Database comparison errors must not be misreported as immutable conflicts.
	if err != nil {
		return fmt.Errorf("compare execution app bundle: %w", err)
	}
	// A source, compiled bundle, or manifest change requires a new app version.
	if !identical {
		return ErrExecutionAppBundleImmutable
	}
	return nil
}

// promoteExecutionAppVersionTx selects one ready immutable version for all new calls in its family.
func promoteExecutionAppVersionTx(ctx context.Context, tx pgx.Tx, familyID, appID uuid.UUID) error {
	result, err := tx.Exec(ctx, `
		UPDATE fused_app_families family
		SET execution_active_app_id = app.app_id,
		    execution_target_initialized = true,
		    mcp_stable_app_id = CASE WHEN app.hosted_mcp THEN app.app_id ELSE NULL END,
		    mcp_stable_route_initialized = true,
		    updated_at = CASE WHEN family.execution_active_app_id IS DISTINCT FROM app.app_id THEN NOW() ELSE family.updated_at END
		FROM fused_apps app
		JOIN fused_execution_app_bundles bundle ON bundle.app_id = app.app_id
		WHERE family.app_family_id = $1 AND family.kind = 'execution'
		  AND family.archived_at IS NULL AND app.app_id = $2
		  AND app.app_family_id = family.app_family_id
		  AND app.status IN ('active', 'deprecated')
	`, familyID, appID)
	// Failed readiness checks must roll back attachment instead of disabling the previous target.
	if err != nil {
		return fmt.Errorf("promote execution app version: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrExecutionAppBundleNotFound
	}
	// Sessions pinned to a prior version cannot continue using its raw MCP tools.
	if _, err := tx.Exec(ctx, `
		UPDATE fused_mcp_sessions session
		SET ended_at = COALESCE(session.ended_at, NOW())
		FROM fused_apps app
		WHERE app.app_id = session.app_id AND app.app_family_id = $1
		  AND app.app_id <> $2 AND session.ended_at IS NULL
	`, familyID, appID); err != nil {
		return fmt.Errorf("retire previous execution app sessions: %w", err)
	}
	return nil
}

// GetExecutionAppBundle loads the authored bundle for exactly one app version ID.
func (s *postgresStore) GetExecutionAppBundle(ctx context.Context, appID uuid.UUID) (*ExecutionAppBundle, error) {
	// Empty identities cannot select a retained app version.
	if appID == uuid.Nil {
		return nil, ErrExecutionAppBundleNotFound
	}
	var bundle ExecutionAppBundle
	var pinnedDigest string
	err := s.db.QueryRow(ctx, `
		SELECT artifact.app_id, artifact.source_hash, artifact.bundle_js, artifact.manifest, artifact.created_at, app.bundle_digest
		FROM fused_execution_app_bundles artifact
		JOIN fused_apps app ON app.app_id = artifact.app_id
		WHERE artifact.app_id = $1 AND app.bundle_digest IS NOT NULL
	`, appID).Scan(&bundle.AppID, &bundle.SourceHash, &bundle.BundleJS, &bundle.Manifest, &bundle.CreatedAt, &pinnedDigest)
	// Absence is part of the bundle lookup contract, including removed app versions.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrExecutionAppBundleNotFound
	}
	// Persistence failures must remain distinguishable from an absent version.
	if err != nil {
		return nil, fmt.Errorf("get execution app bundle: %w", err)
	}
	// Corrupted stored bytes must not execute even if the version and source label still match.
	if ExecutionAppBundleDigest([]byte(bundle.BundleJS)) != pinnedDigest {
		return nil, ErrExecutionAppBundleDigestMismatch
	}
	return &bundle, nil
}

// validateExecutionAppBundle rejects malformed immutable material before persistence.
func validateExecutionAppBundle(bundle ExecutionAppBundle) error {
	// Every bundle must be attached to one exact app version and source identity.
	if bundle.AppID == uuid.Nil || strings.TrimSpace(bundle.SourceHash) == "" {
		return errors.New("execution app bundle identity is incomplete")
	}
	// The compiled script has the same fixed size limit as its database column.
	if len(bundle.BundleJS) == 0 || len(bundle.BundleJS) > maxExecutionAppBundleBytes {
		return errors.New("execution app bundle exceeds the allowed size or is empty")
	}
	var manifest map[string]json.RawMessage
	// A manifest is an object so runtime admission can inspect its capability declarations.
	if err := json.Unmarshal(bundle.Manifest, &manifest); err != nil || manifest == nil {
		return errors.New("execution app bundle manifest must be a JSON object")
	}
	return nil
}

// CreateExecutionAppBundle preserves narrow bundle access through the cached store wrapper.
func (s *cachedStore) CreateExecutionAppBundle(ctx context.Context, bundle ExecutionAppBundle) error {
	repository, ok := s.Store.(ExecutionAppBundleStore)
	// A wrapper without bundle persistence cannot silently discard an immutable app artifact.
	if !ok {
		return errors.New("execution app bundle store unavailable")
	}
	return repository.CreateExecutionAppBundle(ctx, bundle)
}

// GetExecutionAppBundle delegates exact version lookup without caching mutable script bytes.
func (s *cachedStore) GetExecutionAppBundle(ctx context.Context, appID uuid.UUID) (*ExecutionAppBundle, error) {
	repository, ok := s.Store.(ExecutionAppBundleStore)
	// A missing delegate must not be mistaken for an absent app version.
	if !ok {
		return nil, errors.New("execution app bundle store unavailable")
	}
	return repository.GetExecutionAppBundle(ctx, appID)
}

var _ ExecutionAppBundleStore = (*postgresStore)(nil)
var _ ExecutionAppBundleStore = (*cachedStore)(nil)
