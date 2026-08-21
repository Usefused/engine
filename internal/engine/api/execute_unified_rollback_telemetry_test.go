package api

import (
	"context"
	"errors"
	"testing"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
)

// unifiedRollbackTelemetryRuntime fails the dependent forward call while
// retaining the standard handler fixture for its prerequisite and rollback.
type unifiedRollbackTelemetryRuntime struct {
	*unifiedRuntimeTestDouble
}

// ExecuteResolvedPhysicalJSON records and fails the GitHub dependency consumer
// so wrapper telemetry observes one real scheduled compensation.
func (runtime *unifiedRollbackTelemetryRuntime) ExecuteResolvedPhysicalJSON(ctx context.Context, identity auth.RuntimeIdentity, operation sandbox.ResolvedPhysicalOperation, request sandbox.PhysicalExecutionRequest) (sandbox.PhysicalExecutionResult, error) {
	if _, isGitHub := request.Params["title"]; !isGitHub {
		return runtime.unifiedRuntimeTestDouble.ExecuteResolvedPhysicalJSON(ctx, identity, operation, request)
	}
	runtime.mu.Lock()
	runtime.executeCalls++
	runtime.calls = append(runtime.calls, unifiedRuntimeCall{
		params: request.Params, credentials: request.Credentials, environment: request.Environment,
		idempotency: request.IdempotencyKey, requestHash: request.RequestBodyHash,
	})
	runtime.mu.Unlock()
	return sandbox.PhysicalExecutionResult{}, errors.New("private dependent provider failure")
}

// TestExecuteUnifiedRollbackTelemetryUsesBoundedExactCounts proves the logical
// wrapper records only outcome counts while each physical call owns its detail.
func TestExecuteUnifiedRollbackTelemetryUsesBoundedExactCounts(t *testing.T) {
	exporter := setupTestTracer(t)
	server, baseRuntime, appID := newUnifiedRuntimeServerFromFixture(t, newUnifiedRollbackCompileFixture(), store.AppTokenPolicy{AllowAll: true})
	runtime := &unifiedRollbackTelemetryRuntime{unifiedRuntimeTestDouble: baseRuntime}
	server.unifiedRuntime = runtime
	request := unifiedRuntimeRequest()
	request.TargetSelectors = nil
	response, err := server.ExecuteUnified(grpcTestContext(appID), request)
	if err != nil {
		t.Fatalf("ExecuteUnified() error = %v", err)
	}
	if len(response.GetRollbackResults()) != 1 || response.GetRollbackResults()[0].GetStatus() != "success" {
		t.Fatalf("rollback results = %#v", response.GetRollbackResults())
	}
	if baseRuntime.resolveCalls != 1 || len(baseRuntime.bindings) != 3 || baseRuntime.executeCalls != 3 {
		t.Fatalf("physical work = resolve:%d bindings:%d execute:%d", baseRuntime.resolveCalls, len(baseRuntime.bindings), baseRuntime.executeCalls)
	}
	assertUnifiedWrapperTelemetry(t, exporter, map[string]string{
		"unified.schema_version": "3", "unified.stage": "dispatch", "unified.outcome": "partial",
		"unified.target_count": "2", "unified.success_count": "1", "unified.error_count": "1",
		"unified.skipped_count": "0", "unified.rollback_count": "1",
		"unified.rollback_success_count": "1", "unified.rollback_error_count": "0",
	}, "issues.create", "github", "@acme/custom-crm", "private dependent provider failure", "Bug", "crm-1")
}

var _ unifiedPhysicalRuntime = (*unifiedRollbackTelemetryRuntime)(nil)
