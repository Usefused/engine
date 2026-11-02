package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// deploymentSelectionRepository exposes only immutable app lookup and local endpoint resolution to this fixture.
type deploymentSelectionRepository struct {
	store.Store
	app      *store.App
	resolved []models.SDKSelection
	calls    int
}

// GetApp returns the original consent snapshot rather than a current name/version lookup.
func (repository *deploymentSelectionRepository) GetApp(context.Context, uuid.UUID) (*store.App, error) {
	return repository.app, nil
}

// ResolveGenerationSelections models concrete local IDs while recording that invalid bindings fail earlier.
func (repository *deploymentSelectionRepository) ResolveGenerationSelections(_ context.Context, selections []models.SDKSelection) ([]models.SDKSelection, error) {
	repository.calls++
	return repository.resolved, nil
}

// deploymentSelectionFixture carries the original approved hashes and a narrower next release under identical labels.
func deploymentSelectionFixture(t *testing.T) (*deploymentSelectionRepository, appResolvedPayload, store.OAuthRuntimeGrant) {
	t.Helper()
	service, version, first, second := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	binding := models.SDKContractBinding{ServiceID: service, ServiceVersionID: version, Version: "v1", Revision: 1, SourceHash: "sha256:source", RuntimeContractHash: "sha256:runtime"}
	approved := []models.SDKSelection{{ServiceID: service, ServiceVersionID: version, EndpointIDs: []uuid.UUID{first, second}, OperationNames: []string{"Read", "Write"}}}
	raw, err := json.Marshal(approved)
	require.NoError(t, err)
	app := &store.App{AppID: uuid.New(), AppFamilyID: uuid.New(), AccountID: uuid.New(), Selections: raw}
	next := models.SDKSelection{ServiceID: service, ServiceVersionID: version, EndpointIDs: []uuid.UUID{first}, OperationNames: []string{"Read"}}
	repository := &deploymentSelectionRepository{app: app, resolved: []models.SDKSelection{next}}
	payload := appResolvedPayload{Selections: []models.SDKSelection{next}, ContractBindings: []models.SDKContractBinding{binding}}
	ceiling := store.OAuthRuntimeGrant{AppID: app.AppID, AppFamilyID: app.AppFamilyID, ContractBindings: []models.SDKContractBinding{binding}}
	return repository, payload, ceiling
}

// TestDeploymentPlanAcceptsPinnedSubset retains previously reviewed executable IDs while allowing a smaller release.
func TestDeploymentPlanAcceptsPinnedSubset(t *testing.T) {
	repository, payload, ceiling := deploymentSelectionFixture(t)
	require.NoError(t, validateDeploymentPlan(t.Context(), repository, repository.app.AccountID, payload, &ceiling))
	require.Equal(t, 1, repository.calls)
}

// TestDeploymentPlanRejectsReusedLabels rejects provider identity and content changes before any apply may run.
func TestDeploymentPlanRejectsReusedLabels(t *testing.T) {
	cases := []struct {
		name   string
		change func(*deploymentSelectionRepository, *appResolvedPayload)
	}{
		{"service identity", func(_ *deploymentSelectionRepository, payload *appResolvedPayload) {
			payload.ContractBindings[0].ServiceID = uuid.New()
		}},
		{"version identity", func(_ *deploymentSelectionRepository, payload *appResolvedPayload) {
			payload.ContractBindings[0].ServiceVersionID = uuid.New()
		}},
		{"source schema hash", func(_ *deploymentSelectionRepository, payload *appResolvedPayload) {
			payload.ContractBindings[0].SourceHash = "sha256:changed"
		}},
		{"runtime schema hash", func(_ *deploymentSelectionRepository, payload *appResolvedPayload) {
			payload.ContractBindings[0].RuntimeContractHash = "sha256:changed"
		}},
		{"same labels new endpoint", func(repository *deploymentSelectionRepository, _ *appResolvedPayload) {
			repository.resolved[0].EndpointIDs = []uuid.UUID{uuid.New()}
		}},
		{"resolver substituted version", func(repository *deploymentSelectionRepository, _ *appResolvedPayload) {
			repository.resolved[0].ServiceVersionID = uuid.New()
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			repository, payload, ceiling := deploymentSelectionFixture(t)
			test.change(repository, &payload)
			// Human-readable labels deliberately remain unchanged to exercise the backend immutable fence.
			require.Equal(t, "v1", payload.ContractBindings[0].Version)
			require.Equal(t, []string{"Read"}, payload.Selections[0].OperationNames)
			require.Error(t, validateDeploymentPlan(t.Context(), repository, repository.app.AccountID, payload, &ceiling))
		})
	}
}

// TestDeploymentPlanRejectsMissingOrCrossAppCeilings prevents incomplete historical metadata from broadening authority.
func TestDeploymentPlanRejectsMissingOrCrossAppCeilings(t *testing.T) {
	repository, payload, ceiling := deploymentSelectionFixture(t)
	ceiling.ContractBindings = nil
	require.Error(t, validateDeploymentPlan(t.Context(), repository, repository.app.AccountID, payload, &ceiling))
	require.Zero(t, repository.calls)
	repository, payload, ceiling = deploymentSelectionFixture(t)
	ceiling.AppFamilyID = uuid.New()
	require.Error(t, validateDeploymentPlan(t.Context(), repository, repository.app.AccountID, payload, &ceiling))
	require.Zero(t, repository.calls)
	repository, payload, ceiling = deploymentSelectionFixture(t)
	require.Error(t, validateDeploymentPlan(t.Context(), repository, uuid.New(), payload, &ceiling))
	require.Zero(t, repository.calls)
}

// TestDeploymentBindingCeilingIncludesCredentialSources prevents metadata-only credential sources from being substituted.
func TestDeploymentBindingCeilingIncludesCredentialSources(t *testing.T) {
	repository, payload, ceiling := deploymentSelectionFixture(t)
	payload.CredentialSourceBindings = []models.SDKContractBinding{{ServiceID: uuid.New(), ServiceVersionID: uuid.New(), Version: "v1", Revision: 1, SourceHash: "sha256:source", RuntimeContractHash: "sha256:runtime"}}
	require.Error(t, validateDeploymentPlan(t.Context(), repository, repository.app.AccountID, payload, &ceiling))
	// Explicitly approved unchanged credential sources remain valid even for a narrowed operation subset.
	ceiling.ContractBindings = append(ceiling.ContractBindings, payload.CredentialSourceBindings...)
	require.NoError(t, validateDeploymentPlan(t.Context(), repository, repository.app.AccountID, payload, &ceiling))
}
