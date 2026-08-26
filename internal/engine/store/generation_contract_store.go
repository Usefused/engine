package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

var ErrGenerationContractPinUnavailable = errors.New("generation_contract_pin_unavailable")
var ErrServiceProviderIdentityUnavailable = errors.New("service_provider_identity_unavailable")
var generationContractHashPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ValidGenerationContractHash admits only the Registry's canonical immutable object identity.
func ValidGenerationContractHash(hash string) bool {
	return generationContractHashPattern.MatchString(hash)
}

// GenerationAuthSelection limits planning reads to one exact enabled version and its selected operation security.
type GenerationAuthSelection struct {
	ServiceID      uuid.UUID `json:"service_id"`
	Version        string    `json:"version"`
	OperationNames []string  `json:"operation_names"`
	SelectAll      bool      `json:"select_all"`
}

// GenerationOperationSecurity excludes schemas and provider bodies from auth planning.
type GenerationOperationSecurity struct {
	Name                 string                   `json:"name"`
	SecurityRequirements authrouting.Requirements `json:"security_requirements"`
}

// GenerationAuthContract is a selection-filtered projection of an already admitted local snapshot.
type GenerationAuthContract struct {
	GenerationAuthSelection
	ServiceVersionID       uuid.UUID
	GenerationContractHash string
	RuntimeContractHash    string
	AuthConfigs            fusedobject.AuthConfigs
	Operations             []GenerationOperationSecurity
}

// GenerationContractStore keeps generation planning independent of the live Registry catalogue.
type GenerationContractStore interface {
	ResolveGenerationServiceIDsByKeys(context.Context, []string) (map[string]uuid.UUID, error)
	ListGenerationContractBindings(context.Context, []models.ServiceVersionRef, bool) ([]models.SDKContractBinding, error)
	ListGenerationAuthContracts(context.Context, []GenerationAuthSelection, bool) ([]GenerationAuthContract, error)
	ValidateGenerationSelections(context.Context, []models.SDKSelection, bool) error
}

// ResolveGenerationServiceIDsByKeys reuses canonical local matching but requires persisted proof for an explicitly named provider.
func (s *postgresStore) ResolveGenerationServiceIDsByKeys(ctx context.Context, keys []string) (map[string]uuid.UUID, error) {
	result := make(map[string]uuid.UUID, len(keys))
	// Empty plans have no service identity to resolve.
	if len(keys) == 0 {
		return result, nil
	}
	// Provider proof comes from saved version metadata; live lookup and slug stripping alone grant no authority.
	rows, err := s.db.Query(ctx, generationServiceIdentitySQL, keys)
	// Lookup errors must not be treated as a missing or differently owned service.
	if err != nil {
		return nil, fmt.Errorf("resolve pinned service identities: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var serviceID uuid.UUID
		var needsRefresh bool
		// Partial identity rows cannot authorize a generation selection.
		if err := rows.Scan(&key, &serviceID, &needsRefresh); err != nil {
			return nil, err
		}
		// SQL identifies missing proof separately from a wrong provider so legacy snapshots get an actionable repair.
		if needsRefresh {
			return nil, ErrServiceProviderIdentityUnavailable
		}
		result[key] = serviceID
	}
	return result, rows.Err()
}

var generationServiceIdentitySQL = `WITH resolved AS (` + resolveWorkspaceServiceIDsByKeysSQL + `), proof AS (
	SELECT resolved.key, resolved.service_id, service.service_slug,
	 COALESCE(BOOL_OR(NULLIF(snapshot.service_metadata#>>'{provider,handle}', '') IS NOT NULL),false) AS known_provider,
	 COALESCE(BOOL_OR(lower(snapshot.service_metadata#>>'{provider,handle}') = lower(substring(split_part(resolved.key,'/',1) FROM 2))),false) AS provider_matches
	FROM resolved JOIN fused_workspace_services service ON service.service_id=resolved.service_id
	LEFT JOIN fused_workspace_service_versions active ON active.service_id=service.service_id
	LEFT JOIN fused_service_contract_snapshots snapshot ON snapshot.service_id=active.service_id AND snapshot.service_version_id=active.service_version_id
	GROUP BY resolved.key,resolved.service_id,service.service_slug
)
SELECT key,service_id,(key LIKE '@%/%' AND service_slug<>key AND NOT known_provider) AS needs_refresh
FROM proof WHERE key NOT LIKE '@%/%' OR service_slug=key OR provider_matches OR NOT known_provider`

// generationSnapshotJoinSQL keeps Registry labels and exact IDs confined to active workspace snapshots.
const generationSnapshotJoinSQL = `
	JOIN fused_service_contract_snapshots snapshot ON snapshot.service_id = input.service_id
	 AND (snapshot.version = input.version OR snapshot.service_version_id::text = input.version)
	JOIN fused_workspace_service_versions active ON active.service_id = snapshot.service_id
	 AND active.service_version_id = snapshot.service_version_id
`

// ListGenerationContractBindings reads pins in one set-based query; missing versions never fall back to Registry.
func (s *postgresStore) ListGenerationContractBindings(ctx context.Context, refs []models.ServiceVersionRef, requireGenerationPin bool) ([]models.SDKContractBinding, error) {
	// Empty input has no identity to authorize and needs no database work.
	if len(refs) == 0 {
		return nil, nil
	}
	payload, err := encodeGenerationInputs(refs, len(refs))
	// Bounded encoding must succeed before any selection query runs.
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT snapshot.service_id, snapshot.service_version_id, snapshot.version,
		snapshot.revision, snapshot.source_hash, snapshot.generation_contract_hash, snapshot.contract_hash
		FROM jsonb_to_recordset($1::jsonb) AS input(service_id uuid, version text)`+generationSnapshotJoinSQL, payload)
	// Storage failures remain distinct from an absent authoritative pin.
	if err != nil {
		return nil, fmt.Errorf("load generation contract pins: %w", err)
	}
	defer rows.Close()
	bindings := make([]models.SDKContractBinding, 0, len(refs))
	for rows.Next() {
		var binding models.SDKContractBinding
		// A partially scanned pin cannot authorize generation.
		if err := rows.Scan(&binding.ServiceID, &binding.ServiceVersionID, &binding.Version, &binding.Revision, &binding.SourceHash, &binding.GenerationContractHash, &binding.RuntimeContractHash); err != nil {
			return nil, err
		}
		// Old snapshots remain executable but require explicit refresh before generating a new SDK.
		if err := requireGenerationContractPin(binding.GenerationContractHash, requireGenerationPin); err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	// Interrupted reads must retain their storage error instead of recommending an unrelated snapshot refresh.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Every requested identity must resolve exactly once, including duplicated or ambiguous labels.
	if len(bindings) != len(refs) {
		return nil, ErrGenerationContractPinUnavailable
	}
	return bindings, nil
}

// requireGenerationContractPin separates Registry code generation from local-only MCP publication without changing snapshot admission.
func requireGenerationContractPin(hash string, required bool) error {
	// Only SDK generation needs an archived object; MCP executes entirely from the admitted local snapshot.
	if required && !ValidGenerationContractHash(hash) {
		return ErrGenerationContractPinUnavailable
	}
	return nil
}

// encodeGenerationInputs shares bounded batch admission without inspecting or filtering stored data in Go.
func encodeGenerationInputs(value any, count int) ([]byte, error) {
	// App plans cannot turn one database projection into an unbounded scan.
	if count > 1000 {
		return nil, errors.New("generation contract batch exceeds 1000 versions")
	}
	return json.Marshal(value)
}

// ListGenerationAuthContracts projects security only for selected operations in a fixed-query batch.
func (s *postgresStore) ListGenerationAuthContracts(ctx context.Context, selections []GenerationAuthSelection, requireGenerationPin bool) ([]GenerationAuthContract, error) {
	// Webhook-only and empty documents do not need an auth query.
	if len(selections) == 0 {
		return nil, nil
	}
	payload, err := encodeGenerationInputs(selections, len(selections))
	// Malformed or oversized input stops before storage access.
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, generationAuthContractsSQL, payload)
	// A failed dependency cannot be treated as an anonymous contract.
	if err != nil {
		return nil, fmt.Errorf("load generation auth contracts: %w", err)
	}
	defer rows.Close()
	contracts := make([]GenerationAuthContract, 0, len(selections))
	for rows.Next() {
		var contract GenerationAuthContract
		var auth, operations []byte
		// Scanning is atomic for one minimal contract projection.
		if err := rows.Scan(&contract.ServiceID, &contract.Version, &contract.ServiceVersionID, &contract.OperationNames, &contract.SelectAll, &contract.GenerationContractHash, &contract.RuntimeContractHash, &auth, &operations); err != nil {
			return nil, err
		}
		// Only an explicitly pinned snapshot can supply generation authentication authority.
		if err := requireGenerationContractPin(contract.GenerationContractHash, requireGenerationPin); err != nil {
			return nil, err
		}
		// Both pieces share one decoder so malformed security cannot silently become anonymous.
		if err := decodeGenerationAuthContract(&contract, auth, operations); err != nil {
			return nil, err
		}
		contracts = append(contracts, contract)
	}
	// A partial database read is not proof that a generation pin is missing.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Missing snapshots cannot be silently omitted from a multi-service plan.
	if len(contracts) != len(selections) {
		return nil, ErrGenerationContractPinUnavailable
	}
	return contracts, nil
}

// decodeGenerationAuthContract rejects malformed metadata before the shared auth policy resolver sees it.
func decodeGenerationAuthContract(contract *GenerationAuthContract, auth, operations []byte) error {
	// Corrupt auth configuration is a storage failure, never proof of public access.
	if err := json.Unmarshal(auth, &contract.AuthConfigs); err != nil {
		return fmt.Errorf("decode generation auth: %w", err)
	}
	return json.Unmarshal(operations, &contract.Operations)
}

const generationAuthContractsSQL = `
	SELECT input.service_id, input.version, snapshot.service_version_id, input.operation_names, input.select_all,
	 snapshot.generation_contract_hash, snapshot.contract_hash, COALESCE(snapshot.service_metadata->'auth_configs', '[]'::jsonb),
	 COALESCE((SELECT jsonb_agg(jsonb_build_object('name', endpoint.name,
	   'security_requirements', endpoint.operation_json->'security_requirements') ORDER BY endpoint.name)
	 FROM fused_service_contract_endpoints endpoint WHERE endpoint.snapshot_id = snapshot.id
	 AND (input.select_all OR endpoint.name = ANY(input.operation_names))), '[]'::jsonb)
	FROM jsonb_to_recordset($1::jsonb) AS input(service_id uuid, version text, operation_names text[], select_all boolean)
` + generationSnapshotJoinSQL

// ValidateGenerationSelections checks names and exact IDs in SQL, including webhook-only selections.
func (s *postgresStore) ValidateGenerationSelections(ctx context.Context, selections []models.SDKSelection, requireGenerationPin bool) error {
	// No selections means no operation or webhook membership to validate.
	if len(selections) == 0 {
		return nil
	}
	payload, err := encodeGenerationInputs(selections, len(selections))
	// Selection admission is complete before issuing the single validation query.
	if err != nil {
		return err
	}
	var valid bool
	// SQL owns all membership predicates; no catalogue rows are loaded merely to filter in Go.
	if err := s.db.QueryRow(ctx, generationSelectionsSQL, payload, requireGenerationPin).Scan(&valid); err != nil {
		return fmt.Errorf("validate generation selections: %w", err)
	}
	// Invalid names and pins cannot publish a silently narrowed SDK scope.
	if !valid {
		return errors.New("one or more selected operations or webhooks are unavailable in the pinned workspace contract")
	}
	return nil
}

const generationSelectionsSQL = `WITH requested AS (
	SELECT * FROM jsonb_to_recordset($1::jsonb) AS input(service_id uuid, service_version_id uuid,
	 operation_names text[], webhook_names text[], endpoint_ids uuid[], webhook_ids uuid[])
)
SELECT NOT EXISTS (SELECT 1 FROM requested input WHERE NOT EXISTS (
	SELECT 1 FROM fused_service_contract_snapshots snapshot
	JOIN fused_workspace_service_versions active ON active.service_id = snapshot.service_id AND active.service_version_id = snapshot.service_version_id
	WHERE snapshot.service_id = input.service_id AND snapshot.service_version_id = input.service_version_id
	 AND (NOT $2::boolean OR snapshot.generation_contract_hash ~ '^sha256:[0-9a-f]{64}$')
	 AND NOT EXISTS (SELECT 1 FROM unnest(input.operation_names) requested_name(value) WHERE NOT EXISTS (
	  SELECT 1 FROM fused_service_contract_endpoints ep WHERE ep.snapshot_id = snapshot.id AND ep.name = requested_name.value))
	 AND NOT EXISTS (SELECT 1 FROM unnest(input.webhook_names) requested_name(value) WHERE NOT EXISTS (
	  SELECT 1 FROM fused_service_contract_webhooks wh WHERE wh.snapshot_id = snapshot.id AND wh.name = requested_name.value))
	 AND NOT EXISTS (SELECT 1 FROM unnest(input.endpoint_ids) requested_id(value) WHERE NOT EXISTS (
	  SELECT 1 FROM fused_service_contract_endpoints ep WHERE ep.snapshot_id = snapshot.id AND ep.endpoint_id = requested_id.value))
	 AND NOT EXISTS (SELECT 1 FROM unnest(input.webhook_ids) requested_id(value) WHERE NOT EXISTS (
	  SELECT 1 FROM fused_service_contract_webhooks wh WHERE wh.snapshot_id = snapshot.id AND wh.webhook_id = requested_id.value))
))`
