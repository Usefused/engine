package sandbox

import (
	"context"
	"errors"
	"time"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// mockRegistryClient is shared by cache and MCP session fixture tests.
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
// mockRegistryClient to satisfy RegistryClient.
func (m *mockRegistryClient) FetchServiceChangelogSince(ctx context.Context, serviceID uuid.UUID, since time.Time, apiKey string) ([]models.ServiceChangelogEntry, error) {
	return nil, nil
}
