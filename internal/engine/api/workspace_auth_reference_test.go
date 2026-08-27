package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
)

type workspaceStoreWithoutAuthBindings struct {
	store.Store
}

// TestApplyPreparedWorkspaceAuthBindingsAllowsEmptyCurrentStore proves an
// auth-free apply does not require a persistence capability it will not use.
func TestApplyPreparedWorkspaceAuthBindingsAllowsEmptyCurrentStore(t *testing.T) {
	err := applyPreparedWorkspaceAuthBindings(context.Background(), workspaceStoreWithoutAuthBindings{}, nil)
	// Empty state is an intentional no-op, not a request for a legacy writer.
	if err != nil {
		t.Fatalf("empty auth binding apply error = %v", err)
	}
}

// TestApplyPreparedWorkspaceAuthBindingsRejectsStoreWithoutCurrentCapability
// proves credential mutations fail closed instead of using per-secret writes.
func TestApplyPreparedWorkspaceAuthBindingsRejectsStoreWithoutCurrentCapability(t *testing.T) {
	binding := store.WorkspaceAuthBinding{BucketID: uuid.New(), TargetServiceID: uuid.New(), TargetAuthType: "basic", TargetAuthName: "basicAuth", TargetKeys: []string{"basicAuth_username"}}
	err := applyPreparedWorkspaceAuthBindings(context.Background(), workspaceStoreWithoutAuthBindings{}, []store.WorkspaceAuthBinding{binding})
	// The atomic owner is mandatory once any credential representation changes.
	if err == nil || !strings.Contains(err.Error(), "workspace auth binding store is unavailable") {
		t.Fatalf("unsupported auth binding apply error = %v", err)
	}
}

// TestPrepareWorkspaceAuthReferenceMapsCompleteBasicBundle proves different
// source and destination names retain username/password roles without values.
func TestPrepareWorkspaceAuthReferenceMapsCompleteBasicBundle(t *testing.T) {
	destination := workspaceDesiredService{Key: "confluence", ServiceID: uuid.New(), Versions: []string{"v1"}}
	source := workspaceDesiredService{Key: "jira", ServiceID: uuid.New(), Versions: []string{"v1"}}
	// An optional destination still has to reference a source bundle that is
	// complete under the source provider's stricter reviewed contract.
	targetAuth := fusedobject.AuthConfig{Name: "confluenceBasic", Type: "http", Scheme: "basic", BasicPasswordMode: authrouting.BasicPasswordOptional}
	sourceAuth := fusedobject.AuthConfig{Name: "basicAuth", Type: "http", Scheme: "basic", BasicPasswordMode: authrouting.BasicPasswordRequired}
	binding := store.WorkspaceAuthBinding{
		BucketID: uuid.New(), TargetServiceID: destination.ServiceID,
		TargetAuthType: "basic", TargetAuthName: targetAuth.Name,
		TargetKeys: []string{"confluenceBasic_username", "confluenceBasic_password"},
	}
	got, err := prepareWorkspaceAuthReference(
		binding, destination,
		workspaceDesiredBucketServiceConfig{ServiceKey: destination.Key, Auth: &WorkspaceAuthConfig{Ref: "${bucket.auth.jira.basicAuth}"}},
		map[string]workspaceDesiredService{source.Key: source},
		map[string]fusedobject.AuthConfigs{workspaceAuthMetadataKey(source, "v1"): {sourceAuth}},
		workspaceStaticAuthSelection{
			Auth: targetAuth, Required: []string{"confluenceBasic_username"}, Optional: []string{"confluenceBasic_password"},
			Keys: []string{"confluenceBasic_username", "confluenceBasic_password"},
		},
	)
	// Contract resolution must complete before any persistence or encryption.
	if err != nil {
		t.Fatalf("prepareWorkspaceAuthReference() error = %v", err)
	}
	if got.Reference == nil || got.Reference.SourceServiceID != source.ServiceID || got.Reference.SourceAuthName != sourceAuth.Name {
		t.Fatalf("source identity changed: %#v", got.Reference)
	}
	wantKeys := []string{"basicAuth_username", "basicAuth_password"}
	// The reference carries storage identities only, while direct secret rows
	// remain absent so source rotation stays dynamic.
	if !reflect.DeepEqual(got.Reference.SourceRequired, wantKeys) || len(got.Secrets) != 0 {
		t.Fatalf("source bundle = %#v, secrets = %#v", got.Reference.SourceRequired, got.Secrets)
	}
}

// TestWorkspaceAuthReferenceTargetRequiresExactStaticSelector keeps persisted
// target identity independent of provider declaration order.
func TestWorkspaceAuthReferenceTargetRequiresExactStaticSelector(t *testing.T) {
	tests := []struct {
		name string
		auth WorkspaceAuthConfig
		want string
	}{
		{name: "missing name", auth: WorkspaceAuthConfig{AuthType: "basic", Ref: "${bucket.auth.jira.basicAuth}"}, want: "requires auth_name"},
		{name: "missing type", auth: WorkspaceAuthConfig{AuthName: "target", Ref: "${bucket.auth.jira.basicAuth}"}, want: "unsupported auth_type"},
		{name: "unsupported type", auth: WorkspaceAuthConfig{AuthType: "interactive", AuthName: "target", Ref: "${bucket.auth.jira.basicAuth}"}, want: "unsupported auth_type"},
	}
	for _, test := range tests {
		// Each invalid target is admitted through the same Engine trust boundary.
		t.Run(test.name, func(t *testing.T) {
			err := validateWorkspaceAuthConfigIntent("confluence", &test.auth)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateWorkspaceAuthConfigIntent() error = %v, want %q", err, test.want)
			}
		})
	}
}

// TestWorkspaceAuthReferenceRejectsIncompatibleSourceType proves the source
// name cannot silently select a different credential family.
func TestWorkspaceAuthReferenceRejectsIncompatibleSourceType(t *testing.T) {
	destination := workspaceDesiredService{Key: "confluence", ServiceID: uuid.New(), Versions: []string{"v1"}}
	source := workspaceDesiredService{Key: "jira", ServiceID: uuid.New(), Versions: []string{"v1"}}
	_, err := prepareWorkspaceAuthReference(
		store.WorkspaceAuthBinding{BucketID: uuid.New(), TargetServiceID: destination.ServiceID, TargetAuthType: "basic", TargetAuthName: "targetBasic"},
		destination,
		workspaceDesiredBucketServiceConfig{ServiceKey: destination.Key, Auth: &WorkspaceAuthConfig{Ref: "${bucket.auth.jira.sharedName}"}},
		map[string]workspaceDesiredService{source.Key: source},
		map[string]fusedobject.AuthConfigs{workspaceAuthMetadataKey(source, "v1"): {{Name: "sharedName", Type: "http", Scheme: "bearer"}}},
		workspaceStaticAuthSelection{
			Auth:     fusedobject.AuthConfig{Name: "targetBasic", Type: "http", Scheme: "basic"},
			Required: []string{"targetBasic_username", "targetBasic_password"}, Keys: []string{"targetBasic_username", "targetBasic_password"},
		},
	)
	// Exact type matching prevents a same-name Bearer token from impersonating
	// the Basic username/password bundle expected by the destination.
	if err == nil || !strings.Contains(err.Error(), `auth name "sharedName" with type "basic"`) {
		t.Fatalf("prepareWorkspaceAuthReference() error = %v", err)
	}
}

// TestFetchWorkspaceAuthConfigsRequestsEveryEnabledVersion proves one Registry
// batch covers all immutable destination and reference-source contracts.
func TestFetchWorkspaceAuthConfigsRequestsEveryEnabledVersion(t *testing.T) {
	destination := workspaceDesiredService{Key: "confluence", ServiceID: uuid.New(), Versions: []string{"v1", "v2"}}
	source := workspaceDesiredService{Key: "jira", ServiceID: uuid.New(), Versions: []string{"v3", "v4"}}
	desired := workspaceDesiredState{
		Services: map[uuid.UUID]workspaceDesiredService{destination.ServiceID: destination, source.ServiceID: source},
		BucketServiceConfigs: []workspaceDesiredBucketServiceConfig{{
			BucketName: "default", ServiceKey: destination.Key, ServiceID: destination.ServiceID,
			Auth: &WorkspaceAuthConfig{AuthType: "basic", AuthName: "targetBasic", Ref: "${bucket.auth.jira.sourceBasic}"},
		}},
	}
	verifier := &mockVerifier{}
	if _, err := fetchWorkspaceAuthConfigs(context.Background(), verifier, "fsk_test", desired); err != nil {
		t.Fatalf("fetchWorkspaceAuthConfigs() error = %v", err)
	}
	// One call with four exact version refs prevents both N+1 fetching and a
	// mutable first-version fallback.
	if len(verifier.authConfigCalls) != 1 || len(verifier.authConfigCalls[0]) != 4 {
		t.Fatalf("auth metadata calls = %#v", verifier.authConfigCalls)
	}
	want := []sandbox.ServiceVersionRef{
		{ServiceID: destination.ServiceID, Version: "v1"}, {ServiceID: destination.ServiceID, Version: "v2"},
		{ServiceID: source.ServiceID, Version: "v3"}, {ServiceID: source.ServiceID, Version: "v4"},
	}
	if !reflect.DeepEqual(verifier.authConfigCalls[0], want) {
		t.Fatalf("auth metadata refs = %#v, want %#v", verifier.authConfigCalls[0], want)
	}
}

// TestWorkspaceStaticAuthSelectionRejectsVersionDrift proves a service-scoped
// binding cannot hide incompatible immutable Basic credential shapes.
func TestWorkspaceStaticAuthSelectionRejectsVersionDrift(t *testing.T) {
	service := workspaceDesiredService{Key: "jira", ServiceID: uuid.New(), Versions: []string{"v1", "v2"}}
	configs := map[string]fusedobject.AuthConfigs{
		workspaceAuthMetadataKey(service, "v1"): {{Name: "basicAuth", Type: "http", Scheme: "basic", BasicPasswordMode: authrouting.BasicPasswordRequired}},
		workspaceAuthMetadataKey(service, "v2"): {{Name: "basicAuth", Type: "http", Scheme: "basic", BasicPasswordMode: authrouting.BasicPasswordOptional}},
	}
	_, err := workspaceStaticAuthSelectionForService(configs, service, "basic", "basicAuth")
	var httpErr workspaceConfigHTTPError
	// Required-versus-optional password semantics change source completeness
	// and therefore must fail before any local workspace write.
	if !errors.As(err, &httpErr) || httpErr.code != "workspace_auth_contract_drift" || httpErr.recovery != "" {
		t.Fatalf("version drift error = %#v", err)
	}
}

// TestRemovedWorkspaceBucketServiceEntryEmitsExplicitClear proves deleting the
// entire desired service_config entry still reaches atomic reconciliation.
func TestRemovedWorkspaceBucketServiceEntryEmitsExplicitClear(t *testing.T) {
	serviceID := uuid.New()
	payload, err := json.Marshal(map[string]any{
		"kind":     "workspace",
		"services": workspaceTestServicePayload(serviceID),
		"buckets": map[string]any{
			"default": map[string]any{"service_config": map[string]any{}},
		},
	})
	// Parsing the real wire document is essential here because the defect was
	// normalization dropping an absent map entry before reconciliation.
	if err != nil {
		t.Fatalf("marshal workspace payload: %v", err)
	}
	desired, err := parseWorkspaceConfig(payload)
	if err != nil {
		t.Fatalf("parseWorkspaceConfig() error = %v", err)
	}
	// An absent service_config.github entry must survive as nil-auth desired
	// state rather than becoming indistinguishable from an unmanaged no-op.
	if len(desired.BucketServiceConfigs) != 1 {
		t.Fatalf("normalized removed entry = %#v", desired.BucketServiceConfigs)
	}
	normalized := desired.BucketServiceConfigs[0]
	if normalized.ServiceID != serviceID || normalized.Auth != nil {
		t.Fatalf("normalized removed entry = %#v", normalized)
	}
	testStore := &workspaceTestStore{}
	bindings, err := prepareWorkspaceAuthBindings(context.Background(), testStore, &mockVerifier{}, "fsk_test", desired, nil, testMasterKey)
	if err != nil {
		t.Fatalf("prepareWorkspaceAuthBindings() error = %v", err)
	}
	// Service-wide clear targets only reference rows; empty direct material is
	// the store-level invariant that preserves independently stored secrets.
	assertWorkspaceAuthClearBinding(t, bindings, "removed-entry")
}

// TestPrepareWorkspaceAuthBindingsEmitsExplicitClear proves an omitted auth
// block removes a stale reference instead of silently preserving it.
func TestPrepareWorkspaceAuthBindingsEmitsExplicitClear(t *testing.T) {
	service := workspaceDesiredService{Key: "jira", ServiceID: uuid.New(), Versions: []string{"v1"}}
	desired := workspaceDesiredState{
		Services:             map[uuid.UUID]workspaceDesiredService{service.ServiceID: service},
		BucketServiceConfigs: []workspaceDesiredBucketServiceConfig{{BucketName: "default", ServiceKey: service.Key, ServiceID: service.ServiceID}},
	}
	testStore := &workspaceTestStore{}
	bindings, err := prepareWorkspaceAuthBindings(context.Background(), testStore, &mockVerifier{}, "fsk_test", desired, nil, testMasterKey)
	if err != nil {
		t.Fatalf("prepareWorkspaceAuthBindings() error = %v", err)
	}
	// Clear is deliberately service-wide because workspace service_config owns
	// exactly one auth family for a service within a bucket.
	assertWorkspaceAuthClearBinding(t, bindings, "auth-omission")
	// Secret-safe telemetry must still distinguish an intentional clear from an
	// auth-free no-op without exporting the target identity.
	if workspaceAuthClearCount(bindings) != 1 {
		t.Fatalf("clear telemetry count = %d, want 1", workspaceAuthClearCount(bindings))
	}
}

// assertWorkspaceAuthClearBinding centralizes the identity-free service-wide
// shape required to clear references while preserving direct secret rows.
func assertWorkspaceAuthClearBinding(t *testing.T, bindings []store.WorkspaceAuthBinding, source string) {
	t.Helper()
	// A single scoped clear is the only accepted representation of one removed
	// workspace auth intent; material on it would conflate deletion and writes.
	if len(bindings) != 1 || !bindings[0].ReconcileReferences || !bindings[0].ClearReferences || bindings[0].Reference != nil || len(bindings[0].Secrets) != 0 {
		t.Fatalf("%s clear binding = %#v", source, bindings)
	}
}

// TestWorkspaceApplyErrorOmitsPathlessReferenceRecovery proves Engine leaves
// exact local-file recovery to the CLI that knows the apply source path.
func TestWorkspaceApplyErrorOmitsPathlessReferenceRecovery(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{name: "invalid", err: store.ErrWorkspaceAuthReferenceInvalid, code: "workspace_auth_reference_invalid"},
		{name: "in use", err: store.ErrWorkspaceAuthReferenceInUse, code: "workspace_auth_reference_in_use"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var httpErr workspaceConfigHTTPError
			// Engine cannot identify a caller-local file, so a pathless command would
			// knowingly fail to reproduce an explicit `workspace apply -f` request.
			if err := workspaceApplyError(context.Background(), test.err); !errors.As(err, &httpErr) || httpErr.code != test.code || httpErr.recovery != "" {
				t.Fatalf("workspaceApplyError() = %#v", err)
			}
		})
	}
}

// TestWorkspaceConfigApplyPreflightFailureIsNotCommitted proves typed auth
// graph rejection releases the lease and prevents every downstream write.
func TestWorkspaceConfigApplyPreflightFailureIsNotCommitted(t *testing.T) {
	serviceID, bucketID, planID := uuid.New(), uuid.New(), uuid.New()
	payload, err := json.Marshal(map[string]any{
		"kind": "workspace", "services": workspaceTestServicePayload(serviceID),
		"buckets": map[string]any{"default": map[string]any{"service_config": map[string]any{"github": map[string]any{}}}},
	})
	if err != nil {
		t.Fatalf("marshal workspace payload: %v", err)
	}
	testStore := &workspaceTestStore{
		accountID: uuid.New(), workspaceID: uuid.New(), workspaceAuthPreflightErr: store.ErrWorkspaceAuthReferenceInvalid,
		bucketsByName: map[string]*store.Bucket{"default": {ID: bucketID, Name: "default"}},
	}
	configStore := workspaceApplyConfigStore(planID, payload)
	router := newControlTestRouter(testStore.accountID)
	router.Post("/workspace/config/apply", WorkspaceConfigApplyHandler(configStore, testStore, &mockRegistryClient{name: "GitHub"}, testMasterKey))
	request := httptest.NewRequest(http.MethodPost, "/workspace/config/apply", bytes.NewReader([]byte(`{"plan_id":"`+planID.String()+`","source_hash":"abc"}`)))
	request.Header.Set("X-API-Key", "fsk_test")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	// Omitempty must remove the field entirely so direct API clients do not
	// mistake a pathless command for an exact recovery contract.
	if bytes.Contains(response.Body.Bytes(), []byte(`"recovery"`)) {
		t.Fatalf("preflight response exposed pathless recovery: %s", response.Body.String())
	}
	var envelope workspaceConfigErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode preflight error: %v", err)
	}
	assertWorkspacePreflightFailureEnvelope(t, response.Code, envelope, planID)
	assertWorkspacePreflightNoWrites(t, configStore, testStore)
}

// assertWorkspacePreflightFailureEnvelope keeps the complete slim outcome
// contract in one bounded assertion shared by admission tests.
func assertWorkspacePreflightFailureEnvelope(t *testing.T, status int, envelope workspaceConfigErrorResponse, planID uuid.UUID) {
	t.Helper()
	// Admission failures prove rollback and retain durable plan identity, while
	// exact file-aware recovery remains absent at this server-only boundary.
	if status != http.StatusConflict || envelope.Error.Code != "workspace_auth_reference_invalid" || envelope.Error.Phase != "apply_admission" || envelope.Error.CommitState != "not_committed" || envelope.Error.OperationID != planID.String() || envelope.Error.Recovery != "" {
		t.Fatalf("preflight response = status %d, error %#v", status, envelope.Error)
	}
}

// assertWorkspacePreflightNoWrites proves admission rejection cannot leak into
// lease completion, service mutation, or either credential representation.
func assertWorkspacePreflightNoWrites(t *testing.T, configStore *mockConfigStore, testStore *workspaceTestStore) {
	t.Helper()
	// Every mutation signal must stay empty because reference validation runs
	// while the apply lease is still safely releasable.
	if configStore.applyLeaseID != uuid.Nil || testStore.gotServiceName != "" || len(testStore.upsertedSecrets) != 0 || len(testStore.workspaceAuthBindings) != 0 {
		t.Fatalf("preflight leaked mutation: lease=%s service=%q secrets=%d bindings=%d", configStore.applyLeaseID, testStore.gotServiceName, len(testStore.upsertedSecrets), len(testStore.workspaceAuthBindings))
	}
}

// TestRemoveManagedWorkspaceServicesBatchesCompositeSet proves workspace apply
// does not turn target/source deletion into order-dependent single mutations.
func TestRemoveManagedWorkspaceServicesBatchesCompositeSet(t *testing.T) {
	repository := &workspaceTestStore{}
	serviceIDs := []uuid.UUID{uuid.New(), uuid.New()}
	err := removeManagedWorkspaceServices(context.Background(), repository, serviceIDs)
	// The shared test store exposes the same optional batch capability as production.
	if err != nil {
		t.Fatalf("removeManagedWorkspaceServices() error = %v", err)
	}
	// One composite call records the whole set without retrying either service.
	if !reflect.DeepEqual(repository.removedWorkspaceServices, serviceIDs) {
		t.Fatalf("removed workspace services = %v, want %v", repository.removedWorkspaceServices, serviceIDs)
	}
}
