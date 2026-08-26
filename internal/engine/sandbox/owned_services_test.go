package sandbox

import (
	"context"
	"errors"
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
	fetchErr      error
	contractErr   error
}

// FetchOwnedServices injects licensed listing failures independently of contract failures.
func (s *ownedServiceRegistryStub) FetchOwnedServices(context.Context) ([]OwnedRegistryService, error) {
	return append([]OwnedRegistryService(nil), s.services...), s.fetchErr
}

// FetchRuntimeContracts records one batch so recovery cannot regress into N+1 fetches.
func (s *ownedServiceRegistryStub) FetchRuntimeContracts(_ context.Context, versions []store.WorkspaceServiceVersion, _ string) ([]store.ServiceContractSnapshot, error) {
	s.contractCalls = append(s.contractCalls, append([]store.WorkspaceServiceVersion(nil), versions...))
	// Failure injection models the ordinary fail-closed fetcher contract.
	if s.contractErr != nil {
		return nil, s.contractErr
	}
	snapshots := make([]store.ServiceContractSnapshot, 0, len(versions))
	for _, version := range versions {
		snapshots = append(snapshots, store.ServiceContractSnapshot{ServiceID: version.ServiceID, ServiceVersionID: version.ServiceVersionID, Version: version.Version})
	}
	return snapshots, nil
}

type ownedServiceWorkspaceStub struct {
	store.Store
	active          map[uuid.UUID]bool
	added           []OwnedRegistryService
	snapshots       []store.ServiceContractSnapshot
	membershipCalls int
	storeErr        error
	membershipErr   error
}

// MissingOwnedServiceIDs models the SQL anti-join without a database in unit tests.
func (s *ownedServiceWorkspaceStub) MissingOwnedServiceIDs(_ context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	s.membershipCalls++
	var missing []uuid.UUID
	for _, id := range ids {
		// Only absent membership is eligible for startup restoration.
		if !s.active[id] {
			missing = append(missing, id)
		}
	}
	return missing, s.membershipErr
}

// UpsertServiceContractSnapshot records only admitted writes and can inject storage failure.
func (s *ownedServiceWorkspaceStub) UpsertServiceContractSnapshot(_ context.Context, snapshot store.ServiceContractSnapshot) (*store.ServiceContractSnapshot, error) {
	// A storage error must not be downgraded into recoverable content rejection.
	if s.storeErr != nil {
		return nil, s.storeErr
	}
	s.snapshots = append(s.snapshots, snapshot)
	return &snapshot, nil
}

// TestOwnedServiceRecoveryIsolatesRejectedContracts verifies partial recovery never activates bad data.
func TestOwnedServiceRecoveryIsolatesRejectedContracts(t *testing.T) {
	good, bad := recoveryTestService(), recoveryTestService()
	registry := &ownedServiceRegistryStub{services: []OwnedRegistryService{good, bad}}
	registry.contractErr = &runtimeContractRejections{
		failures: []OwnedServiceRejection{{ServiceID: bad.ServiceID, ServiceVersionID: bad.ServiceVersionID, cause: errors.New("invalid content")}},
		accepted: []store.ServiceContractSnapshot{{ServiceID: good.ServiceID, ServiceVersionID: good.ServiceVersionID}},
	}
	workspace := &ownedServiceWorkspaceStub{active: map[uuid.UUID]bool{}}
	result, err := ReconcileOwnedServices(context.Background(), workspace, registry, uuid.New(), "license-secret")
	// Recoverable rejection is not startup failure, but must not disappear from the result.
	if err != nil || result.Activated != 1 || len(result.Deferred) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	// Exactly one membership read and one contract read suffice for the whole batch.
	if workspace.membershipCalls != 1 || len(registry.contractCalls) != 1 {
		t.Fatal("recovery issued N+1 reads")
	}
	// The rejected contract must never reach either persistence or workspace membership.
	if len(workspace.snapshots) != 1 || workspace.active[bad.ServiceID] || !workspace.active[good.ServiceID] {
		t.Fatal("invalid activation boundary")
	}
}

// TestOwnedServiceRecoveryKeepsInfrastructureFailuresFatal prevents catch-all startup fail-open behavior.
func TestOwnedServiceRecoveryKeepsInfrastructureFailuresFatal(t *testing.T) {
	failure := errors.New("sentinel infrastructure failure")
	for _, stage := range []string{"listing", "contract_transport", "membership", "snapshot_storage"} {
		// Each independent boundary must remain fatal even after adding partial content recovery.
		t.Run(stage, func(t *testing.T) {
			registry := &ownedServiceRegistryStub{services: []OwnedRegistryService{recoveryTestService()}}
			workspace := &ownedServiceWorkspaceStub{active: map[uuid.UUID]bool{}}
			// Inject at one boundary so a different earlier failure cannot satisfy this assertion.
			switch stage {
			case "listing":
				registry.fetchErr = failure
			case "contract_transport":
				registry.contractErr = failure
			case "membership":
				workspace.membershipErr = failure
			case "snapshot_storage":
				workspace.storeErr = failure
			}
			_, err := ReconcileOwnedServices(context.Background(), workspace, registry, uuid.New(), "license")
			// Typed recovery must never swallow an unrelated failure.
			if !errors.Is(err, failure) {
				t.Fatalf("fatal cause lost: %v", err)
			}
		})
	}
}

// recoveryTestService gives each test an independent exact Registry version identity.
func recoveryTestService() OwnedRegistryService {
	return OwnedRegistryService{ServiceID: uuid.New(), ServiceVersionID: uuid.New(), Slug: "test-service", Name: "Test service", Version: "v1"}
}

func (s *ownedServiceWorkspaceStub) AddWorkspaceServiceVersion(_ context.Context, serviceID uuid.UUID, slug, version string, serviceVersionID uuid.UUID, name string, _ uuid.UUID) error {
	s.added = append(s.added, OwnedRegistryService{ServiceID: serviceID, Name: name, Slug: slug, Version: version, ServiceVersionID: serviceVersionID})
	s.active[serviceID] = true
	return nil
}

// TestReconcileOwnedServicesRestoresOnlyMissingServices keeps existing local pins authoritative during bootstrap.
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
	// Successful discovery must restore the absent service without touching the existing one.
	if err != nil {
		t.Fatalf("ReconcileOwnedServices: %v", err)
	}
	// The summary must account for both the preserved pin and the new activation.
	if result.Discovered != 2 || result.AlreadyActive != 1 || result.Activated != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
	// Pin identity must be the exact current Registry version of the missing service.
	if len(workspace.added) != 1 || workspace.added[0].ServiceID != missingID || workspace.added[0].ServiceVersionID != missingVersionID {
		t.Fatalf("unexpected activations: %#v", workspace.added)
	}
	// Existing snapshots must not be overwritten just because Registry latest changed.
	if len(workspace.snapshots) != 1 || workspace.snapshots[0].ServiceID != missingID {
		t.Fatalf("unexpected snapshots: %#v", workspace.snapshots)
	}
	assertSingleOwnedContractRead(t, registry, missingID)
}

// assertSingleOwnedContractRead checks the request count separately from activation semantics.
func assertSingleOwnedContractRead(t *testing.T, registry *ownedServiceRegistryStub, missingID uuid.UUID) {
	t.Helper()
	// Recovery must use one exact scoped batch rather than reading each service in a loop.
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
