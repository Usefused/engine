package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestOwnedServiceRecoveryHTTPBatch exercises licensed discovery, real JSON validation, and isolated activation together.
func TestOwnedServiceRecoveryHTTPBatch(t *testing.T) {
	good, goodVersion := recoveryContractFixture()
	bad, badVersion := recoveryContractFixture()
	bad.RequiredCapabilities = []string{"unsupported.test.capability"}
	client, requests := newOwnedRecoveryHTTPRegistry(t, []store.WorkspaceServiceVersion{goodVersion, badVersion}, []runtimeContractBatchItem{bad, good})
	workspace := &ownedServiceWorkspaceStub{active: map[uuid.UUID]bool{}}
	result, err := ReconcileOwnedServices(context.Background(), workspace, client, uuid.New(), "license-fixture")
	// A bad peer remains deferred without preventing a valid peer from becoming usable.
	if err != nil || result.Activated != 1 || len(result.Deferred) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	// Batch recovery must not introduce N+1 network or membership reads.
	if requests.Load() != 2 || workspace.membershipCalls != 1 {
		t.Fatalf("requests=%d membership=%d", requests.Load(), workspace.membershipCalls)
	}
	// Only the validated peer reaches membership and snapshot storage.
	if !workspace.active[goodVersion.ServiceID] || workspace.active[badVersion.ServiceID] || len(workspace.snapshots) != 1 {
		t.Fatal("invalid recovery isolation")
	}
}

// newOwnedRecoveryHTTPRegistry supplies the two real wire shapes while keeping test transport out of assertions.
func newOwnedRecoveryHTTPRegistry(t *testing.T, versions []store.WorkspaceServiceVersion, items []runtimeContractBatchItem) (*HTTPRegistryClient, *atomic.Int64) {
	t.Helper()
	response := runtimeContractsGraphQLResponse{}
	response.Data.Contracts = items
	var requests atomic.Int64
	// One listing and one runtime-contract batch replace any per-service fallback traffic.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		// Both reads must use the Engine license, not an ambient caller or another tenant.
		if r.Header.Get("X-API-Key") != "license-fixture" {
			t.Error("unlicensed recovery request")
			w.WriteHeader(401)
			return
		}
		var query graphqlQuery
		// Reject an invalid test request instead of accidentally serving a success fixture.
		if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Discovery and runtime projection are the two existing product queries, not per-item aliases.
		if strings.Contains(query.Query, "query OwnedServices") {
			_ = json.NewEncoder(w).Encode(ownedRecoveryPageFixture(versions))
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(server.Close)
	client := &HTTPRegistryClient{endpoint: server.URL, licenseKey: "license-fixture", httpClient: server.Client()}
	return client, &requests
}

// ownedRecoveryPageFixture projects exact current versions through the production discovery wire format.
func ownedRecoveryPageFixture(versions []store.WorkspaceServiceVersion) map[string]any {
	rows := make([]map[string]any, 0, len(versions))
	for _, version := range versions {
		rows = append(rows, map[string]any{"id": version.ServiceID, "name": "Recovery", "slug": version.ServiceID.String(),
			"current_service_version": version.Version, "service_versions": []map[string]any{{"id": version.ServiceVersionID, "name": version.Version}}})
	}
	return map[string]any{"data": map[string]any{"services": map[string]any{"data": rows, "page": 1, "limit": 100, "total": len(rows)}}}
}

// recoveryContractFixture is an anonymous valid contract independent of provider-specific fixtures.
func recoveryContractFixture() (runtimeContractBatchItem, store.WorkspaceServiceVersion) {
	version := store.WorkspaceServiceVersion{ServiceID: uuid.New(), ServiceVersionID: uuid.New(), Version: "v1"}
	return runtimeContractBatchItem{
		ExecutionContractEnvelope: fusedobject.ExecutionContractEnvelope{ContractVersion: 2, RequiredCapabilities: []string{}},
		ServiceID:                 version.ServiceID, ServiceVersionID: version.ServiceVersionID, Version: version.Version,
		Service: &runtimeContractService{ID: version.ServiceID, Name: "Recovery", BaseURL: "https://api.example.test"},
	}, version
}

// TestRuntimeContractBatchRecoveryRetainsOnlyValidSnapshots proves one bad webhook cannot enter persistence with good peers.
func TestRuntimeContractBatchRecoveryRetainsOnlyValidSnapshots(t *testing.T) {
	good, goodVersion := recoveryContractFixture()
	bad, badVersion := recoveryContractFixture()
	webhook := inboundSecurityFixture("signature", fusedobject.InboundSecurityScheme{Type: "http", Scheme: "basic"})
	webhook.Contract.SecuritySchemes = nil
	bad.Webhooks = []fusedobject.Webhook{webhook}
	snapshots, err := runtimeContractSnapshotsFromBatch([]runtimeContractBatchItem{good, bad}, []store.WorkspaceServiceVersion{goodVersion, badVersion})
	var rejected *runtimeContractRejections
	// Strict callers get failure, never a successful partial list.
	if snapshots != nil || !errors.As(err, &rejected) {
		t.Fatalf("strict result=%v error=%v", snapshots, err)
	}
	// Only the recovery boundary can access the independently admitted peer.
	if len(rejected.accepted) != 1 || rejected.accepted[0].ServiceID != goodVersion.ServiceID {
		t.Fatal("valid peer was lost")
	}
	// Rejection must identify the exact bad version without inventing inbound authentication.
	if len(rejected.failures) != 1 || rejected.failures[0].ServiceVersionID != badVersion.ServiceVersionID {
		t.Fatal("rejected identity lost")
	}
}

// TestRuntimeContractBatchIdentityFailuresAreFatal prevents partial recovery from masking missing or substituted data.
func TestRuntimeContractBatchIdentityFailuresAreFatal(t *testing.T) {
	item, version := recoveryContractFixture()
	for _, items := range [][]runtimeContractBatchItem{nil, {item, item}, {{ServiceID: uuid.New(), ServiceVersionID: version.ServiceVersionID, Service: item.Service}}} {
		_, err := runtimeContractSnapshotsFromBatch(items, []store.WorkspaceServiceVersion{version})
		var rejected *runtimeContractRejections
		// Identity failures must not enter the recoverable content-error class.
		if err == nil || errors.As(err, &rejected) {
			t.Fatalf("identity error became recoverable: %v", err)
		}
	}
}

// TestRuntimeContractGraphQLRecoveryRequiresExplicitClassification rejects text matching and mixed auth errors.
func TestRuntimeContractGraphQLRecoveryRequiresExplicitClassification(t *testing.T) {
	_, version := recoveryContractFixture()
	classified := runtimeContractGraphQLError{Message: "dependency secret=fsk_never_return"}
	classified.Extensions.Code = "runtime_contract_rejected"
	classified.Extensions.ServiceVersionID = version.ServiceVersionID
	classified.Extensions.Reason = "  Generation contract identity\nfailed deterministic validation.  "
	err := classifyRuntimeContractGraphQLErrors([]runtimeContractGraphQLError{classified}, []store.WorkspaceServiceVersion{version})
	var rejected *runtimeContractRejections
	// An explicit authorized version-bound rejection may defer the whole batch without extra queries.
	if !errors.As(err, &rejected) || len(rejected.accepted) != 0 || len(rejected.failures) != 1 {
		t.Fatalf("classification=%v", err)
	}
	versionID, reason, ok := RuntimeContractRejectionDetails(err)
	// Only the explicit reason is normalized for callers; the top-level GraphQL message remains private.
	if !ok || versionID != version.ServiceVersionID || reason != "Generation contract identity failed deterministic validation." || strings.Contains(reason, "fsk_never_return") {
		t.Fatalf("details=(%s, %q, %t)", versionID, reason, ok)
	}
	wrongVersion := classified
	wrongVersion.Extensions.ServiceVersionID = uuid.New()
	for _, failures := range [][]runtimeContractGraphQLError{
		{{Message: "runtime security requirement references unknown scheme"}},
		{classified, {Message: "Unauthorized"}},
		{wrongVersion},
	} {
		err := classifyRuntimeContractGraphQLErrors(failures, []store.WorkspaceServiceVersion{version})
		// Unknown, old-server, or mixed responses remain fatal rather than guessing from message wording.
		if errors.As(err, &rejected) {
			t.Fatalf("unclassified failure downgraded: %v", err)
		}
	}
}

// TestRuntimeContractRejectionDetailsBoundsExplicitReason prevents typed upstream detail from producing unbounded public output.
func TestRuntimeContractRejectionDetailsBoundsExplicitReason(t *testing.T) {
	_, version := recoveryContractFixture()
	classified := runtimeContractGraphQLError{Message: "unused downstream diagnostic"}
	classified.Extensions.Code = "runtime_contract_rejected"
	classified.Extensions.ServiceVersionID = version.ServiceVersionID
	classified.Extensions.Reason = strings.Repeat("r", maxRuntimeContractRejectionReasonRunes+20)
	err := classifyRuntimeContractGraphQLErrors([]runtimeContractGraphQLError{classified}, []store.WorkspaceServiceVersion{version})
	_, reason, ok := RuntimeContractRejectionDetails(err)
	// The reason remains useful but cannot exceed the Engine's response budget.
	if !ok || len([]rune(reason)) != maxRuntimeContractRejectionReasonRunes {
		t.Fatalf("reason length=%d ok=%t", len([]rune(reason)), ok)
	}
}

// TestOwnedRecoverySetRequiresCompleteDisjointResults prevents a fetcher from silently omitting or double-activating a version.
func TestOwnedRecoverySetRequiresCompleteDisjointResults(t *testing.T) {
	service := recoveryTestService()
	snapshot := store.ServiceContractSnapshot{ServiceID: service.ServiceID, ServiceVersionID: service.ServiceVersionID}
	rejection := OwnedServiceRejection{ServiceID: service.ServiceID, ServiceVersionID: service.ServiceVersionID}
	for _, result := range []struct {
		snapshots []store.ServiceContractSnapshot
		failures  []OwnedServiceRejection
	}{
		{},
		{snapshots: []store.ServiceContractSnapshot{snapshot, snapshot}},
		{snapshots: []store.ServiceContractSnapshot{snapshot}, failures: []OwnedServiceRejection{rejection}},
		{failures: []OwnedServiceRejection{{ServiceID: uuid.New(), ServiceVersionID: service.ServiceVersionID}}},
	} {
		// Every invalid partition must fail before snapshot writes or workspace membership changes.
		if err := validateOwnedRecoverySet([]OwnedRegistryService{service}, result.snapshots, result.failures); err == nil {
			t.Fatal("incomplete or overlapping recovery accepted")
		}
	}
}

// TestRuntimeContractDecodeRetainsStrictFailure proves the actual JSON boundary retains both failure and recovery data.
func TestRuntimeContractDecodeRetainsStrictFailure(t *testing.T) {
	item, version := recoveryContractFixture()
	item.RequiredCapabilities = []string{"unknown.capability"}
	response := runtimeContractsGraphQLResponse{}
	response.Data.Contracts = []runtimeContractBatchItem{item}
	encoded, err := json.Marshal(response)
	// Fixture serialization must succeed before testing the production decoder.
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := decodeRuntimeContractsResponse(strings.NewReader(string(encoded)), []store.WorkspaceServiceVersion{version})
	var rejected *runtimeContractRejections
	// Unknown capabilities remain non-executable even though startup may continue without this service.
	if snapshots != nil || !errors.As(err, &rejected) || len(rejected.accepted) != 0 {
		t.Fatalf("decode=%v err=%v", snapshots, err)
	}
}

// TestOwnedServiceRecoveryTelemetryIsBounded ensures errors and contract contents cannot become trace data.
func TestOwnedServiceRecoveryTelemetryIsBounded(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	// Restore global tracing so independent tests never inherit this test's collector.
	t.Cleanup(func() { otel.SetTracerProvider(previous); _ = provider.Shutdown(context.Background()) })
	service := recoveryTestService()
	registry := &ownedServiceRegistryStub{services: []OwnedRegistryService{service}, contractErr: &runtimeContractRejections{failures: []OwnedServiceRejection{{ServiceID: service.ServiceID, ServiceVersionID: service.ServiceVersionID, cause: errors.New("secret-contract-body")}}}}
	workspace := &ownedServiceWorkspaceStub{active: map[uuid.UUID]bool{}}
	_, err := ReconcileOwnedServices(context.Background(), workspace, registry, uuid.New(), "secret-license")
	// Recoverable content rejection must still emit a completed partial span.
	if err != nil {
		t.Fatal(err)
	}
	spans := recorder.Ended()
	// One recovery attempt owns one aggregate trace rather than one trace per service.
	if len(spans) != 1 {
		t.Fatalf("spans=%d", len(spans))
	}
	allowed := map[string]bool{"outcome": true, "service.discovered_count": true, "service.activated_count": true, "service.deferred_count": true}
	for _, attribute := range spans[0].Attributes() {
		// Exact allowlisting blocks future accidental raw error, license or provider fields.
		if !allowed[string(attribute.Key)] || strings.Contains(attribute.Value.Emit(), "secret") {
			t.Fatalf("unsafe telemetry: %s", attribute.Key)
		}
	}
}
