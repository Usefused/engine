package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type generationPlanningTestStore struct {
	*workspaceTestStore
	binding         models.SDKContractBinding
	pinErr          error
	validationCalls int
}

// generationPlanningRegistryTrap makes any obsolete planning dependency fail at its call boundary.
type generationPlanningRegistryTrap struct {
	sandbox.RegistryClient
	t *testing.T
}

// FetchServiceVersionExecutionAuthContracts proves auth policy is supplied only by local snapshots.
func (r *generationPlanningRegistryTrap) FetchServiceVersionExecutionAuthContracts(context.Context, []sandbox.ServiceVersionExecutionAuthSelection, string) ([]sandbox.ServiceVersionExecutionAuthContract, error) {
	r.t.Fatal("planning attempted Registry auth lookup")
	return nil, errors.New("unexpected Registry auth lookup")
}

// ValidateSDKSelections proves selected operation membership never falls back to Registry.
func (r *generationPlanningRegistryTrap) ValidateSDKSelections(context.Context, []models.SDKSelection) error {
	r.t.Fatal("planning attempted Registry selection validation")
	return errors.New("unexpected Registry selection validation")
}

// FetchServiceVersionRevisions prevents a live revision read from masking missing local identity.
func (r *generationPlanningRegistryTrap) FetchServiceVersionRevisions(context.Context, []sandbox.ServiceVersionRef, string) ([]sandbox.ServiceVersionRevision, error) {
	r.t.Fatal("planning attempted Registry revision lookup")
	return nil, errors.New("unexpected Registry revision lookup")
}

// generationPlanningTestClient centralizes capability admission so each test exercises the real constructor.
func generationPlanningTestClient(t *testing.T, s store.Store, generated bool) sandbox.RegistryClient {
	t.Helper()
	client, err := localSnapshotPlanningClient(s, &generationPlanningRegistryTrap{RegistryClient: &mockRegistryClient{}, t: t}, generated)
	// A fixture with local snapshots must be admitted before testing its revision fence.
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// ResolveGenerationServiceIDsByKeys keeps the fixture behind the same local identity boundary used by production planning.
func (s *generationPlanningTestStore) ResolveGenerationServiceIDsByKeys(ctx context.Context, keys []string) (map[string]uuid.UUID, error) {
	return s.workspaceTestStore.ResolveWorkspaceServiceIDsByKeys(ctx, keys)
}

// ListGenerationContractBindings models the authoritative local pin rather than a mutable Registry lookup.
func (s *generationPlanningTestStore) ListGenerationContractBindings(_ context.Context, _ []models.ServiceVersionRef, requirePin bool) ([]models.SDKContractBinding, error) {
	return []models.SDKContractBinding{s.binding}, s.generationStubError(requirePin)
}

// ListGenerationAuthContracts supplies one already-filtered security contract so production auth decisions remain exercised.
func (s *generationPlanningTestStore) ListGenerationAuthContracts(_ context.Context, selections []store.GenerationAuthSelection, requirePin bool) ([]store.GenerationAuthContract, error) {
	result := make([]store.GenerationAuthContract, len(selections))
	for i, selection := range selections {
		result[i] = store.GenerationAuthContract{GenerationAuthSelection: selection, ServiceVersionID: s.binding.ServiceVersionID,
			GenerationContractHash: s.binding.GenerationContractHash, RuntimeContractHash: s.binding.RuntimeContractHash, Operations: []store.GenerationOperationSecurity{{Name: "listLogEvents", SecurityRequirements: authrouting.Requirements{{}}}}}
	}
	return result, s.generationStubError(requirePin)
}

// ValidateGenerationSelections records the shared planner's call to the local membership boundary.
func (s *generationPlanningTestStore) ValidateGenerationSelections(_ context.Context, _ []models.SDKSelection, requirePin bool) error {
	s.validationCalls++
	return s.generationStubError(requirePin)
}

// generationStubError models the production distinction between missing SDK archive pins and valid local-only MCP snapshots.
func (s *generationPlanningTestStore) generationStubError(requirePin bool) error {
	// Ordinary persistence failures remain failures for both adapters.
	if s.pinErr != nil {
		return s.pinErr
	}
	// Only remote code generation requires an archived provider contract.
	if requirePin && !store.ValidGenerationContractHash(s.binding.GenerationContractHash) {
		return store.ErrGenerationContractPinUnavailable
	}
	return nil
}

// newGenerationPlanningTestStore builds a valid activated service whose archived generation pin is independent of Registry visibility.
func newGenerationPlanningTestStore() *generationPlanningTestStore {
	serviceID, versionID := uuid.New(), uuid.New()
	return &generationPlanningTestStore{
		workspaceTestStore: &workspaceTestStore{accountID: uuid.New(),
			workspaceServices:        []store.WorkspaceService{{ServiceID: serviceID, ServiceName: "Saved service", ServiceSlug: "saved-service", Version: "1.0"}},
			workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{serviceID: {{ServiceID: serviceID, Version: "1.0", ServiceVersionID: versionID}}}},
		binding: models.SDKContractBinding{ServiceID: serviceID, ServiceVersionID: versionID, Version: "1.0", Revision: 7,
			SourceHash: "source-7", GenerationContractHash: "sha256:" + strings.Repeat("a", 64)},
	}
}

// planWithGenerationTestStore exercises generated or direct API planning while trapping every obsolete Registry read.
func planWithGenerationTestStore(t *testing.T, s *generationPlanningTestStore, generate *bool) (*httptest.ResponseRecorder, *mockConfigStore) {
	t.Helper()
	configs := &mockConfigStore{}
	router := newControlTestRouter(s.accountID)
	router.Post("/sdk-config/plan", SDKConfigPlanHandler(configs, s, &generationPlanningRegistryTrap{RegistryClient: &mockRegistryClient{}, t: t}))
	config := map[string]any{"apiVersion": "fused/v1", "kind": "sdk", "name": "retained", "version": "1.0.0", "language": "typescript", "bucket": "default",
		"services": map[string]any{"Saved service": map[string]any{"version": "1.0", "operations": []string{"listLogEvents"}}}}
	// Omission preserves package generation, while an explicit value exercises the public generate policy exactly.
	if generate != nil {
		config["generate"] = *generate
	}
	body, err := json.Marshal(map[string]any{"source_hash": "config-hash", "owner_team": "platform", "config_key": "sdk:retained:1.0.0", "config": config})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/sdk-config/plan", bytes.NewReader(body))
	request.Header.Set("X-API-Key", "fsk_test")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response, configs
}

// TestSDKPlanUsesRetainedLocalGenerationPin proves deleted Registry metadata is unnecessary for the existing plan path.
func TestSDKPlanUsesRetainedLocalGenerationPin(t *testing.T) {
	s := newGenerationPlanningTestStore()
	s.binding.RuntimeContractHash = "sha256:" + strings.Repeat("f", 64)
	response, configs := planWithGenerationTestStore(t, s, nil)
	// A successful plan must use local validation even though Registry has no service data.
	if response.Code != http.StatusOK || s.validationCalls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, s.validationCalls, response.Body)
	}
	var request models.SDKGenerationRequest
	// Read the existing resolved payload rather than introducing a second generation request shape.
	if err := json.Unmarshal(configs.createdPlan.ResolvedPayload, &request); err != nil {
		t.Fatal(err)
	}
	// Full schemas must never be required in the outbound generation payload.
	expected := s.binding
	expected.RuntimeContractHash = ""
	if len(request.ContractBindings) != 1 || request.ContractBindings[0] != expected {
		t.Fatalf("bindings=%+v", request.ContractBindings)
	}
	// The compact object reference is sufficient; runtime schemas remain Engine-local.
	if bytes.Contains(configs.createdPlan.ResolvedPayload, []byte("schema_definitions")) || bytes.Contains(configs.createdPlan.ResolvedPayload, []byte("runtime_contract_hash")) {
		t.Fatal("generation payload contains schema definitions")
	}
}

// TestDirectAPIPlanUsesUnpinnedLocalSnapshot proves generate:false never requires Registry package authority.
func TestDirectAPIPlanUsesUnpinnedLocalSnapshot(t *testing.T) {
	s := newGenerationPlanningTestStore()
	s.binding.GenerationContractHash = ""
	s.binding.RuntimeContractHash = "sha256:" + strings.Repeat("d", 64)
	generate := false
	response, configs := planWithGenerationTestStore(t, s, &generate)
	// Direct API execution is fully admitted by the local runtime snapshot even when no package archive exists.
	if response.Code != http.StatusOK || configs.createdPlan == nil {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	var request models.SDKGenerationRequest
	if err := json.Unmarshal(configs.createdPlan.ResolvedPayload, &request); err != nil {
		t.Fatal(err)
	}
	// The plan retains a runtime staleness fence and its explicit no-package policy without inventing a generation hash.
	if !request.SkipPackaging || len(request.ContractBindings) != 1 || request.ContractBindings[0].RuntimeContractHash != s.binding.RuntimeContractHash || request.ContractBindings[0].GenerationContractHash != "" {
		t.Fatalf("request=%+v", request)
	}
}

// TestSDKGenerationPayloadHidesCredentialSourceBindings proves auth-source fences stay Engine-private while generation receives targets only.
func TestSDKGenerationPayloadHidesCredentialSourceBindings(t *testing.T) {
	targetID, sourceID := uuid.New(), uuid.New()
	targetBinding := models.SDKContractBinding{ServiceID: targetID, ServiceVersionID: uuid.New(), Version: "1.0", Revision: 2, SourceHash: "target-hash"}
	sourceBinding := models.SDKContractBinding{ServiceID: sourceID, ServiceVersionID: uuid.New(), Version: "2.0", Revision: 4, SourceHash: "source-hash"}
	payload := resolvedSDKPayload(GenerateSDKRequest{
		Selections:       []models.SDKSelection{{ServiceID: targetID, ServiceVersionID: targetBinding.ServiceVersionID}},
		ContractBindings: []models.SDKContractBinding{targetBinding},
	}, uuid.New(), uuid.New(), false)
	payload.CredentialSourceBindings = []models.SDKContractBinding{sourceBinding}
	raw, err := json.Marshal(payload)
	// The immutable Engine plan must retain both independent revision fences.
	if err != nil {
		t.Fatal(err)
	}
	assertAppPayloadContractFences(t, raw, targetBinding, sourceBinding)
	assertSDKGenerationRequestExcludesSource(t, raw, targetBinding)
}

// assertAppPayloadContractFences verifies target and credential-source revisions share the Engine concurrency boundary.
func assertAppPayloadContractFences(t *testing.T, raw json.RawMessage, targetBinding, sourceBinding models.SDKContractBinding) {
	t.Helper()
	bindings, err := sdkContractBindingsFromPayload(raw)
	// Concurrency validation must include the metadata-only auth source.
	if err != nil || len(bindings) != 2 || bindings[0] != targetBinding || bindings[1] != sourceBinding {
		t.Fatalf("bindings=%+v err=%v", bindings, err)
	}
}

// assertSDKGenerationRequestExcludesSource verifies Registry receives a one-to-one target generation contract.
func assertSDKGenerationRequestExcludesSource(t *testing.T, raw json.RawMessage, targetBinding models.SDKContractBinding) {
	t.Helper()
	generationPayload, err := sdkGenerationPayloadForPlan(raw, sdkApplyCall{planID: uuid.New()}, uuid.New(), uuid.New(), "config-hash")
	// Generation serialization must discard Engine-private credential-source bindings.
	if err != nil {
		t.Fatal(err)
	}
	var request models.SDKGenerationRequest
	// Decode the actual Registry request shape to assert its one-to-one target contract.
	if err := json.Unmarshal(generationPayload, &request); err != nil {
		t.Fatal(err)
	}
	// Registry rejects extra bindings because source-only services expose no generated operations.
	if len(request.Selections) != 1 || len(request.ContractBindings) != 1 || request.ContractBindings[0] != targetBinding {
		t.Fatalf("generation request=%+v", request)
	}
	// The private payload field must not leak as an unknown extension to Registry.
	if bytes.Contains(generationPayload, []byte("credential_source_bindings")) {
		t.Fatalf("generation payload leaked credential source bindings: %s", generationPayload)
	}
}

// TestSplitAppContractBindingsDeduplicatesSelectedSources keeps a dual-role service in the generated target set only.
func TestSplitAppContractBindingsDeduplicatesSelectedSources(t *testing.T) {
	serviceID := uuid.New()
	target := models.SDKContractBinding{ServiceID: serviceID, ServiceVersionID: uuid.New(), Version: "1.0"}
	source := models.SDKContractBinding{ServiceID: serviceID, ServiceVersionID: uuid.New(), Version: "2.0"}
	targets, sources := splitAppContractBindings(
		[]models.SDKContractBinding{target, source, target},
		[]sdkResolvedService{{ServiceID: serviceID, ServiceVersionID: target.ServiceVersionID, Version: "1.0"}},
	)
	// A duplicated dual-role target must not inflate Registry's binding count.
	if len(targets) != 1 || targets[0] != target {
		t.Fatalf("target bindings=%+v", targets)
	}
	// A source-only service remains available solely as an Engine-side fence.
	if len(sources) != 1 || sources[0] != source {
		t.Fatalf("source bindings=%+v", sources)
	}
}

// TestResolveSDKContractBindingsKeepsSiblingVersions proves one service's target and source versions cannot overwrite each other.
func TestResolveSDKContractBindingsKeepsSiblingVersions(t *testing.T) {
	serviceID, targetVersionID, sourceVersionID := uuid.New(), uuid.New(), uuid.New()
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|1.0": {ServiceID: serviceID, ServiceVersionID: targetVersionID, Version: "1.0"},
		serviceID.String() + "|2.0": {ServiceID: serviceID, ServiceVersionID: sourceVersionID, Version: "2.0"},
	}}
	bindings, err := resolveSDKContractBindings(context.Background(), registry, "", []sdkResolvedService{
		{ServiceID: serviceID, ServiceVersionID: targetVersionID, Version: "1.0"},
		{ServiceID: serviceID, ServiceVersionID: sourceVersionID, Version: "2.0"},
	})
	// Both exact versions must survive the one batched revision response.
	if err != nil || len(bindings) != 2 {
		t.Fatalf("bindings=%+v err=%v", bindings, err)
	}
	// Ordered results must preserve the target/source identities supplied by the planner.
	if bindings[0].ServiceVersionID != targetVersionID || bindings[1].ServiceVersionID != sourceVersionID {
		t.Fatalf("bindings=%+v", bindings)
	}
}

// TestMCPPlanUsesUnpinnedLocalSnapshot proves local-only apps remain independent of Registry archival availability.
func TestMCPPlanUsesUnpinnedLocalSnapshot(t *testing.T) {
	s := newGenerationPlanningTestStore()
	s.binding.GenerationContractHash = ""
	s.binding.RuntimeContractHash = "sha256:" + strings.Repeat("d", 64)
	configs := &mockConfigStore{}
	router := newControlTestRouter(s.accountID)
	router.Post("/mcp-config/plan", MCPConfigPlanHandler(configs, s, &generationPlanningRegistryTrap{RegistryClient: &mockRegistryClient{}, t: t}))
	body := []byte(`{"source_hash":"config-hash","owner_team":"platform","config_key":"mcp:retained:1.0.0","config":{"apiVersion":"fused/v1","kind":"mcp","name":"retained","version":"1.0.0","description":"Review retained service activity.","bucket":"default","services":{"Saved service":{"version":"1.0","operations":["listLogEvents"]}}}}`)
	request := httptest.NewRequest(http.MethodPost, "/mcp-config/plan", bytes.NewReader(body))
	request.Header.Set("X-API-Key", "fsk_test")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	// No Registry service metadata or generation pin is needed for an existing admitted MCP snapshot.
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	var payload appResolvedPayload
	// MCP retains the same shared plan schema rather than inventing a second persistence format.
	if err := json.Unmarshal(configs.createdPlan.ResolvedPayload, &payload); err != nil {
		t.Fatal(err)
	}
	// Its optimistic-concurrency boundary uses the runtime hash even for old revision-zero snapshots.
	if len(payload.ContractBindings) != 1 || payload.ContractBindings[0].RuntimeContractHash != s.binding.RuntimeContractHash {
		t.Fatalf("bindings=%+v", payload.ContractBindings)
	}
	s.binding.RuntimeContractHash = "sha256:" + strings.Repeat("e", 64)
	err := ensureSDKContractBindingsCurrent(context.Background(), generationPlanningTestClient(t, s, false), "", payload.ContractBindings)
	// A later local refresh invalidates the MCP plan without depending on Registry revisions.
	if err == nil || err.Error() != "contract_revision_stale" {
		t.Fatalf("error=%v", err)
	}
}

// TestSDKPlanMissingGenerationPinIsActionable requires explicit refresh instead of inventing a live Registry fallback.
func TestSDKPlanMissingGenerationPinIsActionable(t *testing.T) {
	s := newGenerationPlanningTestStore()
	s.pinErr = store.ErrGenerationContractPinUnavailable
	response, configs := planWithGenerationTestStore(t, s, nil)
	// No incomplete plan may become a recovery or generation authority.
	if response.Code != http.StatusConflict || configs.createdPlan != nil {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	// Stable code and recovery text must survive the older auth error wrapper.
	if !strings.Contains(response.Body.String(), "generation_contract_pin_unavailable") || !strings.Contains(response.Body.String(), "Refresh") {
		t.Fatalf("error=%s", response.Body)
	}
}

// TestSDKPlanningAllowsRuntimeOnlyCredentialSource proves a source service does not need an unused generator archive.
func TestSDKPlanningAllowsRuntimeOnlyCredentialSource(t *testing.T) {
	s := newGenerationPlanningTestStore()
	s.binding.GenerationContractHash = ""
	s.binding.RuntimeContractHash = "sha256:" + strings.Repeat("b", 64)
	client := generationPlanningTestClient(t, s, true).(*generationPlanningClient)
	client.setGenerationTargets([]sdkResolvedService{{ServiceID: uuid.New(), Version: "1.0"}})
	selection := sandbox.ServiceVersionExecutionAuthSelection{ServiceID: s.binding.ServiceID, Version: s.binding.Version}
	_, err := client.FetchServiceVersionExecutionAuthContracts(context.Background(), []sandbox.ServiceVersionExecutionAuthSelection{selection}, "")
	// Source auth metadata must remain usable from its admitted runtime snapshot.
	if err != nil {
		t.Fatalf("source auth contract: %v", err)
	}
	revisions, err := client.FetchServiceVersionRevisions(context.Background(), []sandbox.ServiceVersionRef{{ServiceID: s.binding.ServiceID, Version: s.binding.Version}}, "")
	// The source fence must retain its runtime hash instead of pretending to be generator input.
	if err != nil || len(revisions) != 1 || revisions[0].RuntimeContractHash != s.binding.RuntimeContractHash {
		t.Fatalf("source revisions=%+v err=%v", revisions, err)
	}
}

// TestGenerationPlanningDetectsRefreshBetweenAuthAndBinding rejects mixed-revision plans without comparing full schemas in memory.
func TestGenerationPlanningDetectsRefreshBetweenAuthAndBinding(t *testing.T) {
	s := newGenerationPlanningTestStore()
	client := generationPlanningTestClient(t, s, true).(*generationPlanningClient)
	_, err := client.FetchServiceVersionExecutionAuthContracts(context.Background(), []sandbox.ServiceVersionExecutionAuthSelection{{ServiceID: s.binding.ServiceID, Version: "1.0"}}, "")
	// The first read must establish a valid observed contract before simulating refresh.
	if err != nil {
		t.Fatal(err)
	}
	s.binding.GenerationContractHash = "sha256:" + strings.Repeat("b", 64)
	_, err = client.FetchServiceVersionRevisions(context.Background(), []models.ServiceVersionRef{{ServiceID: s.binding.ServiceID, Version: "1.0"}}, "")
	// A same-label refresh changes immutable generation input even if source hashes happen to match.
	if err == nil || err.Error() != "contract_revision_stale" {
		t.Fatalf("error=%v", err)
	}
}

// TestGenerationBindingHashParticipatesInApplyFence keeps the existing revision checks authoritative for retained object references.
func TestGenerationBindingHashParticipatesInApplyFence(t *testing.T) {
	s := newGenerationPlanningTestStore()
	client := generationPlanningTestClient(t, s, true)
	planned := s.binding
	s.binding.GenerationContractHash = "sha256:" + strings.Repeat("b", 64)
	err := ensureSDKContractBindingsCurrent(context.Background(), client, "", []sdkContractBinding{planned})
	// Changing only the bundle must still invalidate a pre-existing plan.
	if err == nil || err.Error() != "contract_revision_stale" {
		t.Fatalf("error=%v", err)
	}
	s.pinErr = store.ErrGenerationContractPinUnavailable
	err = ensureSDKContractBindingsCurrent(context.Background(), client, "", []sdkContractBinding{planned})
	var httpErr workspaceConfigHTTPError
	// Missing-pin recovery keeps the same structured code through apply's dependency layer.
	if !errors.As(err, &httpErr) || httpErr.code != "generation_contract_pin_unavailable" {
		t.Fatalf("error=%v", err)
	}
}

// TestLocalPlanningRejectsMissingSnapshotCapability proves neither adapter can revive removed Registry planning methods.
func TestLocalPlanningRejectsMissingSnapshotCapability(t *testing.T) {
	for _, generated := range []bool{false, true} {
		// A zero-value HTTP client would fail or panic if construction attempted any network lookup.
		client, err := localSnapshotPlanningClient(&workspaceTestStore{}, &sandbox.HTTPRegistryClient{}, generated)
		var planningErr workspaceConfigHTTPError
		// Capability rejection precedes all lookups and keeps the stable actionable dependency contract.
		if client != nil || !errors.As(err, &planningErr) || planningErr.code != "local_contract_store_unavailable" {
			t.Fatalf("generated=%t client=%T error=%v", generated, client, err)
		}
	}
}
