package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type mockRegistryClient struct {
	shouldFail          bool
	fusedObj            *fusedobject.ServiceMetadata
	fetchCount          int
	fetchedVersions     []string
	endpointFetchCount  int // incremented by FetchEndpointsByNames
	endpointByNameCount int

	// serviceOperations/serviceOperationsErr configure FetchServiceOperations
	// for tests that build a session MCP fixture (mcp_session_fixture_test.go);
	// serviceOperationsCount records how many times it was called.
	serviceOperations      []fusedobject.Endpoint
	serviceOperationsErr   error
	serviceOperationsCount int
}

func (m *mockRegistryClient) FetchServiceMetadata(ctx context.Context, serviceID uuid.UUID, version string) (*fusedobject.ServiceMetadata, error) {
	m.fetchCount++
	m.fetchedVersions = append(m.fetchedVersions, version)
	if m.shouldFail {
		return nil, errors.New("registry offline")
	}
	return m.fusedObj, nil
}

func (m *mockRegistryClient) FetchEndpointByName(ctx context.Context, serviceID uuid.UUID, version string, endpointName string) (*fusedobject.Endpoint, error) {
	m.endpointByNameCount++
	return &fusedobject.Endpoint{Name: endpointName}, nil
}

func (m *mockRegistryClient) FetchEndpointsByNames(ctx context.Context, serviceID uuid.UUID, serviceVersionID uuid.UUID, endpointNames []string) ([]fusedobject.Endpoint, error) {
	m.endpointFetchCount++
	endpoints := make([]fusedobject.Endpoint, len(endpointNames))
	for i, name := range endpointNames {
		endpoints[i] = fusedobject.Endpoint{Name: name}
	}
	return endpoints, nil
}

func (m *mockRegistryClient) FetchServiceOperations(ctx context.Context, serviceID uuid.UUID, serviceVersionID uuid.UUID) ([]fusedobject.Endpoint, error) {
	m.serviceOperationsCount++
	if m.serviceOperationsErr != nil {
		return nil, m.serviceOperationsErr
	}
	return m.serviceOperations, nil
}

func (m *mockRegistryClient) ValidateSDKSelections(ctx context.Context, selections []models.SDKSelection) error {
	return nil
}

func (m *mockRegistryClient) FetchServiceVersionRevisions(ctx context.Context, refs []ServiceVersionRef, apiKey string) ([]ServiceVersionRevision, error) {
	return nil, nil
}

func (m *mockRegistryClient) Handshake(ctx context.Context) (string, string, error) {
	return "mock-acc", "mock-ws", nil
}

func (m *mockRegistryClient) SearchCatalogue(ctx context.Context, query string, page int, limit int) ([]CatalogueService, error) {
	return nil, nil
}

func (m *mockRegistryClient) FetchDriftSnapshots(ctx context.Context, serviceID uuid.UUID, apiKey string) ([]models.DriftSnapshot, error) {
	return nil, nil
}

func (m *mockRegistryClient) FetchDriftSnapshotsForServices(ctx context.Context, serviceIDs []uuid.UUID, apiKey string) ([]models.DriftSnapshot, error) {
	return nil, nil
}

// FetchServiceChangelogSince is unused by these tests but must exist for
// mockRegistryClient to satisfy RegistryClient (Phase 2 widened that
// interface -- see plans/plan-service-changelog.md's "## Phase 2").
func (m *mockRegistryClient) FetchServiceChangelogSince(ctx context.Context, serviceID uuid.UUID, since time.Time, apiKey string) ([]models.ServiceChangelogEntry, error) {
	return nil, nil
}

type mockStore struct {
	store.Store
	saveCount            int
	activateCount        int
	getCount             int
	shouldFail           bool
	fusedObj             []byte
	activatedServiceName string
	activatedVersionID   uuid.UUID
}

func (m *mockStore) BootstrapWorkspace(ctx context.Context, accountID uuid.UUID, name string) (uuid.UUID, error) {
	return uuid.Nil, nil
}

func (m *mockStore) SaveArtifactScope(ctx context.Context, scope store.ArtifactScope) error {
	return nil
}

func (m *mockStore) DeleteArtifactScope(ctx context.Context, accountID uuid.UUID, artifactID uuid.UUID) error {
	return nil
}

func (m *mockStore) GetSDKAccountID(ctx context.Context, artifactID uuid.UUID) (uuid.UUID, error) {
	return uuid.Nil, nil
}
func (m *mockStore) GetAccountByAPIKey(ctx context.Context, apiKey string) (uuid.UUID, error) {
	return uuid.Nil, nil
}
func (m *mockStore) GetArtifactScope(ctx context.Context, artifactID uuid.UUID) (*store.ArtifactScope, error) {
	return &store.ArtifactScope{ArtifactID: artifactID, ScopeSchemaVersion: models.ArtifactScopeSchemaVersion}, nil
}

func (m *mockStore) AddWorkspaceServiceVersion(ctx context.Context, serviceID uuid.UUID, serviceSlug string, version string, serviceVersionID uuid.UUID, serviceName string, addedBy uuid.UUID) error {
	m.activateCount++
	m.activatedServiceName = serviceName
	m.activatedVersionID = serviceVersionID
	if m.shouldFail {
		return errors.New("db error")
	}
	return nil
}

func (m *mockStore) EnableWorkspaceServiceVersion(ctx context.Context, serviceID uuid.UUID, version string, serviceVersionID uuid.UUID, enabledBy uuid.UUID) error {
	return nil
}

func (m *mockStore) DisableWorkspaceServiceVersion(ctx context.Context, serviceID uuid.UUID, version string) error {
	return nil
}

func (m *mockStore) ListWorkspaceServiceVersions(ctx context.Context, serviceID uuid.UUID) ([]store.WorkspaceServiceVersion, error) {
	return nil, nil
}

func (m *mockStore) ListWorkspaceServiceVersionsForServices(ctx context.Context, serviceIDs []uuid.UUID) (map[uuid.UUID][]store.WorkspaceServiceVersion, error) {
	return map[uuid.UUID][]store.WorkspaceServiceVersion{}, nil
}

func (m *mockStore) ListWorkspaceServices(ctx context.Context, names []string) ([]store.WorkspaceService, error) {
	return nil, nil
}

func (m *mockStore) RemoveWorkspaceService(ctx context.Context, serviceID uuid.UUID) error {
	return nil
}

func (m *mockStore) IsWorkspaceServiceEnabled(ctx context.Context, serviceID uuid.UUID) (bool, error) {
	return false, nil
}

func (m *mockStore) GetWorkspaceWebhookBySlug(ctx context.Context, slug string) (*store.WorkspaceWebhook, error) {
	return nil, store.ErrWorkspaceWebhookNotFound
}

func (m *mockStore) ListWorkspaceWebhooks(ctx context.Context, serviceID uuid.UUID) ([]store.WorkspaceWebhook, error) {
	return nil, nil
}

func (m *mockStore) GetLatestWorkspaceServiceVersion(ctx context.Context, accountID uuid.UUID, serviceID uuid.UUID) (string, error) {
	return "1.0", nil
}

func (m *mockStore) GetLatestWorkspaceServiceVersionByWorkspace(ctx context.Context, serviceID uuid.UUID) (string, error) {
	return "1.0", nil
}

func (m *mockStore) GetWorkspaceIDForAccount(ctx context.Context, accountID uuid.UUID) (uuid.UUID, error) {
	return uuid.Nil, nil
}

func TestActivationManager_ActivateService(t *testing.T) {
	ctx := context.Background()
	svcID := uuid.New().String()
	version := "1.0"

	t.Run("Success", func(t *testing.T) {
		versionID := uuid.New()
		mockClient := &mockRegistryClient{
			fusedObj: &fusedobject.ServiceMetadata{
				ServiceVersionID: versionID,
				Name:             "Linear",
			},
		}
		mockDB := &mockStore{}

		manager := NewActivationManager(mockClient, mockDB)
		err := manager.ActivateService(ctx, svcID, version)
		if err != nil {
			t.Fatalf("expected success, got error: %v", err)
		}

		if mockClient.fetchCount != 1 {
			t.Errorf("expected fetch to be called 1 time, got %d", mockClient.fetchCount)
		}
		if mockDB.activateCount != 1 {
			t.Errorf("expected activate to be called 1 time, got %d", mockDB.activateCount)
		}
		if mockDB.activatedServiceName != "Linear" {
			t.Errorf("expected service name Linear, got %q", mockDB.activatedServiceName)
		}
		if mockDB.activatedVersionID != versionID {
			t.Errorf("expected service version ID %s, got %s", versionID, mockDB.activatedVersionID)
		}
	})

	t.Run("InvalidUUID", func(t *testing.T) {
		manager := NewActivationManager(&mockRegistryClient{}, &mockStore{})
		err := manager.ActivateService(ctx, "not-a-uuid", version)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("FetchFails", func(t *testing.T) {
		mockClient := &mockRegistryClient{shouldFail: true}
		manager := NewActivationManager(mockClient, &mockStore{})
		err := manager.ActivateService(ctx, svcID, version)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func (m *mockStore) BatchCreateWebhookEvents(ctx context.Context, events []models.WebhookEvent) error {
	return nil
}

func (m *mockStore) BatchCreateEngineExecutionEvents(ctx context.Context, events []models.EngineExecutionEvent) error {
	return nil
}

func (m *mockStore) InsertMCPAnalytics(ctx context.Context, analytics *models.MCPAnalytics) error {
	return nil
}

func (m *mockStore) UpsertMCPSession(ctx context.Context, session *models.MCPSession) error {
	return nil
}

func (m *mockStore) GetIdempotentExecution(ctx context.Context, artifactID uuid.UUID, idempotencyKeyHash, requestBodyHash string) (*models.IdempotentExecution, error) {
	return nil, store.ErrIdempotentExecutionNotFound
}

func (m *mockStore) SaveIdempotentExecution(ctx context.Context, exec *models.IdempotentExecution) error {
	return nil
}
