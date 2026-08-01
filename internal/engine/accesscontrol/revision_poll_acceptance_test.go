package accesscontrol

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type revisionPollResult struct {
	revision int64
	err      error
}

type sequencedRevisionLoader struct {
	mu      sync.Mutex
	results []revisionPollResult
	calls   int
	called  chan int
}

func (loader *sequencedRevisionLoader) LoadAuthorizationRevision(context.Context) (int64, error) {
	loader.mu.Lock()
	index := loader.calls
	loader.calls++
	result := loader.results[len(loader.results)-1]
	if index < len(loader.results) {
		result = loader.results[index]
	}
	calls := loader.calls
	loader.mu.Unlock()
	select {
	case loader.called <- calls:
	default:
	}
	return result.revision, result.err
}

func (loader *sequencedRevisionLoader) callCount() int {
	loader.mu.Lock()
	defer loader.mu.Unlock()
	return loader.calls
}

func TestPollAuthorizationRevisionsRecoversMissedStaleAllowAndDeny(t *testing.T) {
	for _, test := range []struct {
		name         string
		initialGrant bool
		updatedGrant bool
	}{
		{name: "stale allow", initialGrant: true},
		{name: "stale deny", updatedGrant: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertPollRecoversCachedDecision(t, test.initialGrant, test.updatedGrant)
		})
	}
}

func assertPollRecoversCachedDecision(t *testing.T, initialGrant, updatedGrant bool) {
	t.Helper()
	principalLoader := &principalLoaderStub{principal: pollTestPrincipal(1, initialGrant)}
	authenticator := mustAuthenticator(t, principalLoader, 1, AuthenticatorOptions{})
	assertPollDecision(t, authenticator, initialGrant)
	principalLoader.mu.Lock()
	principalLoader.principal = pollTestPrincipal(2, updatedGrant)
	principalLoader.mu.Unlock()
	pollLoader := &sequencedRevisionLoader{results: []revisionPollResult{{revision: 2}}, called: make(chan int, 4)}
	cancel, done := startRevisionPoller(authenticator, pollLoader, 10*time.Millisecond, nil)
	waitForRevisionPoll(t, authenticator, pollLoader.called, 2, 250*time.Millisecond)
	cancel()
	waitForPollerStop(t, done)
	assertPollDecision(t, authenticator, updatedGrant)
	if principalLoader.loadCount() != 2 {
		t.Fatalf("principal loads = %d, want 2 after poll flushed both caches", principalLoader.loadCount())
	}
}

func TestPollAuthorizationRevisionsRecoversAfterDatabaseErrorWithoutBusyLoop(t *testing.T) {
	authenticator := mustAuthenticator(t, &principalLoaderStub{}, 1, AuthenticatorOptions{})
	databaseErr := errors.New("database unavailable")
	loader := &sequencedRevisionLoader{
		results: []revisionPollResult{{err: databaseErr}, {revision: 2}},
		called:  make(chan int, 8),
	}
	errorsSeen := make(chan error, 2)
	interval := 20 * time.Millisecond
	started := time.Now()
	cancel, done := startRevisionPoller(authenticator, loader, interval, func(err error) { errorsSeen <- err })
	waitForRevisionPoll(t, authenticator, loader.called, 2, 300*time.Millisecond)
	cancel()
	waitForPollerStop(t, done)
	if elapsed := time.Since(started); elapsed < interval || elapsed > 300*time.Millisecond {
		t.Fatalf("poll recovery elapsed = %v, want bounded ticker-driven recovery", elapsed)
	}
	select {
	case err := <-errorsSeen:
		if !errors.Is(err, databaseErr) {
			t.Fatalf("poll error = %v, want database error", err)
		}
	default:
		t.Fatal("poller did not report the database error")
	}
	if calls := loader.callCount(); calls < 2 || calls > 4 {
		t.Fatalf("revision loads = %d, want error recovery without a busy loop", calls)
	}
}

func startRevisionPoller(authenticator *Authenticator, loader AuthorizationRevisionLoader, interval time.Duration, onError func(error)) (context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		PollAuthorizationRevisions(ctx, loader, authenticator, interval, onError)
	}()
	return cancel, done
}

func waitForRevisionPoll(t *testing.T, authenticator *Authenticator, called <-chan int, revision int64, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for authenticator.CurrentRevision() < revision {
		select {
		case <-called:
		case <-timer.C:
			t.Fatalf("revision remained %d, want %d", authenticator.CurrentRevision(), revision)
		}
	}
}

func waitForPollerStop(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("revision poller did not stop after cancellation")
	}
}

func assertPollDecision(t *testing.T, authenticator *Authenticator, wantAllowed bool) {
	t.Helper()
	actor, err := authenticator.AuthenticateControlCredential(context.Background(), "poll-key")
	if err != nil {
		t.Fatalf("AuthenticateControlCredential: %v", err)
	}
	requirement := pollTestRequirement(actor.WorkspaceID)
	allowed := (SnapshotAuthorizer{}).CheckAll(context.Background(), actor, requirement) == nil
	if allowed != wantAllowed {
		t.Fatalf("authorization allowed = %t, want %t", allowed, wantAllowed)
	}
}

func pollTestPrincipal(revision int64, grant bool) ControlPrincipal {
	principal := testPrincipal(revision)
	principal.EffectiveGrants = nil
	if grant {
		principal.EffectiveGrants = []Grant{{Permission: PermissionWorkspaceRead, Resource: pollTestRequirement(principal.WorkspaceID).Resource}}
	}
	return principal
}

func pollTestRequirement(workspaceID uuid.UUID) Requirement {
	return Requirement{Permission: PermissionWorkspaceRead, Resource: ResourceRef{Type: ResourceWorkspace, ID: workspaceID}}
}
