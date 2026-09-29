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
	pinWriter            store.GenerationPinWriter
	observed             map[uuid.UUID]string
	requireGenerationPin bool
	generationTargets    map[string]bool
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
	pinWriter, _ := s.(store.GenerationPinWriter)
	return &generationPlanningClient{
		RegistryClient: registry, contracts: contracts, observed: make(map[uuid.UUID]string),
		pinWriter: pinWriter, requireGenerationPin: requireGenerationPin, generationTargets: make(map[string]bool),
	}, nil
}

// ensureGenerationPins acquires SDK archives only for generated targets missing from active runtime snapshots.
func (c *generationPlanningClient) ensureGenerationPins(ctx context.Context, refs []sandbox.ServiceVersionRef, apiKey string) error {
	// MCP and metadata-only planning keep using the admitted local execution snapshot without Registry storage work.
	if !c.requireGenerationPin || len(refs) == 0 {
		return nil
	}
	bindings, err := c.contracts.ListGenerationContractBindings(ctx, refs, false)
	// Missing local authority cannot be reconstructed from an unscoped Registry lookup.
	if err != nil {
		return err
	}
	missing := make([]store.WorkspaceServiceVersion, 0, len(bindings))
	byVersion := make(map[uuid.UUID]models.SDKContractBinding, len(bindings))
	for _, binding := range bindings {
		// Existing pins remain immutable; credential-source versions do not consume generated package bytes.
		if !c.requiresGenerationPin(binding.ServiceID, binding.Version) || store.ValidGenerationContractHash(binding.GenerationContractHash) {
			continue
		}
		// Several auth selections may reference one service version; acquire its immutable archive once.
		if _, alreadyQueued := byVersion[binding.ServiceVersionID]; alreadyQueued {
			continue
		}
		missing = append(missing, store.WorkspaceServiceVersion{ServiceID: binding.ServiceID, ServiceVersionID: binding.ServiceVersionID, Version: binding.Version})
		byVersion[binding.ServiceVersionID] = binding
	}
	// A fully pinned selection requires no Registry contact during plan or apply.
	if len(missing) == 0 {
		return nil
	}
	fetcher, canFetch := c.RegistryClient.(BatchRuntimeContractFetcher)
	// The production client must be able to acquire a durable archive before local planning can use its hash.
	if !canFetch || c.pinWriter == nil {
		return store.ErrGenerationContractPinUnavailable
	}
	snapshots, err := fetcher.FetchRuntimeContracts(ctx, missing, apiKey)
	// Partial archive acquisition cannot authorize any local pin attachment.
	if err != nil {
		return err
	}
	// Every requested version must return an admitted snapshot before any local archive reference is attached.
	if len(snapshots) != len(missing) {
		return store.ErrGenerationContractPinUnavailable
	}
	for _, snapshot := range snapshots {
		binding, found := byVersion[snapshot.ServiceVersionID]
		// Registry bytes may be attached only to the unchanged local execution snapshot and exact source revision.
		if !found || snapshot.ServiceID != binding.ServiceID || snapshot.Version != binding.Version ||
			snapshot.Revision != binding.Revision || snapshot.SourceHash != binding.SourceHash ||
			snapshot.ContractHash != binding.RuntimeContractHash || !store.ValidGenerationContractHash(snapshot.GenerationContractHash) {
			return store.ErrGenerationContractPinUnavailable
		}
		// The store rechecks the same identity atomically against concurrent refresh or removal.
		if err := c.pinWriter.AttachGenerationContractPin(ctx, binding, snapshot.GenerationContractHash); err != nil {
			return err
		}
	}
	return nil
}

// setGenerationTargets records exact generated service versions so metadata-only auth sources need no archive pin.
func (c *generationPlanningClient) setGenerationTargets(services []sdkResolvedService) {
	for _, service := range services {
		c.generationTargets[generationPlanningRefKey(service.ServiceID, service.Version)] = true
	}
}

// setGenerationTargetBindings restores exact generated targets when a persisted plan is revalidated during apply.
func (c *generationPlanningClient) setGenerationTargetBindings(bindings []sdkContractBinding) {
	for _, binding := range bindings {
		c.generationTargets[generationPlanningRefKey(binding.ServiceID, binding.Version)] = true
	}
}

// requiresGenerationPin returns true only for exact service versions exposed through generated SDK selections.
func (c *generationPlanningClient) requiresGenerationPin(serviceID uuid.UUID, version string) bool {
	// SDK callers that have not yet supplied a target partition retain the historical fail-closed archive requirement.
	if c.requireGenerationPin && len(c.generationTargets) == 0 {
		return true
	}
	return c.requireGenerationPin && c.generationTargets[generationPlanningRefKey(serviceID, version)]
}

// generationPlanningRefKey prevents two versions of one service from sharing generation authority.
func generationPlanningRefKey(serviceID uuid.UUID, version string) string {
	return serviceID.String() + "\x00" + version
}

// requireLocalGenerationPin enforces archive identity only where a generated target actually consumes it.
func requireLocalGenerationPin(hash string, required bool) error {
	// Runtime-only credential sources remain valid without a Registry generation archive.
	if required && !store.ValidGenerationContractHash(hash) {
		return store.ErrGenerationContractPinUnavailable
	}
	return nil
}

// localPlanningUnavailableError distinguishes absent storage support from an individual refreshable SDK pin.
func localPlanningUnavailableError() error {
	return workspaceConfigHTTPError{status: http.StatusServiceUnavailable, code: "local_contract_store_unavailable", category: "dependency",
		message: "Local service contract storage is unavailable. Restart Engine with a supported snapshot store, then create a new plan."}
}

// FetchServiceVersionRevisions keeps the existing before/after-generation checks tied to the local pin instead of current Registry visibility.
func (c *generationPlanningClient) FetchServiceVersionRevisions(ctx context.Context, refs []sandbox.ServiceVersionRef, apiKey string) ([]sandbox.ServiceVersionRevision, error) {
	// SDK planning acquires a missing archive before its existing local-only revision fence reads the pin.
	if err := c.ensureGenerationPins(ctx, refs, apiKey); err != nil {
		return nil, err
	}
	bindings, err := c.contracts.ListGenerationContractBindings(ctx, refs, false)
	// A missing pin is actionable; network fallback would silently select a different contract.
	if err != nil {
		return nil, err
	}
	revisions := make([]sandbox.ServiceVersionRevision, len(bindings))
	for i, binding := range bindings {
		generated := c.requiresGenerationPin(binding.ServiceID, binding.Version)
		// Only generated targets require an archived provider contract; auth-source metadata executes locally.
		if err := requireLocalGenerationPin(binding.GenerationContractHash, generated); err != nil {
			return nil, err
		}
		// A concurrent refresh between auth planning and binding cannot mix two revisions in one plan.
		if hash := c.observed[binding.ServiceVersionID]; hash != "" && hash != planningContractIdentity(binding.GenerationContractHash, binding.RuntimeContractHash, generated) {
			return nil, errors.New("contract_revision_stale")
		}
		// Registry requests carry only the generation reference; MCP instead retains its local runtime staleness fence.
		if generated {
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
func (c *generationPlanningClient) FetchServiceVersionExecutionAuthContracts(ctx context.Context, selections []sandbox.ServiceVersionExecutionAuthSelection, apiKey string) ([]sandbox.ServiceVersionExecutionAuthContract, error) {
	refs := make([]sandbox.ServiceVersionRef, 0, len(selections))
	for _, selection := range selections {
		refs = append(refs, sandbox.ServiceVersionRef{ServiceID: selection.ServiceID, Version: selection.Version})
	}
	// Auth planning must use the same archived identity as operation selection and the final SDK package.
	if err := c.ensureGenerationPins(ctx, refs, apiKey); err != nil {
		return nil, err
	}
	inputs := make([]store.GenerationAuthSelection, len(selections))
	for i, selection := range selections {
		inputs[i] = store.GenerationAuthSelection{ServiceID: selection.ServiceID, Version: selection.Version, OperationNames: selection.OperationNames, SelectAll: selection.SelectAll}
	}
	contracts, err := c.contracts.ListGenerationAuthContracts(ctx, inputs, false)
	// Missing local security cannot be converted into anonymous generation authority.
	if err != nil {
		return nil, err
	}
	result := make([]sandbox.ServiceVersionExecutionAuthContract, len(contracts))
	for i, contract := range contracts {
		generated := c.requiresGenerationPin(contract.ServiceID, contract.Version)
		// Source-only security metadata does not become SDK generator input.
		if err := requireLocalGenerationPin(contract.GenerationContractHash, generated); err != nil {
			return nil, err
		}
		c.observed[contract.ServiceVersionID] = planningContractIdentity(contract.GenerationContractHash, contract.RuntimeContractHash, generated)
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
	refs := make([]sandbox.ServiceVersionRef, 0, len(selections))
	for _, selection := range selections {
		refs = append(refs, sandbox.ServiceVersionRef{ServiceID: selection.ServiceID, Version: selection.ServiceVersionID.String()})
	}
	// A plain workspace activation stays fast; the first SDK selection is the point that acquires its generator archive.
	if err := c.ensureGenerationPins(ctx, refs, ""); err != nil {
		return err
	}
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
