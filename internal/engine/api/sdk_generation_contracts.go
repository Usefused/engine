package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// generationPlanningClient overrides only catalogue-dependent planning reads;
// the ordinary Registry client still owns generation, transport, and notifications.
type generationPlanningClient struct {
	sandbox.RegistryClient
	contracts            store.GenerationContractStore
	observed             map[uuid.UUID]string
	requireGenerationPin bool
}

// workspaceServicesByKeys composes canonical identity resolution with the existing SQL-filtered metadata reader.
func (c *generationPlanningClient) workspaceServicesByKeys(ctx context.Context, s store.Store, doc sdkConfigDocument) (map[string]store.WorkspaceService, error) {
	keys := unresolvedSDKServiceKeys(doc, nil)
	resolved, err := c.contracts.ResolveGenerationServiceIDsByKeys(ctx, keys)
	// Absent provider proof remains repairable without selecting a similarly named service.
	if err != nil {
		return nil, generationPinPlanError(err, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to resolve local service identities"})
	}
	ids := make([]uuid.UUID, 0, len(resolved))
	for _, id := range resolved {
		ids = append(ids, id)
	}
	services, err := s.ListAuthorizedWorkspaceServices(ctx, accesscontrol.AuthorizedScope{IDs: ids}, nil)
	// Metadata is loaded only for the already-resolved requested IDs, never every workspace service.
	if err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to load local service identities"}
	}
	byID := workspaceServicesByID(services)
	result := make(map[string]store.WorkspaceService, len(resolved))
	for key, id := range resolved {
		service, exists := byID[id]
		// A concurrent workspace removal cannot produce a partial valid-looking selection.
		if !exists {
			return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "selected workspace service changed; create a new plan"}
		}
		result[key] = service
	}
	return result, nil
}

// localGenerationPlanningClient reuses the shared SDK/MCP planner while production stores resolve from their exact snapshots.
func localGenerationPlanningClient(s store.Store, registry sandbox.RegistryClient) (sandbox.RegistryClient, error) {
	return localSnapshotPlanningClient(s, registry, true)
}

// localSnapshotPlanningClient keeps one planner while requiring Registry archival authority only for generated SDK packages.
func localSnapshotPlanningClient(s store.Store, registry sandbox.RegistryClient, requireGenerationPin bool) (sandbox.RegistryClient, error) {
	contracts, ok := s.(store.GenerationContractStore)
	// Focused doubles explicitly provide selection admission; an ordinary Registry transport cannot replace absent local authority.
	if !ok {
		// Production HTTP clients have no selection capability, so a missing snapshot store fails before any live lookup.
		if _, supportsSelection := registry.(sdkSelectionValidator); !supportsSelection {
			return nil, localPlanningUnavailableError()
		}
		return registry, nil
	}
	return &generationPlanningClient{RegistryClient: registry, contracts: contracts, observed: make(map[uuid.UUID]string), requireGenerationPin: requireGenerationPin}, nil
}

// localPlanningUnavailableError distinguishes absent storage support from an individual refreshable SDK pin.
func localPlanningUnavailableError() error {
	return workspaceConfigHTTPError{status: http.StatusServiceUnavailable, code: "local_contract_store_unavailable", category: "dependency",
		message: "Local service contract storage is unavailable. Restart Engine with a supported snapshot store, then create a new plan."}
}

// FetchServiceVersionRevisions keeps the existing before/after-generation checks tied to the local pin instead of current Registry visibility.
func (c *generationPlanningClient) FetchServiceVersionRevisions(ctx context.Context, refs []sandbox.ServiceVersionRef, _ string) ([]sandbox.ServiceVersionRevision, error) {
	bindings, err := c.contracts.ListGenerationContractBindings(ctx, refs, c.requireGenerationPin)
	// A missing pin is actionable; network fallback would silently select a different contract.
	if err != nil {
		return nil, err
	}
	revisions := make([]sandbox.ServiceVersionRevision, len(bindings))
	for i, binding := range bindings {
		// A concurrent refresh between auth planning and binding cannot mix two revisions in one plan.
		if hash := c.observed[binding.ServiceVersionID]; hash != "" && hash != planningContractIdentity(binding.GenerationContractHash, binding.RuntimeContractHash, c.requireGenerationPin) {
			return nil, errors.New("contract_revision_stale")
		}
		// Registry requests carry only the generation reference; MCP instead retains its local runtime staleness fence.
		if c.requireGenerationPin {
			binding.RuntimeContractHash = ""
		}
		revisions[i] = sandbox.ServiceVersionRevision{
			ServiceID: binding.ServiceID, ServiceVersionID: binding.ServiceVersionID, Version: binding.Version,
			Revision: binding.Revision, SourceHash: binding.SourceHash, GenerationContractHash: binding.GenerationContractHash,
			RuntimeContractHash: binding.RuntimeContractHash,
		}
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("generation.contract_source", "local_pin"), attribute.Int("generation.contract_count", len(revisions)))
	return revisions, nil
}

// FetchServiceVersionExecutionAuthContracts adapts minimal local security metadata into the existing shared auth resolver.
func (c *generationPlanningClient) FetchServiceVersionExecutionAuthContracts(ctx context.Context, selections []sandbox.ServiceVersionExecutionAuthSelection, _ string) ([]sandbox.ServiceVersionExecutionAuthContract, error) {
	inputs := make([]store.GenerationAuthSelection, len(selections))
	for i, selection := range selections {
		inputs[i] = store.GenerationAuthSelection{ServiceID: selection.ServiceID, Version: selection.Version, OperationNames: selection.OperationNames, SelectAll: selection.SelectAll}
	}
	contracts, err := c.contracts.ListGenerationAuthContracts(ctx, inputs, c.requireGenerationPin)
	// Missing local security cannot be converted into anonymous generation authority.
	if err != nil {
		return nil, err
	}
	result := make([]sandbox.ServiceVersionExecutionAuthContract, len(contracts))
	for i, contract := range contracts {
		c.observed[contract.ServiceVersionID] = planningContractIdentity(contract.GenerationContractHash, contract.RuntimeContractHash, c.requireGenerationPin)
		result[i] = generationAuthProjection(contract)
	}
	return result, nil
}

// planningContractIdentity uses the relevant immutable authority while keeping runtime and generation hashes semantically distinct.
func planningContractIdentity(generationHash, runtimeHash string, generated bool) string {
	// SDK publication must bind the retained generator input; MCP needs only its admitted local runtime contract.
	if generated {
		return generationHash
	}
	return runtimeHash
}

// generationAuthProjection translates transport types only; the shared policy resolver remains the sole auth decision owner.
func generationAuthProjection(contract store.GenerationAuthContract) sandbox.ServiceVersionExecutionAuthContract {
	operations := make([]sandbox.OperationSecuritySummary, len(contract.Operations))
	for i, operation := range contract.Operations {
		operations[i] = sandbox.OperationSecuritySummary{Name: operation.Name, SecurityRequirements: operation.SecurityRequirements}
	}
	return sandbox.ServiceVersionExecutionAuthContract{
		ServiceID: contract.ServiceID, ServiceVersionID: contract.ServiceVersionID, Version: contract.Version,
		OperationNames: contract.OperationNames, SelectAll: contract.SelectAll, AuthConfigs: contract.AuthConfigs, Operations: operations,
	}
}

// ValidateSDKSelections leaves all operation/webhook membership predicates in the local set-based store.
func (c *generationPlanningClient) ValidateSDKSelections(ctx context.Context, selections []models.SDKSelection) error {
	return c.contracts.ValidateGenerationSelections(ctx, selections, c.requireGenerationPin)
}

// generationPinPlanError preserves the migration recovery action through older generic dependency error boundaries.
func generationPinPlanError(err error, fallback error) error {
	var planningErr workspaceConfigHTTPError
	// Inner planning stages already carry the stable recovery code and must not be flattened again.
	if errors.As(err, &planningErr) && (planningErr.code == "generation_contract_pin_unavailable" || planningErr.code == "service_provider_identity_unavailable") {
		return err
	}
	// Provider identity can be repaired by refreshing an older snapshot without implying that MCP needs a generated package.
	if errors.Is(err, store.ErrServiceProviderIdentityUnavailable) {
		return workspaceConfigHTTPError{status: http.StatusConflict, code: "service_provider_identity_unavailable", category: "dependency",
			message: "This workspace snapshot has no saved provider identity for the qualified service reference. Refresh the selected service version while it remains available in Registry, then create a new plan."}
	}
	// Only typed missing-pin failures recommend refresh; storage/auth errors retain their existing classification.
	if !errors.Is(err, store.ErrGenerationContractPinUnavailable) {
		return fallback
	}
	return workspaceConfigHTTPError{
		status: http.StatusConflict, code: "generation_contract_pin_unavailable", category: "dependency",
		message: "This workspace snapshot has no retained SDK generation contract. Refresh the selected service version while it is still available in Registry, then create a new plan.",
	}
}
