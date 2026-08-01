package accesscontrol

import (
	"context"
	"errors"
	"testing"
)

type revisionLoaderStub struct {
	revision int64
	err      error
}

func (s revisionLoaderStub) LoadAuthorizationRevision(context.Context) (int64, error) {
	return s.revision, s.err
}

func TestRefreshAuthorizationRevisionInvalidatesNewerState(t *testing.T) {
	authenticator := newTestAuthenticator(t, 4)
	changed, err := RefreshAuthorizationRevision(context.Background(), revisionLoaderStub{revision: 5}, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || authenticator.CurrentRevision() != 5 {
		t.Fatalf("changed = %v, revision = %d", changed, authenticator.CurrentRevision())
	}

	changed, err = RefreshAuthorizationRevision(context.Background(), revisionLoaderStub{revision: 5}, authenticator)
	if err != nil || changed {
		t.Fatalf("unchanged refresh = (%v, %v), want (false, nil)", changed, err)
	}
}

func TestRefreshAuthorizationRevisionPropagatesLoadFailure(t *testing.T) {
	authenticator := newTestAuthenticator(t, 4)
	want := errors.New("database unavailable")
	if _, err := RefreshAuthorizationRevision(context.Background(), revisionLoaderStub{err: want}, authenticator); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func newTestAuthenticator(t *testing.T, revision int64) *Authenticator {
	t.Helper()
	authenticator, err := NewAuthenticator(&principalLoaderStub{}, revision, AuthenticatorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return authenticator
}
