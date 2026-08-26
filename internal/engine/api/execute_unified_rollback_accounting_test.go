package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Usefused/engine/internal/engine"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/engine/store"
)

// TestExecuteUnifiedRollbackUsesPhysicalAccounting proves a dependency failure
// produces a separately finalized rollback receipt and usage while its parent remains audit-only.
func TestExecuteUnifiedRollbackUsesPhysicalAccounting(t *testing.T) {
	var githubCalls atomic.Int32
	var crmCalls atomic.Int32
	provider := httptest.NewServer(unifiedRollbackAccountingHandler(&githubCalls, &crmCalls))
	defer provider.Close()

	server, _, appID := newUnifiedRuntimeServerFromFixture(t, newUnifiedRollbackCompileFixture(), store.AppTokenPolicy{AllowAll: true})
	server.unifiedRuntime = &unifiedAccountingRuntime{appID: appID, providerURL: provider.URL, dispatcher: engine.NewDispatcher()}
	events, usage := installUnifiedAccountingCaptures(t)
	request := unifiedRuntimeRequest()
	request.TargetSelectors = nil
	response, err := server.ExecuteUnified(grpcTestContext(appID), request)
	if err != nil {
		t.Fatalf("ExecuteUnified() error = %v", err)
	}
	assertUnifiedRollbackAccountingResponse(t, response)
	// Forward, failed dependent and compensation are three provider calls plus one audit parent.
	if githubCalls.Load() != 1 || crmCalls.Load() != 2 || len(events.messages) != 4 {
		t.Fatalf("physical attempts = github:%d crm:%d events:%d, want one/two/four", githubCalls.Load(), crmCalls.Load(), len(events.messages))
	}
	assertUnifiedPhysicalEvents(t, events.messages, appID)
	assertUnifiedUsageOutcomes(t, usage.increments, 3, 2, 1)
}

// TestExecuteUnifiedInputMappingFailureIsNotPhysicallyAccounted proves an
// omitted dependent mapping is a logical result only: the successful
// prerequisite and its rollback are the only billed/audited provider calls.
func TestExecuteUnifiedInputMappingFailureIsNotPhysicallyAccounted(t *testing.T) {
	var githubCalls atomic.Int32
	var crmCalls atomic.Int32
	provider := httptest.NewServer(unifiedRollbackAccountingHandler(&githubCalls, &crmCalls))
	defer provider.Close()

	fixture := newUnifiedRollbackCompileFixture()
	operation := fixture.document.UnifiedOperations["issues.create"]
	github := operation.Bindings["github"]
	github.Input = json.RawMessage(`"${response.@acme/custom-crm.missing}"`)
	operation.Bindings["github"] = github
	fixture.document.UnifiedOperations["issues.create"] = operation
	server, _, appID := newUnifiedRuntimeServerFromFixture(t, fixture, store.AppTokenPolicy{AllowAll: true})
	server.unifiedRuntime = &unifiedAccountingRuntime{appID: appID, providerURL: provider.URL, dispatcher: engine.NewDispatcher()}
	events, usage := installUnifiedAccountingCaptures(t)
	request := unifiedRuntimeRequest()
	request.TargetSelectors = nil
	response, err := server.ExecuteUnified(grpcTestContext(appID), request)
	if err != nil {
		t.Fatalf("ExecuteUnified() error = %v", err)
	}
	assertUnifiedInputMappingAccountingResponse(t, response)
	// Mapping failure adds a parent step, not a physical attempt or usage increment.
	if githubCalls.Load() != 0 || crmCalls.Load() != 2 || len(events.messages) != 3 {
		t.Fatalf("physical attempts = github:%d crm:%d events:%d, want zero/two/three", githubCalls.Load(), crmCalls.Load(), len(events.messages))
	}
	assertUnifiedPhysicalEvents(t, events.messages, appID)
	assertUnifiedUsageOutcomes(t, usage.increments, 2, 2, 0)
}

// unifiedRollbackAccountingHandler models a CRM create, a failing GitHub
// consumer, and the subsequent bodyless CRM delete on one real HTTP server.
func unifiedRollbackAccountingHandler(githubCalls, crmCalls *atomic.Int32) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/github" {
			githubCalls.Add(1)
			response.WriteHeader(http.StatusInternalServerError)
			_, _ = response.Write([]byte(`{"error":"fixture"}`))
			return
		}
		call := crmCalls.Add(1)
		if call == 1 {
			_, _ = response.Write([]byte(`{"iid":"crm-1"}`))
			return
		}
		// a bodyless delete-style compensation must remain a successful
		// physical execution without manufacturing a Unified response body.
		response.WriteHeader(http.StatusNoContent)
	}
}

// assertUnifiedRollbackAccountingResponse requires the forward failure and
// successful compensation to remain separate, data-free public result entries.
func assertUnifiedRollbackAccountingResponse(t *testing.T, response *enginev1.ExecuteUnifiedResponse) {
	t.Helper()
	if len(response.GetResults()) != 2 || response.GetResults()[0].GetErrorCode() != "provider_error" || response.GetResults()[1].GetStatus() != "success" {
		t.Fatalf("forward results = %#v", response.GetResults())
	}
	if len(response.GetRollbackResults()) != 1 || response.GetRollbackResults()[0].GetTarget() != "@acme/custom-crm" || response.GetRollbackResults()[0].GetStatus() != "success" {
		t.Fatalf("rollback results = %#v", response.GetRollbackResults())
	}
}

// assertUnifiedInputMappingAccountingResponse verifies the non-physical
// consumer failure remains visible while its prerequisite is compensated.
func assertUnifiedInputMappingAccountingResponse(t *testing.T, response *enginev1.ExecuteUnifiedResponse) {
	t.Helper()
	if len(response.GetResults()) != 2 || response.GetResults()[0].GetErrorCode() != "input_mapping_failed" || response.GetResults()[1].GetStatus() != "success" {
		t.Fatalf("forward results = %#v", response.GetResults())
	}
	if len(response.GetRollbackResults()) != 1 || response.GetRollbackResults()[0].GetTarget() != "@acme/custom-crm" || response.GetRollbackResults()[0].GetStatus() != "success" {
		t.Fatalf("rollback results = %#v", response.GetRollbackResults())
	}
}
