package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type revisionSyncLoaderStub struct {
	revision int64
	err      error
}

func (s revisionSyncLoaderStub) LoadAuthorizationRevision(context.Context) (int64, error) {
	return s.revision, s.err
}

type revisionSyncSinkStub struct {
	revision int64
}

func (s *revisionSyncSinkStub) SetRevision(revision int64) bool {
	changed := s.revision != revision
	s.revision = revision
	return changed
}

type revisionObservingWriter struct {
	*httptest.ResponseRecorder
	sink             *revisionSyncSinkStub
	revisionAtHeader int64
}

func (w *revisionObservingWriter) WriteHeader(status int) {
	w.revisionAtHeader = w.sink.revision
	w.ResponseRecorder.WriteHeader(status)
}

func TestAuthorizationRevisionSyncHandlerInvalidatesBeforeResponse(t *testing.T) {
	sink := &revisionSyncSinkStub{revision: 4}
	w := &revisionObservingWriter{ResponseRecorder: httptest.NewRecorder(), sink: sink}
	handler := authorizationRevisionSyncHandler(revisionSyncLoaderStub{revision: 5}, sink, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"status":"applied"}`))
	})

	handler(w, httptest.NewRequest(http.MethodPost, "/sdk-config/apply", nil))

	if w.Code != http.StatusCreated || w.Body.String() != `{"status":"applied"}` {
		t.Fatalf("response = %d %q", w.Code, w.Body.String())
	}
	if w.revisionAtHeader != 5 {
		t.Fatalf("revision at response = %d, want 5", w.revisionAtHeader)
	}
}

func TestAuthorizationRevisionSyncHandlerPreservesCommittedResponseOnLoadFailure(t *testing.T) {
	sink := &revisionSyncSinkStub{revision: 4}
	w := httptest.NewRecorder()
	handler := authorizationRevisionSyncHandler(revisionSyncLoaderStub{err: errors.New("database unavailable")}, sink, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	handler(w, httptest.NewRequest(http.MethodPost, "/mcp-config/apply", nil))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
	}
	if sink.revision != 4 {
		t.Fatalf("revision = %d, want unchanged revision 4", sink.revision)
	}
}
