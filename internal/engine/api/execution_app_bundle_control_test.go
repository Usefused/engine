package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type unifiedAppBundleControlStore struct {
	store.Store
	app        store.App
	family     store.AppFamily
	endpoint   fusedobject.Endpoint
	created    store.UnifiedAppBundle
	queryCalls int
	writeCalls int
}

// GetApp returns only the exact version owned by this control fixture.
func (fixture *unifiedAppBundleControlStore) GetApp(_ context.Context, appID uuid.UUID) (*store.App, error) {
	// A wrong app identity must never inherit fixture authorization.
	if appID != fixture.app.AppID {
		return nil, store.ErrAppNotFound
	}
	return &fixture.app, nil
}

// GetAppFamily returns the immutable delivery kind for this test version.
func (fixture *unifiedAppBundleControlStore) GetAppFamily(_ context.Context, familyID uuid.UUID) (*store.AppFamily, error) {
	// Only the version's own family can establish Unified App delivery.
	if familyID != fixture.family.AppFamilyID {
		return nil, store.ErrAppFamilyNotFound
	}
	return &fixture.family, nil
}

// ListServiceContractEndpointsForSelections records one set-based exact-scope query.
func (fixture *unifiedAppBundleControlStore) ListServiceContractEndpointsForSelections(_ context.Context, selections []store.ServiceContractEndpointSelection, _ []string) ([]store.ServiceContractEndpointMatch, error) {
	fixture.queryCalls++
	// A valid manifest should issue one exact selection with a narrowed endpoint name.
	if len(selections) != 1 || len(selections[0].EndpointNames) != 1 {
		return nil, nil
	}
	return []store.ServiceContractEndpointMatch{{SelectionIndex: selections[0].SelectionIndex, Endpoint: fixture.endpoint}}, nil
}

// CreateUnifiedAppBundle captures only a fully admitted exact-version artifact.
func (fixture *unifiedAppBundleControlStore) CreateUnifiedAppBundle(_ context.Context, bundle store.UnifiedAppBundle) error {
	fixture.writeCalls++
	fixture.created = bundle
	return nil
}

// GetUnifiedAppBundle completes the narrow bundle interface without serving this mutation test.
func (fixture *unifiedAppBundleControlStore) GetUnifiedAppBundle(context.Context, uuid.UUID) (*store.UnifiedAppBundle, error) {
	return nil, store.ErrUnifiedAppBundleNotFound
}

// TestUnifiedAppBundleHandlerAdmitsExactSelectedOperation proves one authorized bundle reaches immutable storage.
func TestUnifiedAppBundleHandlerAdmitsExactSelectedOperation(t *testing.T) {
	requireCapabilityWorkerForDarwin(t)
	fixture, router, body := newUnifiedAppBundleControlFixture(t)
	response := issueUnifiedAppBundleAttach(router, fixture.app.AppID, body)
	// Successful admission performs one batched scope read and one immutable write.
	if response.Code != http.StatusOK || fixture.queryCalls != 1 || fixture.writeCalls != 1 || fixture.created.AppID != fixture.app.AppID {
		t.Fatalf("attach status=%d queries=%d writes=%d bundle=%#v body=%s", response.Code, fixture.queryCalls, fixture.writeCalls, fixture.created, response.Body.String())
	}
}

// TestUnifiedAppBundleHandlerRequiresManage rejects an authenticated actor without app management authority.
func TestUnifiedAppBundleHandlerRequiresManage(t *testing.T) {
	fixture, _, body := newUnifiedAppBundleControlFixture(t)
	router := chi.NewRouter()
	router.Post("/apps/{app_id}/bundle", UnifiedAppBundleHandler(fixture))
	actor := accesscontrol.Actor{AccountID: fixture.app.AccountID, SubjectID: uuid.New(), WorkspaceID: uuid.New()}
	encoded, _ := json.Marshal(body)
	request := httptest.NewRequest(http.MethodPost, "/apps/"+fixture.app.AppID.String()+"/bundle", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	// Authentication alone cannot attach authored code or even query selected operations.
	if response.Code != http.StatusForbidden || fixture.queryCalls != 0 || fixture.writeCalls != 0 {
		t.Fatalf("unauthorized attach status=%d queries=%d writes=%d", response.Code, fixture.queryCalls, fixture.writeCalls)
	}
}

// TestUnifiedAppBundleInspectionDistinguishesWorkerFailure preserves a retryable deployment error.
func TestUnifiedAppBundleInspectionDistinguishesWorkerFailure(t *testing.T) {
	worker := unifiedAppBundleInspectionError(sandbox.ErrCapabilityWorkerUnavailable).(workspaceConfigHTTPError)
	invalid := unifiedAppBundleInspectionError(errors.New("invalid declaration")).(workspaceConfigHTTPError)
	// Isolation failure is a service error while an invalid bundle remains a caller error.
	if worker.status != http.StatusServiceUnavailable || invalid.status != http.StatusBadRequest {
		t.Fatalf("worker status=%d invalid status=%d", worker.status, invalid.status)
	}
}

// TestUnifiedAppBundleHandlerRejectsSourceMismatch protects the applied version's source identity.
func TestUnifiedAppBundleHandlerRejectsSourceMismatch(t *testing.T) {
	fixture, router, body := newUnifiedAppBundleControlFixture(t)
	body.SourceHash = "sha256:other"
	response := issueUnifiedAppBundleAttach(router, fixture.app.AppID, body)
	// A changed source cannot reach contract lookup or artifact storage.
	if response.Code != http.StatusConflict || fixture.queryCalls != 0 || fixture.writeCalls != 0 {
		t.Fatalf("source mismatch status=%d queries=%d writes=%d", response.Code, fixture.queryCalls, fixture.writeCalls)
	}
}

// TestUnifiedAppBundleHandlerRejectsUnpinnedCode keeps legacy app versions from gaining unreviewed behavior.
func TestUnifiedAppBundleHandlerRejectsUnpinnedCode(t *testing.T) {
	fixture, router, body := newUnifiedAppBundleControlFixture(t)
	fixture.app.BundleDigest = ""
	response := issueUnifiedAppBundleAttach(router, fixture.app.AppID, body)
	// Missing immutable code identity fails before script inspection, scope lookup, or storage.
	if response.Code != http.StatusConflict || fixture.queryCalls != 0 || fixture.writeCalls != 0 {
		t.Fatalf("unpinned attach status=%d queries=%d writes=%d", response.Code, fixture.queryCalls, fixture.writeCalls)
	}
}

// TestUnifiedAppBundleHandlerRejectsChangedBytes proves app.manage cannot attach other code under the same source label.
func TestUnifiedAppBundleHandlerRejectsChangedBytes(t *testing.T) {
	fixture, router, body := newUnifiedAppBundleControlFixture(t)
	body.BundleJS += "globalThis.changed = true;"
	response := issueUnifiedAppBundleAttach(router, fixture.app.AppID, body)
	// Digest mismatch is a version conflict even when source_hash and submitted manifest match.
	if response.Code != http.StatusConflict || fixture.queryCalls != 0 || fixture.writeCalls != 0 {
		t.Fatalf("changed code status=%d queries=%d writes=%d", response.Code, fixture.queryCalls, fixture.writeCalls)
	}
}

// TestUnifiedAppBundleHandlerRejectsUnselectedEndpoint protects exact physical scope before persistence.
func TestUnifiedAppBundleHandlerRejectsUnselectedEndpoint(t *testing.T) {
	requireCapabilityWorkerForDarwin(t)
	fixture, router, body := newUnifiedAppBundleControlFixture(t)
	fixture.endpoint.ID = uuid.New()
	response := issueUnifiedAppBundleAttach(router, fixture.app.AppID, body)
	// A snapshot endpoint ID mismatch is a denied operation, not a partial deployment.
	if response.Code != http.StatusBadRequest || fixture.queryCalls != 1 || fixture.writeCalls != 0 {
		t.Fatalf("unselected endpoint status=%d queries=%d writes=%d", response.Code, fixture.queryCalls, fixture.writeCalls)
	}
}

// TestUnifiedAppBundleHandlerRejectsForgedManifest binds selected operation authority to inspected bundle code.
func TestUnifiedAppBundleHandlerRejectsForgedManifest(t *testing.T) {
	requireCapabilityWorkerForDarwin(t)
	fixture, router, body := newUnifiedAppBundleControlFixture(t)
	body.Manifest = json.RawMessage(strings.Replace(string(body.Manifest), `"operation":"customers.create"`, `"operation":"customers.delete"`, 1))
	response := issueUnifiedAppBundleAttach(router, fixture.app.AppID, body)
	// A claimed descriptor different from the executable declaration never reaches scope lookup.
	if response.Code != http.StatusBadRequest || fixture.queryCalls != 0 || fixture.writeCalls != 0 {
		t.Fatalf("forged manifest status=%d queries=%d writes=%d", response.Code, fixture.queryCalls, fixture.writeCalls)
	}
}

// TestUnifiedAppBundleHandlerRejectsUnsafeSearchPath protects the immutable fetch allowlist.
func TestUnifiedAppBundleHandlerRejectsUnsafeSearchPath(t *testing.T) {
	requireCapabilityWorkerForDarwin(t)
	fixture, router, body := newUnifiedAppBundleControlFixture(t)
	body.Manifest = json.RawMessage(strings.Replace(string(body.Manifest), `"searchable":["customer.id"]`, `"searchable":["customer..id"]`, 1))
	// Keep compiled and submitted declarations equal so this test reaches path admission.
	body.BundleJS = "globalThis.FusedExecutionManifest = " + string(body.Manifest) + "; globalThis.FusedUnifiedApp = {input:{parse(v){return v}},output:{parse(v){return v}},execute:async()=>({})};"
	fixture.app.BundleDigest = store.UnifiedAppBundleDigest([]byte(body.BundleJS))
	response := issueUnifiedAppBundleAttach(router, fixture.app.AppID, body)
	// Unsafe data paths stop before the batched service snapshot read.
	if response.Code != http.StatusBadRequest || fixture.queryCalls != 0 || fixture.writeCalls != 0 {
		t.Fatalf("invalid search path status=%d queries=%d writes=%d", response.Code, fixture.queryCalls, fixture.writeCalls)
	}
}

// requireCapabilityWorkerForDarwin skips process-dependent API tests only when macOS forbids nested isolation.
func requireCapabilityWorkerForDarwin(t *testing.T) {
	t.Helper()
	// Linux CI must prove the production namespace path works; macOS local sandboxes may forbid nesting.
	if goruntime.GOOS == "darwin" && !sandbox.IsCapabilityWorkerAvailable(context.Background()) {
		t.Skip("nested capability worker isolation is unavailable on this Darwin host")
	}
}

// newUnifiedAppBundleControlFixture builds one active app with a single exact selected service operation.
func newUnifiedAppBundleControlFixture(t *testing.T) (*unifiedAppBundleControlStore, chi.Router, unifiedAppBundleAttachRequest) {
	t.Helper()
	accountID, familyID, appID := uuid.New(), uuid.New(), uuid.New()
	serviceID, versionID, endpointID := uuid.New(), uuid.New(), uuid.New()
	selections, err := json.Marshal([]models.SDKSelection{{
		ServiceID: serviceID, ServiceVersionID: versionID, SchemaVersion: models.AppSelectionSchemaVersion,
		EndpointIDs: []uuid.UUID{endpointID},
	}})
	// The fixture must use the same versioned scope codec as production app rows.
	if err != nil {
		t.Fatal(err)
	}
	fixture := &unifiedAppBundleControlStore{
		app: store.App{AppID: appID, AppFamilyID: familyID, AccountID: accountID, SourceHash: "sha256:source", Status: store.AppStatusActive,
			ScopeSchemaVersion: models.AppScopeSchemaVersion, Selections: selections},
		family:   store.AppFamily{AppFamilyID: familyID, AccountID: accountID, Kind: store.AppKindUnifiedApp},
		endpoint: fusedobject.Endpoint{ID: endpointID, Name: "customers.create"},
	}
	manifest, err := json.Marshal(unifiedAppManifest{SchemaVersion: 1,
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		Searchable: []string{"customer.id"}, SelectedOperations: []unifiedAppManifestOperation{{
			Service: "crm", Operation: "customers.create", ServiceID: serviceID, ServiceVersionID: versionID, EndpointID: endpointID,
		}},
	})
	// The manifest must carry the same typed wire shape as authoring output.
	if err != nil {
		t.Fatal(err)
	}
	router := newControlTestRouter(accountID)
	router.Post("/apps/{app_id}/bundle", UnifiedAppBundleHandler(fixture))
	body := unifiedAppBundleAttachRequest{SourceHash: fixture.app.SourceHash, BundleJS: "globalThis.FusedExecutionManifest = " + string(manifest) + "; globalThis.FusedUnifiedApp = {input:{parse(v){return v}},output:{parse(v){return v}},execute:async()=>({})};", Manifest: manifest}
	fixture.app.BundleDigest = store.UnifiedAppBundleDigest([]byte(body.BundleJS))
	return fixture, router, body
}

// issueUnifiedAppBundleAttach submits one bounded control document without network I/O.
func issueUnifiedAppBundleAttach(router chi.Router, appID uuid.UUID, body unifiedAppBundleAttachRequest) *httptest.ResponseRecorder {
	encoded, _ := json.Marshal(body)
	request := httptest.NewRequest(http.MethodPost, "/apps/"+appID.String()+"/bundle", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}
