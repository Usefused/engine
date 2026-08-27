package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// importDeadlineWriter exposes the HTTP controller boundary without requiring
// sleeps or sockets for deadline and error classification assertions.
type importDeadlineWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
	err      error
}

// SetWriteDeadline captures the selected deadline while allowing an explicit
// transport failure to prove mutation forwarding remains fail-closed.
func (w *importDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return w.err
}

// importUnwrappingWriter reproduces the control-audit middleware contract so
// setting an import deadline cannot accidentally depend on a bare HTTP writer.
type importUnwrappingWriter struct{ http.ResponseWriter }

// Unwrap preserves socket control beneath response inspection middleware.
func (w importUnwrappingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// TestImportResponseWindowBoundsWorkAndDelivery proves work is bounded while
// the existing outer audit still has time to deliver its finalized response.
func TestImportResponseWindowBoundsWorkAndDelivery(t *testing.T) {
	writer := &importDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	before := time.Now()
	request, cancel, err := prepareImportApplyResponseWindow(importUnwrappingWriter{writer}, httptest.NewRequest(http.MethodPost, "/integrations/import/apply", nil))
	defer cancel()
	// A valid response controller is required before mutation may be forwarded.
	if err != nil {
		t.Fatal(err)
	}
	deadline, ok := request.Context().Deadline()
	// The bounded request context, not merely the socket, owns cancellation.
	if !ok || deadline.Before(before.Add(importApplyExecutionTimeout)) || deadline.After(time.Now().Add(importApplyExecutionTimeout)) {
		t.Fatalf("request deadline = %v, present = %v", deadline, ok)
	}
	// A short delivery grace accommodates audit finalization without increasing
	// the amount of time Registry or workspace activation may keep working.
	if !writer.deadline.Equal(deadline.Add(importApplyResponseGrace)) {
		t.Fatalf("write deadline = %v, work deadline = %v", writer.deadline, deadline)
	}
}

// TestImportResponseWindowRetainsEarlierDeadlineAndCancellation ensures a
// caller-owned bound is never detached by the longer import response budget.
func TestImportResponseWindowRetainsEarlierDeadlineAndCancellation(t *testing.T) {
	parent, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	writer := &importDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	request, cancel, err := prepareImportApplyResponseWindow(writer, httptest.NewRequest(http.MethodPost, "/integrations/import/apply", nil).WithContext(parent))
	defer cancel()
	// Preparation failure would make the deadline comparison meaningless.
	if err != nil {
		t.Fatal(err)
	}
	want, _ := parent.Deadline()
	got, _ := request.Context().Deadline()
	// Earlier caller limits remain authoritative, including delivery grace.
	if !got.Equal(want) || !writer.deadline.Equal(want.Add(importApplyResponseGrace)) {
		t.Fatalf("work=%v write=%v want=%v", got, writer.deadline, want)
	}
	stop()
	// Client cancellation must still terminate in-flight proxy and activation work.
	if !errors.Is(request.Context().Err(), context.Canceled) {
		t.Fatalf("context error = %v", request.Context().Err())
	}
}

// TestImportResponseWindowFailureDoesNotForwardOrLeak proves an inability to
// retain the response cannot start an import or expose raw transport details.
func TestImportResponseWindowFailureDoesNotForwardOrLeak(t *testing.T) {
	exporter := setupTestTracer(t)
	writer := &importDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), err: errors.New("private transport detail")}
	forwarder := &recordingForwarder{}
	accountID := uuid.New()
	request := controlTestRequest(httptest.NewRequest(http.MethodPost, "/integrations/import/apply", nil), accountID)
	RESTProxyHandler(forwarder, &autoRegisterMockStore{accountID: accountID}).ServeHTTP(writer, request)
	// No Registry or workspace work is authorized after response admission fails.
	if forwarder.forwardCalled || forwarder.forwardAndInspectCalled || writer.Code != http.StatusServiceUnavailable {
		t.Fatalf("forwarder=%#v response=%d", forwarder, writer.Code)
	}
	assertImportResponseWindowFailure(t, writer.Body.Bytes())
	encodedSpans, _ := json.Marshal(exporter.GetSpans())
	// Only stable classifications, not arbitrary transport error text, are public or traced.
	if strings.Contains(writer.Body.String()+string(encodedSpans), "private transport detail") {
		t.Fatal("raw response-controller error leaked")
	}
	// The existing mutation span must account for rejected user-triggered work.
	if spans := exporter.GetSpans(); len(spans) != 1 || spans[0].Status.Description != "import_response_deadline_unavailable" {
		t.Fatalf("spans = %#v", spans)
	}
}

// assertImportResponseWindowFailure checks that a pre-forward admission error
// does not invent the commit outcome of a different attempt on the same plan.
func assertImportResponseWindowFailure(t *testing.T, body []byte) {
	t.Helper()
	var response workspaceConfigErrorResponse
	// The failure uses the existing slim structured recovery contract.
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	// An earlier attempt may have committed; this admission failure cannot
	// convert an unknown prior result into a fabricated rollback.
	if response.Error.Code != "import_response_deadline_unavailable" || response.Error.Phase != "request_admission" || response.Error.CommitState != "unknown" {
		t.Fatalf("error = %#v", response.Error)
	}
}

// TestImportResponseWindowDoesNotChangeOrdinaryRoutes keeps the server's
// ordinary write budget unchanged for unrelated catalogue mutations.
func TestImportResponseWindowDoesNotChangeOrdinaryRoutes(t *testing.T) {
	writer := &importDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	forwarder := &recordingForwarder{}
	accountID := uuid.New()
	request := controlTestRequest(httptest.NewRequest(http.MethodPost, "/integrations/import/plan", nil), accountID)
	RESTProxyHandler(forwarder, &autoRegisterMockStore{accountID: accountID}).ServeHTTP(writer, request)
	// Scope must follow the exact apply route, not every proxied mutation.
	if !forwarder.forwardCalled || !writer.deadline.IsZero() {
		t.Fatalf("forwarded=%v changed deadline=%v", forwarder.forwardCalled, writer.deadline)
	}
}

// TestImportResponseWindowSurvivesServerWriteTimeout exercises real sockets:
// a slow Registry commit must remain deliverable past the ordinary deadline.
func TestImportResponseWindowSurvivesServerWriteTimeout(t *testing.T) {
	accountID := uuid.New()
	operationID := uuid.New()
	body := committedImportApplyBody(importApplyResponse{ServiceID: uuid.NewString(), ServiceVersionID: testImportServiceVersionID, Slug: "widgets", Version: "v1"})
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This bounded fixture delay deliberately crosses the Engine's ordinary
		// socket deadline without introducing a slow production-scale test.
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer registry.Close()
	proxy := NewRegistryProxy(registry.URL, "fixture-license")
	// The response-window test uses an admitted read-only candidate so its only
	// delayed network boundary remains the mutating Registry apply response.
	fetcher := &preflightRuntimeFetcherStub{
		runtimeContractFetcherStub: &runtimeContractFetcherStub{},
		preflight:                  importPreflightSuccess(operationID),
	}
	handler := RESTProxyHandlerWithRuntimeContracts(proxy, &autoRegisterMockStore{accountID: accountID}, fetcher)
	engine := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Test identity enters through the same Engine-local principal used by
		// handler tests; the Registry fixture never receives a user credential.
		handler.ServeHTTP(importUnwrappingWriter{w}, controlTestRequest(r, accountID))
	}))
	engine.Config.WriteTimeout = 20 * time.Millisecond
	engine.Start()
	defer engine.Close()
	requestBody := `{"plan_id":"` + operationID.String() + `","review_hash":"review-1"}`
	response, err := engine.Client().Post(engine.URL+"/integrations/import/apply", "application/json", strings.NewReader(requestBody))
	// A stale server write deadline previously closed this socket without a receipt.
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	actual, err := io.ReadAll(response.Body)
	// Publication and activation succeeded; the exact committed receipt survives.
	if err != nil || response.StatusCode != http.StatusOK || string(actual) != string(body) {
		t.Fatalf("status=%d body=%s err=%v", response.StatusCode, actual, err)
	}
}
