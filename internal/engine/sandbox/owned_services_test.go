package sandbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

type ownedServiceRegistryStub struct {
	services      []OwnedRegistryService
	contractCalls [][]store.WorkspaceServiceVersion
}

func (s *ownedServiceRegistryStub) FetchOwnedServices(context.Context) ([]OwnedRegistryService, error) {
	return append([]OwnedRegistryService(nil), s.services...), nil
}

func (s *ownedServiceRegistryStub) FetchRuntimeContracts(_ context.Context, versions []store.WorkspaceServiceVersion, _ string) ([]store.ServiceContractSnapshot, error) {
	s.contractCalls = append(s.contractCalls, append([]store.WorkspaceServiceVersion(nil), versions...))
	snapshots := make([]store.ServiceContractSnapshot, 0, len(versions))
	for _, version := range versions {
		snapshots = append(snapshots, store.ServiceContractSnapshot{ServiceID: version.ServiceID, ServiceVersionID: version.ServiceVersionID, Version: version.Version})
	}
	return snapshots, nil
}

type ownedServiceWorkspaceStub struct {
	store.Store
	active    map[uuid.UUID]bool
	added     []OwnedRegistryService
	snapshots []store.ServiceContractSnapshot
}

func (s *ownedServiceWorkspaceStub) IsWorkspaceServiceEnabled(_ context.Context, serviceID uuid.UUID) (bool, error) {
	return s.active[serviceID], nil
}

func (s *ownedServiceWorkspaceStub) UpsertServiceContractSnapshot(_ context.Context, snapshot store.ServiceContractSnapshot) (*store.ServiceContractSnapshot, error) {
	s.snapshots = append(s.snapshots, snapshot)
	return &snapshot, nil
}

func (s *ownedServiceWorkspaceStub) AddWorkspaceServiceVersion(_ context.Context, serviceID uuid.UUID, slug, version string, serviceVersionID uuid.UUID, name string, _ uuid.UUID) error {
	s.added = append(s.added, OwnedRegistryService{ServiceID: serviceID, Name: name, Slug: slug, Version: version, ServiceVersionID: serviceVersionID})
	s.active[serviceID] = true
	return nil
}

func TestReconcileOwnedServicesRestoresOnlyMissingServices(t *testing.T) {
	accountID := uuid.New()
	activeID := uuid.New()
	missingID := uuid.New()
	missingVersionID := uuid.New()
	registry := &ownedServiceRegistryStub{services: []OwnedRegistryService{
		{ServiceID: activeID, Name: "Existing", Slug: "existing", Version: "1.0", ServiceVersionID: uuid.New()},
		{ServiceID: missingID, Name: "Missing", Slug: "missing", Version: "2.0", ServiceVersionID: missingVersionID},
	}}
	workspace := &ownedServiceWorkspaceStub{active: map[uuid.UUID]bool{activeID: true}}

	result, err := ReconcileOwnedServices(context.Background(), workspace, registry, accountID, "license")
	if err != nil {
		t.Fatalf("ReconcileOwnedServices: %v", err)
	}
	if result.Discovered != 2 || result.AlreadyActive != 1 || result.Activated != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if len(workspace.added) != 1 || workspace.added[0].ServiceID != missingID || workspace.added[0].ServiceVersionID != missingVersionID {
		t.Fatalf("unexpected activations: %#v", workspace.added)
	}
	if len(workspace.snapshots) != 1 || workspace.snapshots[0].ServiceID != missingID {
		t.Fatalf("unexpected snapshots: %#v", workspace.snapshots)
	}
	if len(registry.contractCalls) != 1 || len(registry.contractCalls[0]) != 1 || registry.contractCalls[0][0].ServiceID != missingID {
		t.Fatalf("unexpected contract calls: %#v", registry.contractCalls)
	}
}

func TestFetchOwnedServicesUsesLicensedAccountAndCurrentVersion(t *testing.T) {
	serviceID := uuid.New()
	versionID := uuid.New()
	authenticated := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authenticated = r.Header.Get("Authorization") == "Bearer license" && r.Header.Get("X-API-Key") == "license"
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"services":{"data":[` +
			`{"id":"` + serviceID.String() + `","name":"Jira","slug":"jira","current_service_version":"1.0","service_versions":[{"id":"` + versionID.String() + `","name":"1.0"}]},` +
			`{"id":"` + uuid.NewString() + `","name":"Draft","slug":"draft","current_service_version":"","service_versions":[]}` +
			`],"total":2,"page":1,"limit":100}}}`))
	}))
	defer server.Close()

	client := &HTTPRegistryClient{endpoint: server.URL, licenseKey: "license", httpClient: server.Client()}
	services, err := client.FetchOwnedServices(context.Background())
	if err != nil {
		t.Fatalf("FetchOwnedServices: %v", err)
	}
	if !authenticated {
		t.Fatal("Registry request did not use licensed identity")
	}
	if len(services) != 1 {
		t.Fatalf("services = %#v", services)
	}
	service := services[0]
	if service.ServiceID != serviceID || service.ServiceVersionID != versionID || service.Version != "1.0" || service.Slug != "jira" {
		t.Fatalf("service = %#v", service)
	}
}

func TestFetchOwnedServicesRejectsRegistryErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errors":[{"message":"unavailable"}]}`))
	}))
	defer server.Close()
	client := &HTTPRegistryClient{endpoint: server.URL, licenseKey: "license", httpClient: server.Client()}
	_, err := client.FetchOwnedServices(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("expected Registry error, got %v", err)
	}
}
