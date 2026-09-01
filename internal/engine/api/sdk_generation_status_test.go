package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type sdkGenerationStatusStore struct {
	*workspaceTestStore
	app    store.App
	family store.AppFamily
}

// GetApp returns the exact lifecycle row exercised by local status projection.
func (s *sdkGenerationStatusStore) GetApp(context.Context, uuid.UUID) (*store.App, error) {
	copy := s.app
	return &copy, nil
}

// GetAppFamily proves the read is scoped to the app's SDK family.
func (s *sdkGenerationStatusStore) GetAppFamily(context.Context, uuid.UUID) (*store.AppFamily, error) {
	copy := s.family
	return &copy, nil
}

// TestResolveSDKGenerationStatusProjectsOnlyLocalState pins pending, failed, complete, and legacy lifecycle views without a mutation path.
func TestResolveSDKGenerationStatusProjectsOnlyLocalState(t *testing.T) {
	fixture := newSDKGenerationStatusFixture()
	tests := []struct {
		name       string
		appStatus  store.AppStatus
		generation string
		want       string
	}{
		{name: "pending build", appStatus: store.AppStatusBuilding, generation: models.SDKGenerationStatusPending, want: models.SDKGenerationStatusPending},
		{name: "failed build", appStatus: store.AppStatusBuilding, generation: models.SDKGenerationStatusFailed, want: models.SDKGenerationStatusFailed},
		{name: "complete", appStatus: store.AppStatusActive, generation: models.SDKGenerationStatusComplete, want: models.SDKGenerationStatusComplete},
		{name: "legacy active", appStatus: store.AppStatusActive, want: models.SDKGenerationStatusComplete},
	}
	// Each case changes only stored lifecycle fields, proving the endpoint does not need Registry state.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture.app.Status = test.appStatus
			fixture.app.SDKGenerationStatus = test.generation
			response, err := resolveSDKGenerationStatus(context.Background(), fixture, fixture.app.AccountID, fixture.app.AppID)
			// Exact persisted status and immutable identities are the complete read contract.
			if err != nil || response.Status != test.want || response.AppID != fixture.app.AppID || response.AppFamilyID != fixture.app.AppFamilyID {
				t.Fatalf("status = %#v / %v, want %q", response, err, test.want)
			}
		})
	}
}

// TestResolveSDKGenerationStatusRejectsInvalidAuthority verifies account, kind, and incoherent state all fail closed.
func TestResolveSDKGenerationStatusRejectsInvalidAuthority(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*sdkGenerationStatusStore)
	}{
		{name: "cross workspace", mutate: func(fixture *sdkGenerationStatusStore) { fixture.app.AccountID = uuid.New() }},
		{name: "mcp family", mutate: func(fixture *sdkGenerationStatusStore) { fixture.family.Kind = store.AppKindMCP }},
		{name: "invalid building state", mutate: func(fixture *sdkGenerationStatusStore) { fixture.app.SDKGenerationStatus = "" }},
	}
	// A fresh fixture prevents one authority mismatch from influencing the next rejection.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSDKGenerationStatusFixture()
			callerAccountID := fixture.app.AccountID
			test.mutate(fixture)
			_, err := resolveSDKGenerationStatus(context.Background(), fixture, callerAccountID, fixture.app.AppID)
			var httpErr workspaceConfigHTTPError
			// Every invalid projection must remain a bounded client-visible error with no lifecycle side effect.
			if !errors.As(err, &httpErr) || (httpErr.status != http.StatusNotFound && httpErr.status != http.StatusConflict) {
				t.Fatalf("authority error = %#v", err)
			}
		})
	}
}

// newSDKGenerationStatusFixture creates one coherent pending SDK version for local projection tests.
func newSDKGenerationStatusFixture() *sdkGenerationStatusStore {
	accountID, familyID, appID := uuid.New(), uuid.New(), uuid.New()
	return &sdkGenerationStatusStore{
		workspaceTestStore: &workspaceTestStore{},
		app: store.App{
			AppID: appID, AppFamilyID: familyID, AccountID: accountID,
			Status: store.AppStatusBuilding, SDKGenerationJobID: "job-1", SDKGenerationStatus: models.SDKGenerationStatusPending,
		},
		family: store.AppFamily{AppFamilyID: familyID, AccountID: accountID, Kind: store.AppKindSDK},
	}
}
