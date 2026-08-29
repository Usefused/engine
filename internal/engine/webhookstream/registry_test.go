package webhookstream

import (
	"sync"
	"testing"

	"github.com/Usefused/engine/internal/engine/apptokeninvalidation"
	"github.com/google/uuid"
)

// Compile-time conformance keeps token revocation fan-out usable without a
// production dependency from this focused registry to the publisher package.
var _ apptokeninvalidation.Invalidator = (*Registry)(nil)

// TestTokenInvalidationClosesAllTokenStreams proves a family token revocation
// closes every exact-version receiver while leaving authorization to decide
// whether a later pending registration may confirm.
func TestTokenInvalidationClosesAllTokenStreams(t *testing.T) {
	registry := NewRegistry()
	tokenID := uuid.New()
	first := mustRegisterAndConfirm(t, registry, tokenID, uuid.New())
	second := mustRegisterAndConfirm(t, registry, tokenID, uuid.New())

	// One token revocation must reach every sibling exact app registration.
	if invalidated := registry.InvalidateToken(tokenID); invalidated != 2 {
		t.Fatalf("invalidated registrations = %d, want 2", invalidated)
	}
	assertRegistrationClosed(t, first, CancellationReasonTokenInvalidated)
	assertRegistrationClosed(t, second, CancellationReasonTokenInvalidated)

	pending, accepted := registry.Register(tokenID, uuid.New())
	// A later attempt may register only as pending; the caller must revalidate
	// the revoked token against the authoritative validator before Confirm.
	if !accepted {
		t.Fatal("post-invalidation pending registration was rejected before revalidation")
	}
	pending.Unregister()
	// Repeated invalidation has no active registrations left to count.
	if invalidated := registry.InvalidateToken(tokenID); invalidated != 0 {
		t.Fatalf("repeat invalidation count = %d, want 0", invalidated)
	}
}

// TestExactAppInvalidationIsolatesSiblingsAndAllowsReauthorization proves an
// exact runtime invalidation is temporary and never broadens to a sibling app.
func TestExactAppInvalidationIsolatesSiblingsAndAllowsReauthorization(t *testing.T) {
	registry := NewRegistry()
	tokenID := uuid.New()
	appID := uuid.New()
	siblingAppID := uuid.New()
	first := mustRegisterAndConfirm(t, registry, tokenID, appID)
	second := mustRegisterAndConfirm(t, registry, uuid.New(), appID)
	sibling := mustRegisterAndConfirm(t, registry, tokenID, siblingAppID)

	// Exact app invalidation closes every receiver for that version regardless of token.
	if invalidated := registry.InvalidateAppRuntime(appID); invalidated != 2 {
		t.Fatalf("invalidated registrations = %d, want 2", invalidated)
	}
	assertRegistrationClosed(t, first, CancellationReasonAppInvalidated)
	assertRegistrationClosed(t, second, CancellationReasonAppInvalidated)
	assertRegistrationOpen(t, sibling)

	// App invalidation is a generation fence rather than a tombstone, so a fresh
	// authorization can register the unchanged active exact app again.
	replacement := mustRegisterAndConfirm(t, registry, tokenID, appID)
	assertRegistrationOpen(t, replacement)
	replacement.Unregister()
	sibling.Unregister()
}

// TestConfirmRejectsInvalidationDuringReauthorization proves the generation
// fence closes the gap between initial identity resolution and stream startup.
func TestConfirmRejectsInvalidationDuringReauthorization(t *testing.T) {
	registry := NewRegistry()
	appID := uuid.New()
	registration, accepted := registry.Register(uuid.New(), appID)
	// A valid pre-authorization identity should acquire only a pending registration.
	if !accepted {
		t.Fatal("valid pending registration was rejected")
	}

	registry.InvalidateAppRuntime(appID)
	// Revalidation from the old generation cannot activate after invalidation.
	if registration.Confirm() {
		t.Fatal("stale registration confirmed after app invalidation")
	}
	assertRegistrationClosed(t, registration, CancellationReasonAppInvalidated)

	// The next registration captures the advanced generation and can confirm.
	replacement := mustRegisterAndConfirm(t, registry, uuid.New(), appID)
	replacement.Unregister()
}

// TestConcurrentConfirmAndInvalidationAlwaysEndsTheOldStream exercises both
// lock orderings and proves a completed invalidation leaves no stale receiver.
func TestConcurrentConfirmAndInvalidationAlwaysEndsTheOldStream(t *testing.T) {
	const attempts = 200
	registry := NewRegistry()
	appID := uuid.New()
	// Repetition increases the chance that both lock acquisition orders execute
	// while keeping each assertion deterministic after the workers join.
	for attempt := 0; attempt < attempts; attempt++ {
		registration, accepted := registry.Register(uuid.New(), appID)
		// Each new token is valid, so only the app-generation race may reject it.
		if !accepted {
			t.Fatalf("attempt %d registration was unexpectedly rejected", attempt)
		}

		start := make(chan struct{})
		var workers sync.WaitGroup
		workers.Add(2)
		go func() {
			defer workers.Done()
			<-start
			registration.Confirm()
		}()
		go func() {
			defer workers.Done()
			<-start
			registry.InvalidateAppRuntime(appID)
		}()
		close(start)
		workers.Wait()

		// Whether Confirm won or lost, the later completed invalidation must close
		// the registration captured from the preceding generation.
		assertRegistrationClosed(t, registration, CancellationReasonAppInvalidated)
		// Detached registrations can never appear current after the race settles.
		if registration.IsCurrent() {
			t.Fatalf("attempt %d retained a current stale registration", attempt)
		}
	}
}

// TestUnregisterCleansIndexesIdempotently proves normal stream completion does
// not retain token/app memberships or inflate later invalidation counts.
func TestUnregisterCleansIndexesIdempotently(t *testing.T) {
	registry := NewRegistry()
	tokenID, appID := uuid.New(), uuid.New()
	registration := mustRegisterAndConfirm(t, registry, tokenID, appID)

	registration.Unregister()
	registration.Unregister()
	assertRegistrationClosed(t, registration, CancellationReasonUnregistered)
	// Cleanup must make the old handle unusable even without advancing a generation.
	if registration.IsCurrent() {
		t.Fatal("unregistered stream remained current")
	}
	// Later identity invalidation has no detached stream left to close or count.
	if invalidated := registry.InvalidateAppRuntime(appID); invalidated != 0 {
		t.Fatalf("app invalidation count after cleanup = %d, want 0", invalidated)
	}
	if invalidated := registry.InvalidateToken(tokenID); invalidated != 0 {
		t.Fatalf("token invalidation count after cleanup = %d, want 0", invalidated)
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	// Once no preceding registration can confirm, both generation state and live
	// indexes must be released so process-lifetime churn remains bounded.
	if len(registry.tokens) != 0 || len(registry.apps) != 0 || len(registry.registrations) != 0 ||
		len(registry.registrationsByToken) != 0 || len(registry.registrationsByApp) != 0 {
		t.Fatalf("registry state was not cleaned: token_generations=%d app_generations=%d registrations=%d token_indexes=%d app_indexes=%d",
			len(registry.tokens), len(registry.apps), len(registry.registrations), len(registry.registrationsByToken), len(registry.registrationsByApp))
	}
}

// mustRegisterAndConfirm creates one active registration or fails the test at
// the call site so individual lifecycle assertions remain focused.
func mustRegisterAndConfirm(t *testing.T, registry *Registry, tokenID, appID uuid.UUID) *Registration {
	t.Helper()
	registration, accepted := registry.Register(tokenID, appID)
	// Valid exact token and app identities must produce a pending registration.
	if !accepted {
		t.Fatal("valid registration was rejected")
	}
	// Revalidation is represented by Confirm in this focused registry test.
	if !registration.Confirm() {
		t.Fatal("current registration did not confirm")
	}
	return registration
}

// assertRegistrationClosed verifies the cancellation signal and bounded reason
// without waiting on wall-clock time.
func assertRegistrationClosed(t *testing.T, registration *Registration, expected CancellationReason) {
	t.Helper()
	// A non-blocking receive distinguishes synchronous cancellation from a
	// stream that would terminate only after an unrelated client timeout.
	select {
	case <-registration.Done():
		// A closed signal is the required synchronous cancellation boundary.
	default:
		t.Fatal("registration cancellation signal remained open")
	}
	// The reason must be stored before Done closes so stream owners see a stable cause.
	if reason := registration.Reason(); reason != expected {
		t.Fatalf("cancellation reason = %d, want %d", reason, expected)
	}
}

// assertRegistrationOpen verifies sibling isolation without introducing a
// timeout or scheduler-dependent assertion.
func assertRegistrationOpen(t *testing.T, registration *Registration) {
	t.Helper()
	// A non-blocking receive verifies isolation without adding timing flakiness.
	select {
	case <-registration.Done():
		t.Fatalf("registration closed unexpectedly with reason %d", registration.Reason())
	default:
		// An open signal proves the exact sibling remained attached.
	}
	// A confirmed sibling must remain current while its signal is open.
	if !registration.IsCurrent() {
		t.Fatal("open registration was not current")
	}
}
