package store

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/Usefused/engine/internal/shared/canonical"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// UnifiedAppAttachmentStore resolves bounded hosted dependencies and reads exact applied bindings.
type UnifiedAppAttachmentStore interface {
	ResolveUnifiedAppBindings(context.Context, uuid.UUID, map[string]models.UnifiedAppReference) ([]models.UnifiedAppBinding, error)
	ReadUnifiedAppBindings(context.Context, uuid.UUID, uuid.UUID) ([]models.UnifiedAppBinding, error)
}

// ResolveUnifiedAppBindings uses one account-scoped query so dependency count cannot amplify database work.
func (s *postgresStore) ResolveUnifiedAppBindings(ctx context.Context, accountID uuid.UUID, refs map[string]models.UnifiedAppReference) ([]models.UnifiedAppBinding, error) {
	// Empty references preserve ordinary SDK and MCP planning without a database dependency.
	if len(refs) == 0 {
		return nil, nil
	}
	// Bound both the SQL input and the generated public API surface.
	if len(refs) > 16 {
		return nil, errors.New("at most 16 Unified Apps may be attached")
	}
	rows := make([]map[string]string, 0, len(refs))
	for alias, ref := range refs {
		name, _, err := canonical.AppName(ref.Name)
		// Canonicalization must match family creation rather than SQL locale rules.
		if err != nil {
			return nil, err
		}
		rows = append(rows, map[string]string{"alias": alias, "name": name, "version": ref.Version})
	}
	raw, err := json.Marshal(rows)
	// Serialization failure cannot degrade into an unbounded query.
	if err != nil {
		return nil, err
	}
	result, err := s.db.Query(ctx, `
 SELECT requested.alias, family.display_name, app.version, app.app_id, family.app_family_id,
        app.source_hash, app.bundle_digest, bundle.manifest->'inputSchema', bundle.manifest->'outputSchema'
 FROM jsonb_to_recordset($2::jsonb) requested(alias text, name text, version text)
 JOIN fused_app_families family ON family.account_id=$1 AND family.kind='unified_app'
   AND family.canonical_name=requested.name AND family.archived_at IS NULL
 JOIN fused_apps app ON app.app_family_id=family.app_family_id AND app.version=requested.version
   AND app.status IN ('active','deprecated') AND family.unified_active_app_id=app.app_id
 JOIN fused_unified_app_bundles bundle ON bundle.app_id=app.app_id AND bundle.source_hash=app.source_hash
 ORDER BY requested.alias`, accountID, raw)
	// Lookup failure must never look like an empty attachment set.
	if err != nil {
		return nil, err
	}
	defer result.Close()
	bindings := make([]models.UnifiedAppBinding, 0, len(refs))
	for result.Next() {
		var binding models.UnifiedAppBinding
		// Every requested alias must have a complete immutable public contract.
		if err := result.Scan(&binding.Alias, &binding.Name, &binding.Version, &binding.AppID, &binding.AppFamilyID, &binding.SourceHash, &binding.BundleDigest, &binding.InputSchema, &binding.OutputSchema); err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	// Partial resolution must not silently omit a requested capability.
	if err := result.Err(); err != nil {
		return nil, err
	}
	if len(bindings) != len(refs) {
		return nil, errors.New("a referenced Unified App version is unavailable")
	}
	return bindings, nil
}

// ReadUnifiedAppBindings reconstructs delegation only from the consumer version's successful immutable plan.
func (s *postgresStore) ReadUnifiedAppBindings(ctx context.Context, accountID, appID uuid.UUID) ([]models.UnifiedAppBinding, error) {
	var raw []byte
	err := s.db.QueryRow(ctx, `
 SELECT COALESCE(plan.resolved_payload->'unified_apps','[]'::jsonb)
 FROM fused_apps app JOIN fused_app_families family ON family.app_family_id=app.app_family_id
 JOIN LATERAL (
   SELECT applied.resolved_payload FROM fused_config_plans applied
   WHERE applied.config_key=app.config_key AND applied.source_hash=app.source_hash AND applied.status='applied'
     AND NOT COALESCE((applied.resolved_payload->>'noop')::boolean,false)
   ORDER BY applied.applied_at DESC, applied.created_at DESC LIMIT 1
 ) plan ON true
 WHERE app.account_id=$1 AND app.app_id=$2 AND app.status IN ('active','deprecated')
   AND family.kind IN ('sdk','mcp','unified_app') AND family.archived_at IS NULL`, accountID, appID).Scan(&raw)
	// Missing publication authority fails closed even if a version row remains.
	if err != nil {
		return nil, err
	}
	var bindings []models.UnifiedAppBinding
	if err := json.Unmarshal(raw, &bindings); err != nil {
		return nil, err
	}
	// Apply-time bounds remain mandatory when reading persisted metadata.
	if len(bindings) > 16 {
		return nil, errors.New("invalid Unified App bindings")
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Alias < bindings[j].Alias })
	return bindings, nil
}

// ResolveUnifiedAppBindings never caches deployment or dependency admission.
func (s *cachedStore) ResolveUnifiedAppBindings(ctx context.Context, accountID uuid.UUID, refs map[string]models.UnifiedAppReference) ([]models.UnifiedAppBinding, error) {
	repository, ok := s.Store.(UnifiedAppAttachmentStore)
	// Missing storage cannot authorize a hosted dependency.
	if !ok {
		return nil, errors.New("Unified App attachments unavailable")
	}
	return repository.ResolveUnifiedAppBindings(ctx, accountID, refs)
}

// ReadUnifiedAppBindings leaves runtime revocation and deletion visible on every request.
func (s *cachedStore) ReadUnifiedAppBindings(ctx context.Context, accountID, appID uuid.UUID) ([]models.UnifiedAppBinding, error) {
	repository, ok := s.Store.(UnifiedAppAttachmentStore)
	// Unsupported stores fail closed instead of guessing from public app metadata.
	if !ok {
		return nil, errors.New("Unified App attachments unavailable")
	}
	return repository.ReadUnifiedAppBindings(ctx, accountID, appID)
}
