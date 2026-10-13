package store

import (
	"context"
	"errors"
	"github.com/google/uuid"
)

// UnifiedAppRuntimeBucketStore deliberately bypasses runtime/token caches for mutable bucket authority.
type UnifiedAppRuntimeBucketStore interface {
	AuthorizeUnifiedAppRuntimeBucket(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error)
}

// AuthorizeUnifiedAppRuntimeBucket intersects immutable scope, live token authority and the owner's current bucket.use grant.
func (s *postgresStore) AuthorizeUnifiedAppRuntimeBucket(ctx context.Context, accountID, appID, tokenID, bucketID uuid.UUID) (bool, error) {
	var allowed bool
	err := s.db.QueryRow(ctx, unifiedAppRuntimeBucketSQL, accountID, appID, tokenID, bucketID).Scan(&allowed)
	return allowed, err
}

// AuthorizeUnifiedAppRuntimeBucket never caches grants, expiry, deletion or scope admission.
func (s *cachedStore) AuthorizeUnifiedAppRuntimeBucket(ctx context.Context, accountID, appID, tokenID, bucketID uuid.UUID) (bool, error) {
	repository, ok := s.Store.(UnifiedAppRuntimeBucketStore)
	// A wrapper without authoritative backing cannot broaden routing.
	if !ok {
		return false, errors.New("runtime bucket authorization unavailable")
	}
	return repository.AuthorizeUnifiedAppRuntimeBucket(ctx, accountID, appID, tokenID, bucketID)
}

// sameRuntimeBucket keeps idempotency tied to credential routing as well as the authored input.
func sameRuntimeBucket(a, b *uuid.UUID) bool {
	// Absence means static routing, which is distinct from every explicit selection.
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// The bearer token delegates the owning subject/team's bucket authority only within the reviewed version scope.
// Fixed connection tokens cannot be repurposed for a different bucket, and revoked/expired tokens fail immediately.
const unifiedAppRuntimeBucketSQL = `
WITH candidate AS (
 SELECT family.owner_subject_id, family.owner_team_id, workspace.id AS workspace_id
 FROM fused_apps app
 JOIN fused_app_families family ON family.app_family_id=app.app_family_id AND family.account_id=app.account_id
 JOIN fused_workspaces workspace ON workspace.account_id=app.account_id AND workspace.singleton_key=1
 JOIN fused_buckets bucket ON bucket.id=$4
 JOIN fused_app_tokens token ON token.app_family_id=family.app_family_id AND token.id=$3
 JOIN LATERAL (
   SELECT applied.resolved_payload FROM fused_config_plans applied
   WHERE applied.config_key=app.config_key AND applied.source_hash=app.source_hash AND applied.status='applied'
     AND NOT COALESCE((applied.resolved_payload->>'noop')::boolean,false)
   ORDER BY applied.applied_at DESC, applied.created_at DESC LIMIT 1
 ) plan ON true
 WHERE app.account_id=$1 AND app.app_id=$2 AND app.status IN ('active','deprecated')
   AND family.kind='unified_app' AND family.archived_at IS NULL AND family.unified_active_app_id=app.app_id
   AND token.binding_mode='dynamic' AND (token.expires_at IS NULL OR token.expires_at>NOW())
   AND (token.allow_all OR 'execute'=ANY(token.allowed_operations))
   AND EXISTS (SELECT 1 FROM jsonb_array_elements(COALESCE(plan.resolved_payload->'runtime_buckets','[]'::jsonb)) ref
               WHERE ref->>'bucket_id'=$4::text)
), principals(subject_type, subject_id) AS (
 SELECT 'subject'::text, subject.id FROM candidate c JOIN fused_subjects subject ON subject.id=c.owner_subject_id AND subject.status='active'
 UNION
 SELECT 'team'::text, membership.team_id FROM candidate c
 JOIN fused_subjects subject ON subject.id=c.owner_subject_id AND subject.status='active'
 JOIN fused_team_memberships membership ON membership.member_subject_id=subject.id
 JOIN fused_teams team ON team.id=membership.team_id AND team.status='active'
 UNION
 SELECT 'team'::text, team.id FROM candidate c JOIN fused_teams team ON team.id=c.owner_team_id AND team.status='active'
 UNION
 SELECT 'workspace'::text, c.workspace_id FROM candidate c WHERE
   EXISTS (SELECT 1 FROM fused_subjects subject WHERE subject.id=c.owner_subject_id AND subject.status='active') OR
   EXISTS (SELECT 1 FROM fused_teams team WHERE team.id=c.owner_team_id AND team.status='active')
)
SELECT EXISTS (
 SELECT 1 FROM principals principal
 JOIN fused_role_bindings binding ON binding.subject_type=principal.subject_type AND binding.subject_id=principal.subject_id
 JOIN fused_roles role ON role.id=binding.role_id AND role.scope_type=binding.resource_type
 JOIN fused_role_permissions permission ON permission.role_id=role.id AND permission.permission='bucket.use'
 WHERE (binding.resource_type='bucket' AND binding.resource_id=$4) OR
       (binding.resource_type='workspace' AND binding.resource_id=(SELECT workspace_id FROM candidate))
)`

// UnifiedAppRuntimeBucketScope exposes only the deployment's non-secret bucket identities for clients.
type UnifiedAppRuntimeBucketScope interface {
	ReadUnifiedAppRuntimeBuckets(context.Context, uuid.UUID, uuid.UUID) ([]Bucket, error)
}

// ReadUnifiedAppRuntimeBuckets projects exact applied scope, never newly created buckets with reused names.
func (s *postgresStore) ReadUnifiedAppRuntimeBuckets(ctx context.Context, accountID, appID uuid.UUID) ([]Bucket, error) {
	rows, err := s.db.Query(ctx, `
 SELECT (ref->>'bucket_id')::uuid, ref->>'bucket_name'
 FROM fused_apps app JOIN fused_app_families family ON family.app_family_id=app.app_family_id
 JOIN LATERAL (
 SELECT applied.resolved_payload FROM fused_config_plans applied
 WHERE applied.config_key=app.config_key AND applied.source_hash=app.source_hash AND applied.status='applied'
 AND NOT COALESCE((applied.resolved_payload->>'noop')::boolean,false)
 ORDER BY applied.applied_at DESC, applied.created_at DESC LIMIT 1
 ) plan ON true
 CROSS JOIN LATERAL jsonb_array_elements(COALESCE(plan.resolved_payload->'runtime_buckets','[]'::jsonb)) ref
 WHERE app.account_id=$1 AND app.app_id=$2 AND family.kind='unified_app'
 ORDER BY ref->>'bucket_name' LIMIT 33`, accountID, appID)
	// Scope read errors cannot be presented as a static app contract.
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	buckets := make([]Bucket, 0)
	for rows.Next() {
		var bucket Bucket
		// A malformed persisted ID must not become an unrestricted schema.
		if err := rows.Scan(&bucket.ID, &bucket.Name); err != nil {
			return nil, err
		}
		buckets = append(buckets, bucket)
	}
	if len(buckets) > 32 {
		return nil, errors.New("invalid runtime bucket scope")
	}
	return buckets, rows.Err()
}

// ReadUnifiedAppRuntimeBuckets uses the same authoritative applied metadata through store wrappers.
func (s *cachedStore) ReadUnifiedAppRuntimeBuckets(ctx context.Context, accountID, appID uuid.UUID) ([]Bucket, error) {
	repository, ok := s.Store.(UnifiedAppRuntimeBucketScope)
	// Missing backing cannot fabricate public deployment scope.
	if !ok {
		return nil, errors.New("runtime bucket scope unavailable")
	}
	return repository.ReadUnifiedAppRuntimeBuckets(ctx, accountID, appID)
}
