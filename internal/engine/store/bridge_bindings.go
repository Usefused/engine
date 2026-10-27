package store

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// BridgeBindingStore shares immutable app publication rather than introducing a separate app lifecycle.
type BridgeBindingStore interface {
	ResolveBridgeBindings(context.Context, uuid.UUID, map[string]models.BridgeReference) ([]models.BridgeBinding, error)
	ReadBridgeBindings(context.Context, uuid.UUID, uuid.UUID) ([]models.BridgeBinding, error)
}

// ResolveBridgeBindings freezes all requested contracts with one account-scoped snapshot query.
func (s *postgresStore) ResolveBridgeBindings(ctx context.Context, account uuid.UUID, refs map[string]models.BridgeReference) ([]models.BridgeBinding, error) {
	// Empty attachment sets impose no storage dependency on existing applications.
	if len(refs) == 0 {
		return nil, nil
	}
	// Bound the query and generated SDK surface before allocating descriptors.
	if len(refs) > 100 {
		return nil, errors.New("at most 100 Bridge capabilities may be attached")
	}
	refs, groups, err := s.expandBridgeGroups(ctx, account, refs)
	if err != nil {
		return nil, err
	}
	requested := make([]models.BridgeBinding, 0, len(refs))
	for alias, ref := range refs {
		requested = append(requested, models.BridgeBinding{Alias: alias, BridgeReference: ref})
	}
	raw, _ := json.Marshal(requested)
	rows, err := s.db.Query(ctx, `WITH requested AS (
 SELECT * FROM jsonb_to_recordset($2::jsonb) AS r(alias text,pairing_id text,profile_id text,manifest_hash text,connection_id text,operation_id text))
 SELECT p.workspace_id,r.alias,r.pairing_id,r.profile_id,r.manifest_hash,r.connection_id,r.operation_id,op->>'contract_hash',op->>'contract_json'
 FROM requested r JOIN fused_bridge_pairings p ON p.id::text=r.pairing_id AND p.account_id=$1 AND p.revoked_at IS NULL AND p.scope='workspace' AND EXISTS(SELECT 1 FROM fused_bridge_profiles current WHERE current.pairing_id=p.id AND current.profile_id=r.profile_id AND current.state<>'removed')
 JOIN fused_bridge_manifests m ON m.pairing_id=p.id AND m.profile_id=r.profile_id AND m.hash=r.manifest_hash
 CROSS JOIN LATERAL jsonb_array_elements(convert_from(m.document,'UTF8')::jsonb->'operations') op
 WHERE op->>'connection_id'=r.connection_id AND op->>'operation_id'=r.operation_id`, account, raw)
	// A storage failure cannot narrow an authored capability set silently.
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []models.BridgeBinding{}
	for rows.Next() {
		var b models.BridgeBinding
		if err = rows.Scan(&b.WorkspaceID, &b.Alias, &b.PairingID, &b.ProfileID, &b.ManifestHash, &b.ConnectionID, &b.OperationID, &b.ContractHash, &b.ContractJSON); err != nil {
			return nil, err
		}
		var c struct {
			Kind   string          `json:"kind"`
			Input  json.RawMessage `json:"input_schema"`
			Output json.RawMessage `json:"output_schema"`
		}
		// Only Engine-validated manifests may provide the public generated contracts.
		if json.Unmarshal([]byte(b.ContractJSON), &c) != nil {
			return nil, errors.New("invalid Bridge contract")
		}
		b.Kind, b.InputSchema, b.OutputSchema = c.Kind, c.Input, c.Output
		b.Group = groups[b.Alias]
		result = append(result, b)
	}
	// Missing or duplicate resolved entries invalidate the complete selection.
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	if len(result) != len(refs) {
		return nil, errors.New("Bridge capability unavailable")
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Alias < result[j].Alias })
	return result, nil
}

// ReadBridgeBindings reads the successful immutable plan, never the current profile catalogue.
func (s *postgresStore) ReadBridgeBindings(ctx context.Context, account, app uuid.UUID) ([]models.BridgeBinding, error) {
	var raw []byte
	var expected int
	err := s.db.QueryRow(ctx, `SELECT COALESCE(plan.resolved_payload->'bridges','[]'::jsonb),
 (SELECT count(*) FROM fused_app_capabilities capability WHERE capability.app_id=app.app_id AND capability.capability_key LIKE 'bridge:%')
 FROM fused_apps app JOIN fused_app_families family ON family.app_family_id=app.app_family_id
 LEFT JOIN LATERAL (SELECT applied.resolved_payload FROM fused_config_plans applied
 WHERE applied.config_key=app.config_key AND applied.source_hash=app.source_hash AND applied.status='applied'
 AND NOT COALESCE((applied.resolved_payload->>'noop')::boolean,false)
 ORDER BY applied.applied_at DESC,applied.created_at DESC LIMIT 1) plan ON true
 WHERE app.account_id=$1 AND app.app_id=$2 AND app.status IN ('active','deprecated') AND family.archived_at IS NULL`, account, app).Scan(&raw, &expected)
	// Unpublished or deactivated versions cannot acquire local-device execution rights.
	if err != nil {
		return nil, err
	}
	var bindings []models.BridgeBinding
	// Historical provider-only apps have no plan dependency; published Bridge keys require every preserved contract.
	if json.Unmarshal(raw, &bindings) != nil || len(bindings) > 100 || len(bindings) != expected {
		return nil, errors.New("invalid Bridge bindings")
	}
	return bindings, nil
}

// ResolveBridgeBindings keeps plan authority uncached so revocation is visible at admission.
func (s *cachedStore) ResolveBridgeBindings(ctx context.Context, account uuid.UUID, refs map[string]models.BridgeReference) ([]models.BridgeBinding, error) {
	repository, ok := s.Store.(BridgeBindingStore)
	// Storage adapters without Bridge support cannot accept unchecked references.
	if !ok {
		return nil, errors.New("Bridge binding store unavailable")
	}
	return repository.ResolveBridgeBindings(ctx, account, refs)
}

// ReadBridgeBindings leaves exact app deletion and revocation visible to every dispatch.
func (s *cachedStore) ReadBridgeBindings(ctx context.Context, account, app uuid.UUID) ([]models.BridgeBinding, error) {
	repository, ok := s.Store.(BridgeBindingStore)
	if !ok {
		return nil, errors.New("Bridge binding store unavailable")
	}
	return repository.ReadBridgeBindings(ctx, account, app)
}
