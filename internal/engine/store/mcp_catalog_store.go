package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Usefused/engine/internal/engine/mcpcatalog"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrMCPCatalogConflict = errors.New("MCP preview expired or the saved catalog changed; discover again")
var ErrMCPCatalogUnavailable = errors.New("MCP catalog storage unavailable")

// MCPCatalogScope keeps authorization-dependent discovery private to its exact actor and service version.
type MCPCatalogScope struct{ ServiceID, VersionID, SubjectID uuid.UUID }
type MCPCatalogSnapshot struct {
	ID         uuid.UUID          `json:"id"`
	URL        string             `json:"url"`
	BucketName string             `json:"bucket_name,omitempty"`
	SecretName string             `json:"secret_name,omitempty"`
	BucketID   uuid.UUID          `json:"-"`
	CreatedAt  time.Time          `json:"created_at"`
	Catalog    mcpcatalog.Catalog `json:"catalog"`
}
type MCPCatalogDraft struct {
	MCPCatalogSnapshot
	ExpiresAt time.Time          `json:"expires_at"`
	Changes   mcpcatalog.Changes `json:"changes"`
	BaseID    *uuid.UUID         `json:"-"`
}

// MCPCatalogStore owns immutable previews and atomic promotion without modifying published Registry contracts.
type MCPCatalogStore interface {
	GetMCPCatalog(context.Context, MCPCatalogScope) (*MCPCatalogSnapshot, error)
	SaveMCPCatalogDraft(context.Context, MCPCatalogScope, MCPCatalogDraft) error
	GetMCPCatalogDraft(context.Context, MCPCatalogScope, uuid.UUID) (*MCPCatalogDraft, error)
	ApplyMCPCatalogDraft(context.Context, MCPCatalogScope, uuid.UUID) (*MCPCatalogSnapshot, error)
}

// GetMCPCatalog reads only the exact actor's latest approved definitions from local storage.
func (s *postgresStore) GetMCPCatalog(ctx context.Context, scope MCPCatalogScope) (*MCPCatalogSnapshot, error) {
	var raw []byte
	var bucketID *uuid.UUID
	err := s.db.QueryRow(ctx, `SELECT r.payload, r.bucket_id FROM fused_service_mcp_catalogs c JOIN fused_service_mcp_catalog_revisions r ON r.id=c.current_id AND r.service_id=c.service_id AND r.service_version_id=c.service_version_id AND r.subject_id=c.subject_id WHERE c.service_id=$1 AND c.service_version_id=$2 AND c.subject_id=$3`, scope.ServiceID, scope.VersionID, scope.SubjectID).Scan(&raw, &bucketID)
	// No approved revision is a normal empty state, not a discovery failure.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	// A database outage must remain distinguishable from a service with no imported catalog.
	if err != nil {
		return nil, err
	}
	var result MCPCatalogSnapshot
	// Decode errors must not masquerade as a valid empty catalog.
	if err = json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	// Restore the credential reference so later reads can enforce current bucket access.
	if bucketID != nil {
		result.BucketID = *bucketID
	}
	return &result, nil
}

// lockMCPCatalog serializes promotions and holds the parent workspace version against concurrent removal.
func lockMCPCatalog(ctx context.Context, tx pgx.Tx, scope MCPCatalogScope) (*uuid.UUID, error) {
	var serviceID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT service_id FROM fused_workspace_service_versions WHERE service_id=$1 AND service_version_id=$2 AND status <> 'deprecated' FOR SHARE`, scope.ServiceID, scope.VersionID).Scan(&serviceID)
	// An inactive or removed workspace version cannot acquire new catalog state.
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO fused_service_mcp_catalogs(service_id,service_version_id,subject_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, scope.ServiceID, scope.VersionID, scope.SubjectID)
	// The actor-scoped parent must exist before its current revision can be locked.
	if err != nil {
		return nil, err
	}
	var current *uuid.UUID
	err = tx.QueryRow(ctx, `SELECT current_id FROM fused_service_mcp_catalogs WHERE service_id=$1 AND service_version_id=$2 AND subject_id=$3 FOR UPDATE`, scope.ServiceID, scope.VersionID, scope.SubjectID).Scan(&current)
	return current, err
}

// sameCatalogRevision treats the absence of a saved catalog as a real compare-and-swap base.
func sameCatalogRevision(a, b *uuid.UUID) bool {
	// Nil denotes the first import rather than an arbitrary current revision.
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// SaveMCPCatalogDraft retains one pending preview per actor/version and rejects discovery based on a superseded snapshot.
func (s *postgresStore) SaveMCPCatalogDraft(ctx context.Context, scope MCPCatalogScope, draft MCPCatalogDraft) error {
	raw, err := json.Marshal(draft)
	// Store only complete, bounded, credential-free server-owned previews.
	if err != nil || len(raw) > mcpcatalog.MaxCatalogBytes+32768 {
		return mcpcatalog.ErrInvalidCatalog
	}
	tx, err := s.db.Begin(ctx)
	// Catalog writes require a transaction to keep preview and head changes atomic.
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	current, err := lockMCPCatalog(ctx, tx, scope)
	// Catalog writes require a transaction to keep preview and head changes atomic.
	if err != nil {
		return err
	}
	// A concurrent import invalidates the diff the user would otherwise review.
	if !sameCatalogRevision(current, draft.BaseID) {
		return ErrMCPCatalogConflict
	}
	_, err = tx.Exec(ctx, `DELETE FROM fused_service_mcp_catalog_revisions WHERE service_id=$1 AND service_version_id=$2 AND subject_id=$3 AND applied_at IS NULL`, scope.ServiceID, scope.VersionID, scope.SubjectID)
	// Replacing a pending preview must be atomic with removal of its predecessor.
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO fused_service_mcp_catalog_revisions(id,service_id,service_version_id,subject_id,base_id,bucket_id,payload,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, draft.ID, scope.ServiceID, scope.VersionID, scope.SubjectID, draft.BaseID, nullableMCPCatalogBucket(draft.BucketID), raw, draft.ExpiresAt)
	// Preview insertion and old-preview cleanup must commit together.
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// nullableMCPCatalogBucket stores no fabricated bucket reference for unauthenticated servers.
func nullableMCPCatalogBucket(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// readMCPCatalogDraft always includes the actor/version tuple so an opaque draft ID alone grants no access.
func readMCPCatalogDraft(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, scope MCPCatalogScope, id uuid.UUID) (*MCPCatalogDraft, *time.Time, error) {
	var raw []byte
	var base, bucket *uuid.UUID
	var applied *time.Time
	var expires time.Time
	err := db.QueryRow(ctx, `SELECT payload,base_id,bucket_id,expires_at,applied_at FROM fused_service_mcp_catalog_revisions WHERE id=$1 AND service_id=$2 AND service_version_id=$3 AND subject_id=$4`, id, scope.ServiceID, scope.VersionID, scope.SubjectID).Scan(&raw, &base, &bucket, &expires, &applied)
	// Unknown, deleted, or foreign drafts share a safe recovery path.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrMCPCatalogConflict
	}
	// Draft lookup failures cannot authorize promotion or expose incomplete stored data.
	if err != nil {
		return nil, nil, err
	}
	var result MCPCatalogDraft
	// Corrupt stored definitions must not become a valid-looking empty catalog.
	if err = json.Unmarshal(raw, &result); err != nil {
		return nil, nil, err
	}
	result.BaseID = base
	result.ExpiresAt = expires
	// Restore the credential reference so later reads can enforce current bucket access.
	if bucket != nil {
		result.BucketID = *bucket
	}
	return &result, applied, nil
}

// GetMCPCatalogDraft allows authorization to be rechecked against the server-held credential reference before promotion.
func (s *postgresStore) GetMCPCatalogDraft(ctx context.Context, scope MCPCatalogScope, id uuid.UUID) (*MCPCatalogDraft, error) {
	draft, _, err := readMCPCatalogDraft(ctx, s.db, scope, id)
	return draft, err
}

// ApplyMCPCatalogDraft atomically advances the head after expiry and base checks, retaining prior approved revisions.
func (s *postgresStore) ApplyMCPCatalogDraft(ctx context.Context, scope MCPCatalogScope, id uuid.UUID) (*MCPCatalogSnapshot, error) {
	tx, err := s.db.Begin(ctx)
	// Catalog writes require a transaction to keep preview and head changes atomic.
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	current, err := lockMCPCatalog(ctx, tx, scope)
	// Catalog writes require a transaction to keep preview and head changes atomic.
	if err != nil {
		return nil, err
	}
	draft, applied, err := readMCPCatalogDraft(ctx, tx, scope, id)
	// Promotion requires the locked current revision to prevent concurrent lost updates.
	if err != nil {
		return nil, err
	}
	unchanged, err := validateMCPCatalogPromotion(current, draft, applied)
	// Retries return the already-approved revision; stale or expired drafts never advance the head.
	if err != nil {
		return nil, err
	}
	// An idempotent retry returns the immutable current snapshot without another write.
	if unchanged {
		return &draft.MCPCatalogSnapshot, nil
	}
	_, err = tx.Exec(ctx, `UPDATE fused_service_mcp_catalog_revisions SET applied_at=clock_timestamp() WHERE id=$1`, id)
	// The approval receipt must be recorded before advancing the head in the same transaction.
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `UPDATE fused_service_mcp_catalogs SET current_id=$4 WHERE service_id=$1 AND service_version_id=$2 AND subject_id=$3`, scope.ServiceID, scope.VersionID, scope.SubjectID, id)
	// The revision receipt and the selected head are one indivisible mutation.
	if err != nil {
		return nil, err
	}
	// A failed commit cannot be reported as a successful durable import.
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &draft.MCPCatalogSnapshot, nil
}

// validateMCPCatalogPromotion distinguishes idempotent retries from stale, expired, or rollback attempts.
func validateMCPCatalogPromotion(current *uuid.UUID, draft *MCPCatalogDraft, applied *time.Time) (bool, error) {
	// Only the current approved revision can be replayed without a new write.
	if current != nil && *current == draft.ID && applied != nil {
		return true, nil
	}
	// Every new promotion must still match the reviewed base and its review lifetime.
	if applied != nil || !draft.ExpiresAt.After(time.Now()) || !sameCatalogRevision(current, draft.BaseID) {
		return false, ErrMCPCatalogConflict
	}
	return false, nil
}

// mcpCatalogDelegate forwards uncached actor-private catalogs so permission changes never leave a shared cached projection.
func (s *cachedStore) mcpCatalogDelegate() (MCPCatalogStore, error) {
	delegate, ok := s.Store.(MCPCatalogStore)
	// Test doubles and older store implementations fail closed instead of dropping a persistence step.
	if !ok {
		return nil, ErrMCPCatalogUnavailable
	}
	return delegate, nil
}

// GetMCPCatalog forwards the exact private scope to durable storage.
func (s *cachedStore) GetMCPCatalog(ctx context.Context, scope MCPCatalogScope) (*MCPCatalogSnapshot, error) {
	d, err := s.mcpCatalogDelegate()
	// Without durable catalog support the cache wrapper must fail closed.
	if err != nil {
		return nil, err
	}
	return d.GetMCPCatalog(ctx, scope)
}

// SaveMCPCatalogDraft forwards previews without caching mutable draft state.
func (s *cachedStore) SaveMCPCatalogDraft(ctx context.Context, scope MCPCatalogScope, draft MCPCatalogDraft) error {
	d, err := s.mcpCatalogDelegate()
	// Without durable catalog support the cache wrapper must fail closed.
	if err != nil {
		return err
	}
	return d.SaveMCPCatalogDraft(ctx, scope, draft)
}

// GetMCPCatalogDraft loads the authoritative reference used for apply authorization.
func (s *cachedStore) GetMCPCatalogDraft(ctx context.Context, scope MCPCatalogScope, id uuid.UUID) (*MCPCatalogDraft, error) {
	d, err := s.mcpCatalogDelegate()
	// Without durable catalog support the cache wrapper must fail closed.
	if err != nil {
		return nil, err
	}
	return d.GetMCPCatalogDraft(ctx, scope, id)
}

// ApplyMCPCatalogDraft leaves compare-and-swap and idempotency inside the database transaction.
func (s *cachedStore) ApplyMCPCatalogDraft(ctx context.Context, scope MCPCatalogScope, id uuid.UUID) (*MCPCatalogSnapshot, error) {
	d, err := s.mcpCatalogDelegate()
	// Without durable catalog support the cache wrapper must fail closed.
	if err != nil {
		return nil, err
	}
	return d.ApplyMCPCatalogDraft(ctx, scope, id)
}
