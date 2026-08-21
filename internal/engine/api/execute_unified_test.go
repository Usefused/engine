package api

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type unifiedTestValidator struct {
	identity auth.RuntimeIdentity
}

// Validate exercises selector admission through the production Unified RPC execution interface.
func (validator unifiedTestValidator) Validate(_ context.Context, appID uuid.UUID, token string) (auth.RuntimeIdentity, error) {
	if appID != validator.identity.AppID || token != "fsk_test" {
		return auth.RuntimeIdentity{}, auth.ErrUnauthorized
	}
	return validator.identity, nil
}

type unifiedRuntimeCall struct {
	params      map[string]any
	credentials map[string]any
	environment string
	idempotency string
	requestHash string
}

type unifiedRuntimeTestDouble struct {
	mu                sync.Mutex
	resolveCalls      int
	executeCalls      int
	bindings          []sandbox.ExactOperationBinding
	calls             []unifiedRuntimeCall
	started           chan struct{}
	release           <-chan struct{}
	failSummary       bool
	rejectEnvironment string
	selectorChecks    int
}

// ResolveExactPhysicalOperations supplies exact pre-resolved operations so the test isolates Unified RPC execution.
func (runtime *unifiedRuntimeTestDouble) ResolveExactPhysicalOperations(_ context.Context, _ uuid.UUID, bindings []sandbox.ExactOperationBinding) ([]sandbox.ResolvedPhysicalOperation, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.resolveCalls++
	runtime.bindings = append([]sandbox.ExactOperationBinding(nil), bindings...)
	return make([]sandbox.ResolvedPhysicalOperation, len(bindings)), nil
}

// ValidateResolvedPhysicalSelectors exercises selector admission through the production Unified RPC execution interface.
func (runtime *unifiedRuntimeTestDouble) ValidateResolvedPhysicalSelectors(_ sandbox.ResolvedPhysicalOperation, selectors sandbox.PhysicalExecutionSelectors) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.selectorChecks++
	if selectors.Environment == runtime.rejectEnvironment && runtime.rejectEnvironment != "" {
		return sandbox.ErrPhysicalSelectorContract
	}
	return nil
}

// ExecuteResolvedPhysicalJSON records one scripted physical call while preserving Unified RPC execution assertions.
func (runtime *unifiedRuntimeTestDouble) ExecuteResolvedPhysicalJSON(_ context.Context, _ auth.RuntimeIdentity, _ sandbox.ResolvedPhysicalOperation, request sandbox.PhysicalExecutionRequest) (sandbox.PhysicalExecutionResult, error) {
	runtime.mu.Lock()
	runtime.executeCalls++
	runtime.calls = append(runtime.calls, unifiedRuntimeCall{
		params: request.Params, credentials: request.Credentials, environment: request.Environment,
		idempotency: request.IdempotencyKey, requestHash: request.RequestBodyHash,
	})
	runtime.mu.Unlock()
	if runtime.started != nil {
		runtime.started <- struct{}{}
	}
	if runtime.release != nil {
		<-runtime.release
	}
	if _, isCRM := request.Params["summary"]; isCRM {
		if runtime.failSummary {
			return sandbox.PhysicalExecutionResult{}, errors.New("private provider failure")
		}
		return sandbox.PhysicalExecutionResult{Body: []byte(`{"iid":"crm-1"}`)}, nil
	}
	return sandbox.PhysicalExecutionResult{Body: []byte(`{"id":"gh-1"}`)}, nil
}

// ExecuteResolvedPhysicalSuccess records a rollback invocation for scheduler
// assertions without requiring a provider response body.
func (runtime *unifiedRuntimeTestDouble) ExecuteResolvedPhysicalSuccess(_ context.Context, _ auth.RuntimeIdentity, _ sandbox.ResolvedPhysicalOperation, request sandbox.PhysicalExecutionRequest) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.executeCalls++
	runtime.calls = append(runtime.calls, unifiedRuntimeCall{
		params: request.Params, credentials: request.Credentials, environment: request.Environment,
		idempotency: request.IdempotencyKey, requestHash: request.RequestBodyHash,
	})
	return nil
}

// TestExecuteUnifiedFansOutConcurrentlyAndNormalizesRootOutput protects the rule that ready DAG nodes run promptly while public results retain request order.
func TestExecuteUnifiedFansOutConcurrentlyAndNormalizesRootOutput(t *testing.T) {
	exporter := setupTestTracer(t)
	server, runtime, appID := newUnifiedRuntimeServer(t, store.AppTokenPolicy{AllowAll: true})
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	runtime.started = started
	runtime.release = release

	type callResult struct {
		response *enginev1.ExecuteUnifiedResponse
		err      error
	}
	done := make(chan callResult, 1)
	go func() {
		response, err := server.ExecuteUnified(grpcTestContext(appID), unifiedRuntimeRequest())
		done <- callResult{response: response, err: err}
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("selected targets did not enter the physical boundary concurrently")
		}
	}
	close(release)

	result := <-done
	if result.err != nil {
		t.Fatalf("ExecuteUnified() error = %v", result.err)
	}
	assertUnifiedResults(t, result.response, []string{"github", "@acme/custom-crm"}, []string{`{"id":"gh-1"}`, `{"iid":"crm-1"}`})
	if got := string(result.response.GetOutputJson()); got != `{"id":"gh-1"}` || result.response.GetOutputErrorCode() != "" {
		t.Fatalf("root output = %s / %q", got, result.response.GetOutputErrorCode())
	}
	if runtime.resolveCalls != 1 || runtime.executeCalls != 2 || len(runtime.bindings) != 2 {
		t.Fatalf("runtime calls = resolve:%d execute:%d bindings:%d", runtime.resolveCalls, runtime.executeCalls, len(runtime.bindings))
	}
	assertUnifiedChildRequests(t, runtime.calls)
	assertUnifiedWrapperTelemetry(t, exporter, map[string]string{
		"unified.schema_version": "3", "unified.stage": "dispatch", "unified.outcome": "success",
		"unified.target_count": "2", "unified.success_count": "2", "unified.error_count": "0",
		"unified.skipped_count": "0", "unified.rollback_count": "0",
		"unified.rollback_success_count": "0", "unified.rollback_error_count": "0",
	}, "issues.create", "github", "@acme/custom-crm", "Bug", "sandbox", "user-1", "githubOAuth", "logical-request-1", "gh-1", "crm-1")
}

// TestExecuteUnifiedReturnsOrderedMixedResults protects the rule that partial failures do not disturb deterministic target ordering.
func TestExecuteUnifiedReturnsOrderedMixedResults(t *testing.T) {
	exporter := setupTestTracer(t)
	server, runtime, appID := newUnifiedRuntimeServer(t, store.AppTokenPolicy{AllowAll: true})
	runtime.failSummary = true
	response, err := server.ExecuteUnified(grpcTestContext(appID), unifiedRuntimeRequest())
	if err != nil {
		t.Fatalf("ExecuteUnified() error = %v", err)
	}
	if len(response.GetResults()) != 2 || response.GetResults()[0].GetStatus() != "success" {
		t.Fatalf("unexpected mixed response: %#v", response.GetResults())
	}
	failure := response.GetResults()[1]
	if failure.GetTarget() != "@acme/custom-crm" || failure.GetStatus() != "error" || failure.GetErrorCode() != "execution_failed" || len(failure.GetDataJson()) != 0 {
		t.Fatalf("unexpected failure result: %#v", failure)
	}
	assertUnifiedWrapperTelemetry(t, exporter, map[string]string{
		"unified.schema_version": "3", "unified.stage": "dispatch", "unified.outcome": "partial",
		"unified.target_count": "2", "unified.success_count": "1", "unified.error_count": "1",
		"unified.skipped_count": "0", "unified.rollback_count": "0",
		"unified.rollback_success_count": "0", "unified.rollback_error_count": "0",
	}, "issues.create", "github", "@acme/custom-crm", "private provider failure", "logical-request-1")
}

// TestExecuteUnifiedRejectsUnderlyingOperationBeforeResolution proves a
// wrapper cannot bypass the token policy of either physical operation.
func TestExecuteUnifiedRejectsUnderlyingOperationBeforeResolution(t *testing.T) {
	exporter := setupTestTracer(t)
	server, runtime, appID := newUnifiedRuntimeServer(t, store.AppTokenPolicy{AllowedOperations: []string{"createIssue"}})
	_, err := server.ExecuteUnified(grpcTestContext(appID), unifiedRuntimeRequest())
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("ExecuteUnified() error = %v, want PermissionDenied", err)
	}
	if runtime.resolveCalls != 0 || runtime.executeCalls != 0 {
		t.Fatalf("predispatch rejection touched runtime: resolve=%d execute=%d", runtime.resolveCalls, runtime.executeCalls)
	}
	assertUnifiedWrapperTelemetry(t, exporter, map[string]string{
		"unified.schema_version": "3", "unified.stage": "validation", "unified.outcome": "rejected",
		"unified.target_count": "2", "unified.error_code": "operation_not_allowed",
	}, "issues.create", "github", "@acme/custom-crm", "Bug", "sandbox", "user-1", "githubOAuth", "logical-request-1")
}

// TestExecuteUnifiedRejectsInvalidInputBeforeResolution keeps schema-invalid
// logical input outside endpoint resolution and provider accounting.
func TestExecuteUnifiedRejectsInvalidInputBeforeResolution(t *testing.T) {
	server, runtime, appID := newUnifiedRuntimeServer(t, store.AppTokenPolicy{AllowAll: true})
	request := unifiedRuntimeRequest()
	request.InputJson = []byte(`{"title":7}`)
	_, err := server.ExecuteUnified(grpcTestContext(appID), request)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("ExecuteUnified() error = %v, want InvalidArgument", err)
	}
	if runtime.resolveCalls != 0 || runtime.executeCalls != 0 {
		t.Fatalf("invalid input touched runtime: resolve=%d execute=%d", runtime.resolveCalls, runtime.executeCalls)
	}
}

// TestExecuteUnifiedUsesServiceSelectorForEveryAlias proves one end-user
// selector is applied to each selected graph step backed by that service.
func TestExecuteUnifiedUsesServiceSelectorForEveryAlias(t *testing.T) {
	server, runtime, appID := newUnifiedRuntimeServerFromFixture(t, newUnifiedAliasedCompileFixture(), store.AppTokenPolicy{AllowAll: true})
	request := unifiedRuntimeRequest()
	request.Targets = []string{"github_create", "github_lookup"}
	request.TargetSelectors = map[string]*enginev1.ExecutionSelectors{
		"github": {EndUserRef: "user-a", AuthType: "oauth", AuthName: "githubOAuth"},
	}
	response, err := server.ExecuteUnified(grpcTestContext(appID), request)
	if err != nil {
		t.Fatalf("ExecuteUnified() error = %v", err)
	}
	if len(response.GetResults()) != 2 || runtime.executeCalls != 2 || runtime.selectorChecks != 2 {
		t.Fatalf("result/runtime counts = %d/%d/%d", len(response.GetResults()), runtime.executeCalls, runtime.selectorChecks)
	}
	for index, call := range runtime.calls {
		if call.credentials["fused_end_user_ref"] != "user-a" {
			t.Fatalf("call %d credentials = %#v", index, call.credentials)
		}
	}
}

// TestExecuteUnifiedRejectsAliasAsSelectorTarget keeps result namespaces from
// becoming independent credential-routing authority.
func TestExecuteUnifiedRejectsAliasAsSelectorTarget(t *testing.T) {
	server, runtime, appID := newUnifiedRuntimeServerFromFixture(t, newUnifiedAliasedCompileFixture(), store.AppTokenPolicy{AllowAll: true})
	request := unifiedRuntimeRequest()
	request.Targets = []string{"github_create", "github_lookup"}
	request.TargetSelectors = map[string]*enginev1.ExecutionSelectors{"github_lookup": {EndUserRef: "user-a"}}
	_, err := server.ExecuteUnified(grpcTestContext(appID), request)
	if status.Code(err) != codes.InvalidArgument || runtime.resolveCalls != 0 || runtime.executeCalls != 0 {
		t.Fatalf("ExecuteUnified() error/calls = %v/%d/%d", err, runtime.resolveCalls, runtime.executeCalls)
	}
}

// TestExecuteUnifiedRejectsInvalidLaterSelectorBeforePhysicalExecution protects the rule that caller routing selectors cannot exceed the compiled endpoint contract.
func TestExecuteUnifiedRejectsInvalidLaterSelectorBeforePhysicalExecution(t *testing.T) {
	server, runtime, appID := newUnifiedRuntimeServer(t, store.AppTokenPolicy{AllowAll: true})
	runtime.rejectEnvironment = "production"
	request := unifiedRuntimeRequest()
	request.TargetSelectors["@acme/custom-crm"] = &enginev1.ExecutionSelectors{Environment: "production"}

	_, err := server.ExecuteUnified(grpcTestContext(appID), request)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("ExecuteUnified() error = %v, want InvalidArgument", err)
	}
	if runtime.resolveCalls != 1 || runtime.selectorChecks != 2 || runtime.executeCalls != 0 {
		t.Fatalf("selector preflight calls = resolve:%d validate:%d execute:%d", runtime.resolveCalls, runtime.selectorChecks, runtime.executeCalls)
	}
}

// TestExecuteUnifiedAllowsIndependentTargetWithUnusedRollback proves an inert
// declaration adds no rollback authorization, resolution, or selector work.
func TestExecuteUnifiedAllowsIndependentTargetWithUnusedRollback(t *testing.T) {
	server, runtime, appID := newUnifiedRuntimeServerFromFixture(t, newUnifiedRollbackCompileFixture(), store.AppTokenPolicy{
		AllowedOperations: []string{"createTicket"},
	})
	request := unifiedRuntimeRequest()
	request.Targets = []string{"@acme/custom-crm"}
	request.TargetSelectors = nil
	response, err := server.ExecuteUnified(grpcTestContext(appID), request)
	if err != nil || len(response.GetResults()) != 1 || response.GetResults()[0].GetStatus() != "success" {
		t.Fatalf("ExecuteUnified() = (%#v, %v)", response, err)
	}
	if len(runtime.bindings) != 1 || runtime.selectorChecks != 1 || runtime.executeCalls != 1 {
		t.Fatalf("inactive rollback work = bindings:%d selectors:%d executions:%d", len(runtime.bindings), runtime.selectorChecks, runtime.executeCalls)
	}
}

// TestExecuteUnifiedRequiresSelectedDependency rejects a hidden dependency
// before resolution or provider dispatch.
func TestExecuteUnifiedRequiresSelectedDependency(t *testing.T) {
	server, runtime, appID := newUnifiedRuntimeServerFromFixture(t, newUnifiedRollbackCompileFixture(), store.AppTokenPolicy{AllowAll: true})
	request := unifiedRuntimeRequest()
	request.Targets = []string{"github"}
	request.TargetSelectors = nil
	_, err := server.ExecuteUnified(grpcTestContext(appID), request)
	if status.Code(err) != codes.InvalidArgument || runtime.resolveCalls != 0 || runtime.executeCalls != 0 {
		t.Fatalf("dependency preflight = error:%v resolve:%d execute:%d", err, runtime.resolveCalls, runtime.executeCalls)
	}
}

// TestExecuteUnifiedAuthorizesActiveRollbackBeforeResolution proves a selected
// consumer cannot gain compensation authority from its wrapper.
func TestExecuteUnifiedAuthorizesActiveRollbackBeforeResolution(t *testing.T) {
	server, runtime, appID := newUnifiedRuntimeServerFromFixture(t, newUnifiedRollbackCompileFixture(), store.AppTokenPolicy{
		AllowedOperations: []string{"createTicket", "createIssue"},
	})
	request := unifiedRuntimeRequest()
	request.TargetSelectors = nil
	_, err := server.ExecuteUnified(grpcTestContext(appID), request)
	if status.Code(err) != codes.PermissionDenied || runtime.resolveCalls != 0 || runtime.executeCalls != 0 {
		t.Fatalf("rollback authorization = error:%v resolve:%d execute:%d", err, runtime.resolveCalls, runtime.executeCalls)
	}
}

// TestUnifiedChildReplayIdentityIncludesRoutingSelectors protects the rule that caller routing selectors cannot exceed the compiled endpoint contract.
func TestUnifiedChildReplayIdentityIncludesRoutingSelectors(t *testing.T) {
	appID := uuid.New()
	firstKey, firstHash := deriveUnifiedChildIdentity(appID, "issues.create", "github", "forward", "logical-1", []byte(`{"title":"Bug"}`), &enginev1.ExecutionSelectors{Environment: "sandbox"})
	secondKey, secondHash := deriveUnifiedChildIdentity(appID, "issues.create", "github", "forward", "logical-1", []byte(`{"title":"Bug"}`), &enginev1.ExecutionSelectors{Environment: "production"})
	if firstKey != secondKey {
		t.Fatal("routing selectors must not create a second logical child idempotency key")
	}
	if firstHash == secondHash {
		t.Fatal("routing selectors must participate in child replay conflict detection")
	}
}

// newUnifiedRuntimeServer builds a deterministic Unified RPC execution fixture with exact identities and isolated state.
func newUnifiedRuntimeServer(t *testing.T, policy store.AppTokenPolicy) (*EngineGRPCServer, *unifiedRuntimeTestDouble, uuid.UUID) {
	t.Helper()
	return newUnifiedRuntimeServerFromFixture(t, newUnifiedCompileFixture(), policy)
}

// newUnifiedRuntimeServerFromFixture persists one compiled definition into the
// authenticated runtime used by handler-level preflight tests.
func newUnifiedRuntimeServerFromFixture(t *testing.T, fixture unifiedCompileFixture, policy store.AppTokenPolicy) (*EngineGRPCServer, *unifiedRuntimeTestDouble, uuid.UUID) {
	t.Helper()
	compiled, err := compileSDKUnifiedOperations(context.Background(), fixture.store, fixture.document, fixture.selections, fixture.services)
	if err != nil {
		t.Fatal(err)
	}
	accountID, appID := uuid.New(), uuid.New()
	scope := &store.AppRuntime{
		AccountID: accountID, AppID: appID, BucketID: uuid.New(), Kind: store.AppKindSDK,
		UnifiedDefinitionSchemaVersion: store.UnifiedDefinitionSchemaVersion,
		UnifiedDefinitions:             compiled.DefinitionJSON, UnifiedDefinitionHash: compiled.DefinitionHash,
	}
	runtimeStore := &grpcRuntimeStore{Store: &workspaceTestStore{}, accountID: accountID, appID: appID, scope: scope}
	identity := auth.RuntimeIdentity{
		AccountID: accountID, AppFamilyID: uuid.New(), AppID: appID, AppVersion: "1.0.0",
		Kind: store.AppKindSDK, Status: store.AppStatusActive, TokenPolicy: policy,
	}
	server := NewEngineGRPCServer(runtimeStore, nil, nil, nil, nil, unifiedTestValidator{identity: identity})
	runtime := &unifiedRuntimeTestDouble{}
	server.unifiedRuntime = runtime
	return server, runtime, appID
}

// unifiedRuntimeRequest returns a valid two-target request whose selectors and
// input exercise both static and connected-auth routing paths.
func unifiedRuntimeRequest() *enginev1.ExecuteUnifiedRequest {
	return &enginev1.ExecuteUnifiedRequest{
		Operation: "issues.create", Targets: []string{"github", "@acme/custom-crm"},
		InputJson: []byte(`{"title":"Bug"}`), IdempotencyKey: "logical-request-1",
		TargetSelectors: map[string]*enginev1.ExecutionSelectors{
			"github": {Environment: "sandbox", EndUserRef: "user-1", AuthType: "oauth", AuthName: "githubOAuth"},
		},
	}
}

// assertUnifiedResults compares stable target order and canonical public JSON
// without inspecting private provider response storage.
func assertUnifiedResults(t *testing.T, response *enginev1.ExecuteUnifiedResponse, targets, data []string) {
	t.Helper()
	if len(response.GetResults()) != len(targets) {
		t.Fatalf("result count = %d, want %d", len(response.GetResults()), len(targets))
	}
	for index, result := range response.GetResults() {
		if result.GetTarget() != targets[index] || result.GetStatus() != "success" || string(result.GetDataJson()) != data[index] || result.GetErrorCode() != "" {
			t.Fatalf("result %d = %#v", index, result)
		}
	}
}

// assertUnifiedChildRequests proves mapped params and environments reach the
// correct physical endpoints regardless of concurrent start order.
func assertUnifiedChildRequests(t *testing.T, calls []unifiedRuntimeCall) {
	t.Helper()
	if len(calls) != 2 {
		t.Fatalf("child calls = %d, want 2", len(calls))
	}
	seenKeys := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		if call.idempotency == "" || call.requestHash == "" {
			t.Fatalf("child identity is incomplete: %#v", call)
		}
		seenKeys[call.idempotency] = struct{}{}
	}
	if len(seenKeys) != 2 {
		t.Fatal("each target must receive a distinct deterministic child idempotency key")
	}
	assertUnifiedGitHubSelectors(t, calls)
}

// assertUnifiedGitHubSelectors checks that OAuth/OIDC routing fields remain
// attached to the selected target rather than leaking across fan-out calls.
func assertUnifiedGitHubSelectors(t *testing.T, calls []unifiedRuntimeCall) {
	t.Helper()
	for _, call := range calls {
		if _, isGitHub := call.params["title"]; isGitHub {
			if call.environment != "sandbox" || call.credentials["fused_end_user_ref"] != "user-1" || call.credentials["fused_auth_name"] != "githubOAuth" {
				t.Fatalf("selectors were not forwarded as Engine routing values: %#v", call)
			}
		}
	}
}

// assertUnifiedWrapperTelemetry finds the one logical wrapper span and permits
// only bounded outcome/count attributes.
func assertUnifiedWrapperTelemetry(t *testing.T, exporter *tracetest.InMemoryExporter, expected map[string]string, forbidden ...string) {
	t.Helper()
	for _, span := range exporter.GetSpans() {
		if span.Name != "engine.unified.execute" {
			continue
		}
		if len(span.Events) != 0 {
			t.Fatal("Unified wrapper must not record raw error events")
		}
		assertUnifiedSpanAttributes(t, span.Attributes, expected, forbidden)
		return
	}
	t.Fatal("engine.unified.execute span was not emitted")
}

// assertUnifiedSpanAttributes compares the wrapper allowlist while rejecting
// caller input, selectors, mappings, and provider payloads.
func assertUnifiedSpanAttributes(t *testing.T, attributes []attribute.KeyValue, expected map[string]string, forbidden []string) {
	t.Helper()
	actual := make(map[string]string, len(attributes))
	for _, item := range attributes {
		key, value := string(item.Key), item.Value.Emit()
		if _, allowed := expected[key]; !allowed {
			t.Fatalf("Unified telemetry emitted unexpected attribute %q", key)
		}
		actual[key] = value
		assertUnifiedAttributeSafe(t, key, value, forbidden)
	}
	for key, value := range expected {
		if actual[key] != value {
			t.Fatalf("Unified telemetry %s = %q, want %q; all=%#v", key, actual[key], value, actual)
		}
	}
}

// assertUnifiedAttributeSafe scans one attribute for forbidden sensitive fixture values.
func assertUnifiedAttributeSafe(t *testing.T, key, value string, forbidden []string) {
	t.Helper()
	for _, secret := range forbidden {
		if strings.Contains(key, secret) || strings.Contains(value, secret) {
			t.Fatalf("Unified telemetry leaked request/config value %q in %s=%s", secret, key, value)
		}
	}
}
