package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

const testImportServiceVersionID = "11111111-1111-1111-1111-111111111111"
const testImportOperationID = "22222222-2222-4222-8222-222222222222"

// committedImportApplyBody supplies the durable Registry proof required before Engine workspace activation.
func committedImportApplyBody(response importApplyResponse) []byte {
	response.Status = "applied"
	response.OperationID = testImportOperationID
	response.Phase = "complete"
	response.CommitState = "committed"
	body, _ := json.Marshal(response)
	return body
}

// TestImportWorkspaceActivationFailureOmitsMissingIdentities ensures incomplete
// Registry proof falls back to status recovery without presenting nil UUIDs.
func TestImportWorkspaceActivationFailureOmitsMissingIdentities(t *testing.T) {
	operationID := uuid.MustParse(testImportOperationID)
	body := importWorkspaceActivationFailure(autoRegistrationAudit{operationID: operationID, outcome: "missing_service_id"}, "request-1")
	var response workspaceConfigErrorResponse
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode partial error: %v", err)
	}
	// Status is the only exact recovery until Registry can return service identity.
	if response.Error.Recovery != "fused-cli import status "+testImportOperationID {
		t.Fatalf("recovery = %q", response.Error.Recovery)
	}
	if _, exists := response.Error.Details["service_id"]; exists {
		t.Fatalf("partial error exposed a nil service identity: %#v", response.Error.Details)
	}
	if _, exists := response.Error.Details["service_version_id"]; exists {
		t.Fatalf("partial error exposed a nil service-version identity: %#v", response.Error.Details)
	}
}

// autoRegisterMockStore records activation calls made for an authenticated Actor.
// AddWorkspaceServiceVersion calls for Task 3's unit tests
// (engine_workspace_registration_plan.md). Embeds store.Store so any method
// not overridden here isn't needed by the code under test.
type autoRegisterMockStore struct {
	store.Store
	accountID uuid.UUID

	workspaceErr       error
	alreadyActivated   bool
	isActivatedErr     error
	activateErr        error
	activateCalls      int
	lastActivateArgs   []any
	enableVersionErr   error
	enableVersionCalls int
	lastEnableArgs     []any
	workspaceCalls     int
}

type snapshotAutoRegisterStore struct {
	*autoRegisterMockStore
	snapshotCalls int
	snapshotErr   error
}

func (m *snapshotAutoRegisterStore) UpsertServiceContractSnapshot(ctx context.Context, snapshot store.ServiceContractSnapshot) (*store.ServiceContractSnapshot, error) {
	m.snapshotCalls++
	return &snapshot, m.snapshotErr
}

type runtimeContractFetcherStub struct {
	snapshot         *store.ServiceContractSnapshot
	err              error
	calls            int
	apiKey           string
	serviceID        uuid.UUID
	serviceVersionID uuid.UUID
	version          string
}

func autoRegisterTestContext(accountID uuid.UUID) context.Context {
	return accesscontrol.ContextWithActor(context.Background(), accesscontrol.Actor{
		AccountID: accountID, WorkspaceID: uuid.New(), SubjectID: uuid.New(), Kind: accesscontrol.SubjectUser,
	})
}

func (f *runtimeContractFetcherStub) FetchRuntimeContract(ctx context.Context, serviceID, serviceVersionID uuid.UUID, version, apiKey string) (*store.ServiceContractSnapshot, error) {
	f.calls++
	f.apiKey = apiKey
	f.serviceID = serviceID
	f.serviceVersionID = serviceVersionID
	f.version = version
	if f.err != nil {
		return nil, f.err
	}
	return f.snapshot, nil
}

func (m *autoRegisterMockStore) GetAccountByAPIKey(ctx context.Context, apiKey string) (uuid.UUID, error) {
	return m.accountID, nil
}

func (m *autoRegisterMockStore) VerifyWorkspaceOwner(ctx context.Context, accountID uuid.UUID) error {
	m.workspaceCalls++
	return m.workspaceErr
}

func (m *autoRegisterMockStore) IsWorkspaceServiceEnabled(ctx context.Context, serviceID uuid.UUID) (bool, error) {
	return m.alreadyActivated, m.isActivatedErr
}

func (s *autoRegisterMockStore) AddWorkspaceServiceVersion(ctx context.Context, serviceID uuid.UUID, serviceSlug string, version string, serviceVersionID uuid.UUID, serviceName string, addedBy uuid.UUID) error {
	s.activateCalls++
	s.lastActivateArgs = []any{serviceID, serviceSlug, version, serviceVersionID, serviceName, addedBy}
	return s.activateErr
}

func (m *autoRegisterMockStore) EnableWorkspaceServiceVersion(ctx context.Context, serviceID uuid.UUID, version string, serviceVersionID uuid.UUID, enabledBy uuid.UUID) error {
	m.enableVersionCalls++
	m.lastEnableArgs = []any{serviceID, version, serviceVersionID, enabledBy}
	return m.enableVersionErr
}

func TestAutoRegisterImportedService_ActivatesWhenNotYetActivated(t *testing.T) {
	accountID := uuid.New()
	serviceID := uuid.New()
	s := &autoRegisterMockStore{accountID: accountID}

	body := committedImportApplyBody(importApplyResponse{ServiceID: serviceID.String(), ServiceVersionID: testImportServiceVersionID, Name: "Stripe Payments", Slug: "stripe", IsNewService: true, Version: "2026-01-01"})
	autoRegisterImportedService(autoRegisterTestContext(accountID), s, nil, accountID, "", body)

	if s.workspaceCalls != 0 {
		t.Fatalf("expected no legacy workspace ownership lookup, got %d", s.workspaceCalls)
	}
	if s.activateCalls != 1 {
		t.Fatalf("expected AddWorkspaceServiceVersion called once, got %d", s.activateCalls)
	}
	got := s.lastActivateArgs
	if got[0] != serviceID || got[1] != "stripe" || got[2] != "2026-01-01" || got[3] != uuid.MustParse(testImportServiceVersionID) || got[4] != "Stripe Payments" || got[5] != accountID {
		t.Errorf("unexpected AddWorkspaceServiceVersion args: %#v", got)
	}
}

func TestAutoRegisterImportedService_FallsBackToSlugWhenNameIsMissing(t *testing.T) {
	accountID := uuid.New()
	serviceID := uuid.New()
	s := &autoRegisterMockStore{accountID: accountID}

	body := committedImportApplyBody(importApplyResponse{ServiceID: serviceID.String(), ServiceVersionID: testImportServiceVersionID, Slug: "stripe", Version: "2026-01-01"})
	autoRegisterImportedService(autoRegisterTestContext(accountID), s, nil, accountID, "", body)

	if got := s.lastActivateArgs[4]; got != "stripe" {
		t.Errorf("expected slug fallback for an older Registry response, got %q", got)
	}
}

func TestAutoRegisterImportedService_MissingSlugFailsClosed(t *testing.T) {
	exporter := setupTestTracer(t)
	accountID := uuid.New()
	serviceID := uuid.New()
	s := &autoRegisterMockStore{accountID: accountID}

	body := committedImportApplyBody(importApplyResponse{ServiceID: serviceID.String(), ServiceVersionID: testImportServiceVersionID, Name: "Stripe Payments", Slug: "  ", Version: "2026-01-01"})
	autoRegisterImportedService(autoRegisterTestContext(accountID), s, nil, accountID, "", body)

	if s.activateCalls != 0 {
		t.Fatalf("activation without a stable Registry slug must fail closed, got %d calls", s.activateCalls)
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected one failed auto-registration audit span, got %d", len(spans))
	}
	for _, attr := range spans[0].Attributes {
		if attr.Key == "outcome" && attr.Value.AsString() == "missing_slug" {
			return
		}
	}
	t.Fatal("missing-slug activation did not emit outcome=missing_slug")
}

func TestAutoRegisterImportedService_EmitsMutationAuditSpan(t *testing.T) {
	exporter := setupTestTracer(t)
	accountID := uuid.New()
	serviceID := uuid.New()
	s := &autoRegisterMockStore{accountID: accountID}

	body := committedImportApplyBody(importApplyResponse{ServiceID: serviceID.String(), ServiceVersionID: testImportServiceVersionID, Slug: "stripe", Version: "2026-01-01"})
	autoRegisterImportedService(autoRegisterTestContext(accountID), s, nil, accountID, "", body)

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected one auto-registration audit span, got %d", len(spans))
	}
	if spans[0].Name != "engine.workspace.auto_register_service" {
		t.Fatalf("unexpected span name %q", spans[0].Name)
	}

	want := map[string]string{
		"user_action":        "workspace.auto_register_service",
		"account_id":         accountID.String(),
		"service_id":         serviceID.String(),
		"service_version_id": testImportServiceVersionID,
		"service_version":    "2026-01-01",
		"outcome":            "activated",
	}
	for _, attr := range spans[0].Attributes {
		key := string(attr.Key)
		if expected, ok := want[key]; ok && attr.Value.AsString() == expected {
			delete(want, key)
		}
	}
	for key, value := range want {
		t.Errorf("expected span attribute %s=%q", key, value)
	}
}

func TestAutoRegisterImportedService_EnablesImportedVersionWhenAlreadyActivated(t *testing.T) {
	accountID := uuid.New()
	serviceID := uuid.New()
	s := &autoRegisterMockStore{accountID: accountID, alreadyActivated: true}

	body := committedImportApplyBody(importApplyResponse{ServiceID: serviceID.String(), ServiceVersionID: testImportServiceVersionID, Slug: "stripe", Version: "2026-01-01"})
	autoRegisterImportedService(autoRegisterTestContext(accountID), s, nil, accountID, "", body)

	if s.activateCalls != 0 {
		t.Errorf("expected AddWorkspaceServiceVersion NOT called when already activated, got %d calls", s.activateCalls)
	}
	if s.enableVersionCalls != 1 {
		t.Fatalf("expected imported version enabled once, got %d calls", s.enableVersionCalls)
	}
	got := s.lastEnableArgs
	if got[0] != serviceID || got[1] != "2026-01-01" || got[2] != uuid.MustParse(testImportServiceVersionID) || got[3] != accountID {
		t.Errorf("unexpected EnableWorkspaceServiceVersion args: %#v", got)
	}
}

func TestAutoRegisterImportedService_MaterializesSnapshotBeforeActivation(t *testing.T) {
	accountID := uuid.New()
	serviceID := uuid.New()
	serviceVersionID := uuid.MustParse(testImportServiceVersionID)
	s := &snapshotAutoRegisterStore{autoRegisterMockStore: &autoRegisterMockStore{accountID: accountID}}
	fetcher := &runtimeContractFetcherStub{snapshot: &store.ServiceContractSnapshot{
		ServiceID:        serviceID,
		ServiceVersionID: serviceVersionID,
		Version:          "2026-01-01",
	}}

	body := committedImportApplyBody(importApplyResponse{ServiceID: serviceID.String(), ServiceVersionID: testImportServiceVersionID, Name: "Stripe Payments", Slug: "stripe", Version: "2026-01-01"})
	autoRegisterImportedService(autoRegisterTestContext(accountID), s, fetcher, accountID, "user-api-key", body)

	if fetcher.calls != 1 || s.snapshotCalls != 1 {
		t.Fatalf("expected one runtime contract fetch and snapshot write, got fetch=%d write=%d", fetcher.calls, s.snapshotCalls)
	}
	if fetcher.serviceID != serviceID || fetcher.serviceVersionID != serviceVersionID || fetcher.version != "2026-01-01" || fetcher.apiKey != "user-api-key" {
		t.Fatalf("unexpected runtime contract fetch args: %#v", fetcher)
	}
	if s.activateCalls != 1 {
		t.Fatalf("expected activation after snapshot materialization, got %d calls", s.activateCalls)
	}
}

func TestAutoRegisterImportedService_SnapshotFetchFailureSkipsActivation(t *testing.T) {
	accountID := uuid.New()
	serviceID := uuid.New()
	s := &snapshotAutoRegisterStore{autoRegisterMockStore: &autoRegisterMockStore{accountID: accountID}}
	fetcher := &runtimeContractFetcherStub{err: context.DeadlineExceeded}

	body := committedImportApplyBody(importApplyResponse{ServiceID: serviceID.String(), ServiceVersionID: testImportServiceVersionID, Slug: "stripe", Version: "2026-01-01"})
	autoRegisterImportedService(autoRegisterTestContext(accountID), s, fetcher, accountID, "user-api-key", body)

	if fetcher.calls != 1 {
		t.Fatalf("expected one runtime contract fetch, got %d", fetcher.calls)
	}
	if s.snapshotCalls != 0 || s.activateCalls != 0 || s.enableVersionCalls != 0 {
		t.Fatalf("snapshot failure must block activation, got snapshot=%d activate=%d enable=%d", s.snapshotCalls, s.activateCalls, s.enableVersionCalls)
	}
}

func TestAutoRegisterImportedService_MalformedJSONNoPanic(t *testing.T) {
	s := &autoRegisterMockStore{accountID: uuid.New()}
	autoRegisterImportedService(autoRegisterTestContext(s.accountID), s, nil, s.accountID, "", []byte(`not json`))

	if s.activateCalls != 0 || s.workspaceCalls != 0 {
		t.Errorf("expected no store calls for malformed JSON, got workspace=%d activate=%d", s.workspaceCalls, s.activateCalls)
	}
}

func TestAutoRegisterImportedService_MissingServiceIDSkips(t *testing.T) {
	s := &autoRegisterMockStore{accountID: uuid.New()}
	autoRegisterImportedService(autoRegisterTestContext(s.accountID), s, nil, s.accountID, "", committedImportApplyBody(importApplyResponse{}))

	if s.activateCalls != 0 || s.workspaceCalls != 0 {
		t.Errorf("expected no store calls when service_id is missing, got workspace=%d activate=%d", s.workspaceCalls, s.activateCalls)
	}
}

func TestAutoRegisterImportedService_MissingVersionSkips(t *testing.T) {
	serviceID := uuid.New()
	s := &autoRegisterMockStore{accountID: uuid.New()}
	body := committedImportApplyBody(importApplyResponse{ServiceID: serviceID.String(), Slug: "stripe"})
	autoRegisterImportedService(autoRegisterTestContext(s.accountID), s, nil, s.accountID, "", body)

	if s.workspaceCalls != 0 || s.activateCalls != 0 {
		t.Errorf("expected no store calls when version is missing, got workspace=%d activate=%d", s.workspaceCalls, s.activateCalls)
	}
}

func TestAutoRegisterImportedService_StoreErrorsDoNotPanic(t *testing.T) {
	serviceID := uuid.New()
	body := committedImportApplyBody(importApplyResponse{ServiceID: serviceID.String(), ServiceVersionID: testImportServiceVersionID, Slug: "stripe", Version: "2026-01-01"})

	t.Run("missing actor fails closed", func(t *testing.T) {
		s := &autoRegisterMockStore{accountID: uuid.New()}
		autoRegisterImportedService(context.Background(), s, nil, s.accountID, "", body)
		if s.activateCalls != 0 {
			t.Errorf("expected AddWorkspaceServiceVersion not reached without an authenticated Actor")
		}
	})

	t.Run("IsWorkspaceServiceEnabled error", func(t *testing.T) {
		s := &autoRegisterMockStore{accountID: uuid.New(), isActivatedErr: context.DeadlineExceeded}
		autoRegisterImportedService(autoRegisterTestContext(s.accountID), s, nil, s.accountID, "", body)
		if s.activateCalls != 0 {
			t.Errorf("expected AddWorkspaceServiceVersion not reached after an IsWorkspaceServiceEnabled error")
		}
	})

	t.Run("AddWorkspaceServiceVersion error", func(t *testing.T) {
		s := &autoRegisterMockStore{accountID: uuid.New(), activateErr: context.DeadlineExceeded}
		// Must not panic.
		autoRegisterImportedService(autoRegisterTestContext(s.accountID), s, nil, s.accountID, "", body)
	})

	t.Run("EnableWorkspaceServiceVersion error", func(t *testing.T) {
		s := &autoRegisterMockStore{
			accountID:        uuid.New(),
			alreadyActivated: true,
			enableVersionErr: context.DeadlineExceeded,
		}
		// Auto-registration is best effort and must not overwrite apply success.
		autoRegisterImportedService(autoRegisterTestContext(s.accountID), s, nil, s.accountID, "", body)
	})
}

// recordingForwarder is a Forwarder mock dedicated to the handler-routing
// tests below: it distinguishes which of Forward/ForwardAndInspect was
// called (mockForwarder in graphql_proxy_test.go implements both the same
// way, which can't tell them apart) and lets a test configure the status/
// body ForwardAndInspect should simulate receiving from the Registry.
type recordingForwarder struct {
	forwardCalled           bool
	forwardAndInspectCalled bool
	forwardPaths            []string
	forwardMethods          []string
	forwardContextErrors    []error
	status                  int
	statuses                []int
	body                    string
	bodies                  []string
}

func (f *recordingForwarder) Forward(w http.ResponseWriter, r *http.Request, stripPrefix string) {
	f.forwardCalled = true
	f.forwardPaths = append(f.forwardPaths, r.URL.Path)
	f.forwardMethods = append(f.forwardMethods, r.Method)
	f.forwardContextErrors = append(f.forwardContextErrors, r.Context().Err())
	status := f.status
	if len(f.statuses) > 0 {
		status = f.statuses[0]
		f.statuses = f.statuses[1:]
	}
	if status == 0 {
		status = http.StatusOK
	}
	body := f.body
	if len(f.bodies) > 0 {
		body = f.bodies[0]
		f.bodies = f.bodies[1:]
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// ForwardAndInspect simulates the production callback-before-write ordering so response replacement is testable.
func (f *recordingForwarder) ForwardAndInspect(w http.ResponseWriter, r *http.Request, stripPrefix string, onSuccess func(*http.Response, []byte)) {
	f.forwardAndInspectCalled = true
	status := f.status
	// Tests default to a successful Registry response unless they explicitly
	// exercise a non-2xx proxy path.
	if status == 0 {
		status = http.StatusOK
	}
	response := &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader([]byte(f.body)))}
	// Only a successful Registry response owns Engine-local follow-up work.
	if status >= 200 && status < 300 && onSuccess != nil {
		onSuccess(response, []byte(f.body))
	}
	for key, values := range response.Header {
		w.Header()[key] = append([]string(nil), values...)
	}
	w.WriteHeader(response.StatusCode)
	body, _ := io.ReadAll(response.Body)
	_, _ = w.Write(body)
}

// TestRESTProxyHandler_ImportApply_RoutesThroughAutoRegister is the
// end-to-end routing guard: POST /integrations/import/apply must use
// ForwardAndInspect (so auto-register can run), while every other proxied
// path keeps using plain Forward, completely unchanged.
func TestRESTProxyHandler_ImportApply_RoutesThroughAutoRegister(t *testing.T) {
	accountID := uuid.New()
	serviceID := uuid.New()
	fwd := &recordingForwarder{body: string(committedImportApplyBody(importApplyResponse{ServiceID: serviceID.String(), ServiceVersionID: testImportServiceVersionID, Slug: "stripe", Version: "2026-01-01"}))}
	s := &autoRegisterMockStore{accountID: accountID}
	handler := RESTProxyHandler(fwd, s)

	req := httptest.NewRequest(http.MethodPost, "/integrations/import/apply", bytes.NewBufferString(`{}`))
	req.Header.Set("X-API-Key", "fsk_valid")
	req = controlTestRequest(req, accountID)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !fwd.forwardAndInspectCalled {
		t.Error("expected ForwardAndInspect to be used for POST /integrations/import/apply")
	}
	if fwd.forwardCalled {
		t.Error("expected plain Forward NOT to be used for POST /integrations/import/apply")
	}
	if s.activateCalls != 1 {
		t.Errorf("expected auto-register to activate the service, got %d AddWorkspaceServiceVersion calls", s.activateCalls)
	}
}

// TestRESTProxyHandler_ImportApplyReportsCommittedActivationFailure proves a Registry success cannot mask local partial state.
func TestRESTProxyHandler_ImportApplyReportsCommittedActivationFailure(t *testing.T) {
	accountID := uuid.New()
	serviceID := uuid.New()
	fwd := &recordingForwarder{body: string(committedImportApplyBody(importApplyResponse{
		ServiceID: serviceID.String(), ServiceVersionID: testImportServiceVersionID,
		Slug: "chargebee", Version: "2026-08-19-webhooks",
	}))}
	store := &snapshotAutoRegisterStore{autoRegisterMockStore: &autoRegisterMockStore{accountID: accountID}}
	fetcher := &runtimeContractFetcherStub{err: context.DeadlineExceeded}
	handler := chimiddleware.RequestID(RESTProxyHandlerWithRuntimeContracts(fwd, store, fetcher))

	request := httptest.NewRequest(http.MethodPost, "/integrations/import/apply", bytes.NewBufferString(`{}`))
	request.Header.Set("X-API-Key", "fsk_valid")
	request.Header.Set("X-Request-ID", "import-chargebee-1")
	request = controlTestRequest(request, accountID)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusFailedDependency {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusFailedDependency, recorder.Body.String())
	}
	var response workspaceConfigErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode partial error: %v", err)
	}
	if response.Error.Code != "import_workspace_activation_failed" || response.Error.CommitState != "committed" || response.Error.Phase != "workspace_activation" {
		t.Fatalf("partial error = %#v", response.Error)
	}
	if response.Error.OperationID != testImportOperationID || response.Error.RequestID == "" || !strings.Contains(response.Error.Recovery, "--service-id") {
		t.Fatalf("recovery contract = %#v", response.Error)
	}
}

func TestRESTProxyHandler_OtherIntegrationsPaths_StillUsePlainForward(t *testing.T) {
	accountID := uuid.New()
	fwd := &recordingForwarder{}
	s := &autoRegisterMockStore{accountID: accountID}
	handler := RESTProxyHandler(fwd, s)

	req := httptest.NewRequest(http.MethodPost, "/integrations", bytes.NewBufferString(`{"name":"stripe"}`))
	req.Header.Set("X-API-Key", "fsk_valid")
	req = controlTestRequest(req, accountID)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !fwd.forwardCalled {
		t.Error("expected plain Forward to still be used for a regular POST /integrations")
	}
	if fwd.forwardAndInspectCalled {
		t.Error("expected ForwardAndInspect NOT to be used outside of import/apply")
	}
	if s.activateCalls != 0 {
		t.Errorf("expected no auto-register activity for a non-import/apply path, got %d calls", s.activateCalls)
	}
}
