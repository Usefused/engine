package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// preflightRuntimeFetcherStub joins the read-only preflight and post-commit
// fetch seams without changing production's shared Registry client.
type preflightRuntimeFetcherStub struct {
	*runtimeContractFetcherStub
	preflight      *sandbox.ImportContractPreflight
	preflightErr   error
	preflightBody  []byte
	preflightCalls int
}

// PreflightImport records the exact receipt and returns the configured admission outcome.
func (f *preflightRuntimeFetcherStub) PreflightImport(_ context.Context, body []byte) (*sandbox.ImportContractPreflight, error) {
	f.preflightCalls++
	f.preflightBody = append([]byte(nil), body...)
	// Deterministic failure fixtures must never fall through to a synthetic success.
	if f.preflightErr != nil {
		return nil, f.preflightErr
	}
	return f.preflight, nil
}

// importPreflightSuccess creates the minimal proof needed by proxy tests; full
// runtime semantics are exercised in sandbox and store tests.
func importPreflightSuccess(operationID uuid.UUID) *sandbox.ImportContractPreflight {
	return &sandbox.ImportContractPreflight{
		OperationID:  operationID,
		ContractHash: "sha256:" + strings.Repeat("a", 64),
		Snapshot:     store.ServiceContractSnapshot{ServiceID: uuid.New(), ServiceVersionID: uuid.New(), Version: "v1"},
	}
}

// bodyRecordingForwarder captures the final Registry apply body while retaining
// the existing response-inspection behavior used by activation tests.
type bodyRecordingForwarder struct {
	*recordingForwarder
	requestBody []byte
}

// ForwardAndInspect records the proof-bound request before simulating Registry apply.
func (f *bodyRecordingForwarder) ForwardAndInspect(w http.ResponseWriter, r *http.Request, stripPrefix string, onSuccess func(*http.Response, []byte)) {
	body, _ := io.ReadAll(r.Body)
	f.requestBody = append([]byte(nil), body...)
	r.Body = io.NopCloser(bytes.NewReader(body))
	f.recordingForwarder.ForwardAndInspect(w, r, stripPrefix, onSuccess)
}

// TestImportApplyPreflightBindsProofBeforePublication proves the existing apply
// endpoint stays composite while Registry receives only Engine's admitted hash.
func TestImportApplyPreflightBindsProofBeforePublication(t *testing.T) {
	accountID, serviceID := uuid.New(), uuid.New()
	operationID := uuid.New()
	forwarder := &bodyRecordingForwarder{recordingForwarder: &recordingForwarder{body: string(committedImportApplyBody(importApplyResponse{
		ServiceID: serviceID.String(), ServiceVersionID: testImportServiceVersionID, Slug: "stripe", Version: "v1",
	}))}}
	workspace := &autoRegisterMockStore{accountID: accountID}
	fetcher := &preflightRuntimeFetcherStub{
		runtimeContractFetcherStub: &runtimeContractFetcherStub{},
		preflight:                  importPreflightSuccess(operationID),
	}
	handler := RESTProxyHandlerWithRuntimeContracts(forwarder, workspace, fetcher)
	request := httptest.NewRequest(http.MethodPost, "/integrations/import/apply", bytes.NewBufferString(`{"plan_id":"`+operationID.String()+`","review_hash":"review-1","preflight_hash":"caller-value"}`))
	request = controlTestRequest(request, accountID)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if fetcher.preflightCalls != 1 || !forwarder.forwardAndInspectCalled {
		t.Fatalf("preflight=%d forwarded=%v", fetcher.preflightCalls, forwarder.forwardAndInspectCalled)
	}
	var preflightBody, applyBody map[string]any
	_ = json.Unmarshal(fetcher.preflightBody, &preflightBody)
	_ = json.Unmarshal(forwarder.requestBody, &applyBody)
	// Caller proof is removed from preview and overwritten before the mutating request.
	if _, exists := preflightBody["preflight_hash"]; exists || applyBody["preflight_hash"] != fetcher.preflight.ContractHash {
		t.Fatalf("preflight body=%v apply body=%v", preflightBody, applyBody)
	}
}

// TestImportApplyPreflightRejectionNeverForwards proves deterministic Engine
// incompatibility remains a known not-committed outcome.
func TestImportApplyPreflightRejectionNeverForwards(t *testing.T) {
	accountID, operationID := uuid.New(), uuid.New()
	forwarder := &recordingForwarder{}
	fetcher := &preflightRuntimeFetcherStub{
		runtimeContractFetcherStub: &runtimeContractFetcherStub{},
		preflightErr:               fmtImportRuntimeContractRejection(),
	}
	handler := RESTProxyHandlerWithRuntimeContracts(forwarder, &autoRegisterMockStore{accountID: accountID}, fetcher)
	request := httptest.NewRequest(http.MethodPost, "/integrations/import/apply", bytes.NewBufferString(`{"plan_id":"`+operationID.String()+`","review_hash":"review-1"}`))
	request = controlTestRequest(request, accountID)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnprocessableEntity || forwarder.forwardAndInspectCalled || forwarder.forwardCalled {
		t.Fatalf("status=%d forwarded=%v/%v body=%s", recorder.Code, forwarder.forwardCalled, forwarder.forwardAndInspectCalled, recorder.Body.String())
	}
	var response workspaceConfigErrorResponse
	_ = json.Unmarshal(recorder.Body.Bytes(), &response)
	if response.Error.Code != "import_runtime_contract_rejected" || response.Error.Phase != "engine_preflight" || response.Error.CommitState != "not_committed" {
		t.Fatalf("preflight rejection = %#v", response.Error)
	}
}

// fmtImportRuntimeContractRejection wraps the sentinel as the real sandbox client does.
func fmtImportRuntimeContractRejection() error {
	return errors.Join(sandbox.ErrImportRuntimeContractRejected, errors.New("fixture transport defect"))
}

// TestImportApplyPreflightPreservesRegistryError verifies plan/review failures
// stay Registry-owned and never reach publication or activation.
func TestImportApplyPreflightPreservesRegistryError(t *testing.T) {
	accountID, operationID := uuid.New(), uuid.New()
	body := []byte(`{"error":{"code":"IMPORT_REVIEW_MISMATCH","message":"review changed","phase":"engine_preflight","commit_state":"not_committed"}}`)
	forwarder := &recordingForwarder{}
	fetcher := &preflightRuntimeFetcherStub{
		runtimeContractFetcherStub: &runtimeContractFetcherStub{},
		preflightErr:               &sandbox.ImportPreflightHTTPError{StatusCode: http.StatusConflict, Body: body},
	}
	handler := RESTProxyHandlerWithRuntimeContracts(forwarder, &autoRegisterMockStore{accountID: accountID}, fetcher)
	request := httptest.NewRequest(http.MethodPost, "/integrations/import/apply", bytes.NewBufferString(`{"plan_id":"`+operationID.String()+`","review_hash":"review-1"}`))
	request = controlTestRequest(request, accountID)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict || !bytes.Equal(bytes.TrimSpace(recorder.Body.Bytes()), body) || forwarder.forwardAndInspectCalled {
		t.Fatalf("status=%d body=%s forwarded=%v", recorder.Code, recorder.Body.String(), forwarder.forwardAndInspectCalled)
	}
}

// TestImportApplyPreflightRejectsUnstructuredRegistryError keeps proxy and
// transport prose out of the public Engine response while retaining safe retry.
func TestImportApplyPreflightRejectsUnstructuredRegistryError(t *testing.T) {
	accountID, operationID := uuid.New(), uuid.New()
	forwarder := &recordingForwarder{}
	fetcher := &preflightRuntimeFetcherStub{
		runtimeContractFetcherStub: &runtimeContractFetcherStub{},
		preflightErr:               &sandbox.ImportPreflightHTTPError{StatusCode: http.StatusBadGateway, Body: []byte("private proxy detail")},
	}
	handler := RESTProxyHandlerWithRuntimeContracts(forwarder, &autoRegisterMockStore{accountID: accountID}, fetcher)
	request := controlTestRequest(httptest.NewRequest(http.MethodPost, "/integrations/import/apply", strings.NewReader(`{"plan_id":"`+operationID.String()+`","review_hash":"review-1"}`)), accountID)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	// Unstructured upstream bodies cannot be distinguished from infrastructure failures.
	if recorder.Code != http.StatusServiceUnavailable || strings.Contains(recorder.Body.String(), "private proxy detail") || forwarder.forwardAndInspectCalled {
		t.Fatalf("status=%d body=%s forwarded=%v", recorder.Code, recorder.Body.String(), forwarder.forwardAndInspectCalled)
	}
}

// TestImportApplyPreflightAllowsTerminalReplay leaves durable plan replay with
// Registry while ensuring no caller-supplied proof is needed for a no-op apply.
func TestImportApplyPreflightAllowsTerminalReplay(t *testing.T) {
	accountID, operationID := uuid.New(), uuid.New()
	terminal := []byte(`{"error":{"code":"IMPORT_OPERATION_NOT_PENDING","phase":"engine_preflight","commit_state":"not_committed"}}`)
	forwarder := &bodyRecordingForwarder{recordingForwarder: &recordingForwarder{body: string(committedImportApplyBody(importApplyResponse{
		ServiceID: uuid.NewString(), ServiceVersionID: testImportServiceVersionID, Slug: "stripe", Version: "v1",
	}))}}
	fetcher := &preflightRuntimeFetcherStub{
		runtimeContractFetcherStub: &runtimeContractFetcherStub{},
		preflightErr:               &sandbox.ImportPreflightHTTPError{StatusCode: http.StatusConflict, Body: terminal},
	}
	handler := RESTProxyHandlerWithRuntimeContracts(forwarder, &autoRegisterMockStore{accountID: accountID}, fetcher)
	original := `{"plan_id":"` + operationID.String() + `","review_hash":"review-1"}`
	request := controlTestRequest(httptest.NewRequest(http.MethodPost, "/integrations/import/apply", strings.NewReader(original)), accountID)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	// Only Registry's exact terminal classification may resume forwarding, and
	// the original immutable receipt must remain byte-for-byte unchanged.
	if !forwarder.forwardAndInspectCalled || string(forwarder.requestBody) != original {
		t.Fatalf("forwarded=%v body=%q", forwarder.forwardAndInspectCalled, forwarder.requestBody)
	}
}

// TestImportApplyPreflightRejectsMalformedReceipt proves invalid local input
// cannot invoke either the read-only Registry preview or mutating apply path.
func TestImportApplyPreflightRejectsMalformedReceipt(t *testing.T) {
	accountID := uuid.New()
	forwarder := &recordingForwarder{}
	fetcher := &preflightRuntimeFetcherStub{runtimeContractFetcherStub: &runtimeContractFetcherStub{}}
	handler := RESTProxyHandlerWithRuntimeContracts(forwarder, &autoRegisterMockStore{accountID: accountID}, fetcher)
	request := controlTestRequest(httptest.NewRequest(http.MethodPost, "/integrations/import/apply", strings.NewReader(`{"plan_id":[]} trailing`)), accountID)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	// Malformed input has no durable operation identity and is a known pre-commit rejection.
	if recorder.Code != http.StatusBadRequest || fetcher.preflightCalls != 0 || forwarder.forwardCalled || forwarder.forwardAndInspectCalled {
		t.Fatalf("status=%d preflight=%d forwarded=%v/%v body=%s", recorder.Code, fetcher.preflightCalls, forwarder.forwardCalled, forwarder.forwardAndInspectCalled, recorder.Body.String())
	}
}
