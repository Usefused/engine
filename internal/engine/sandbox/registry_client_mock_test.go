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
}

func (m *mockRegistryClient) FetchServiceMetadata(ctx context.Context, serviceID uuid.UUID, version string) (*fusedobject.ServiceMetadata, error) {
	m.fetchCount++
	m.fetchedVersions = append(m.fetchedVersions, version)
	if m.shouldFail {
		return nil, errors.New("registry offline")
	}
	if m.fusedObj != nil && m.fusedObj.ContractVersion == 0 {
		m.fusedObj.ExecutionContractEnvelope = fusedobject.EngineExecutionContractSupport()
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

func (m *mockRegistryClient) FetchServiceOperations(context.Context, uuid.UUID, uuid.UUID) ([]fusedobject.Endpoint, error) {
	return nil, nil
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
