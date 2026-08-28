package sandbox

import (
	"context"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

type mcpRouteResolverStub struct {
	target *store.MCPRouteTarget
	err    error
	calls  int
}

type directMCPRouteResolverStub struct{}

// ResolveMCPRoute treats the requested UUID as an MCP version for tests that do not exercise persistence.
func (directMCPRouteResolverStub) ResolveMCPRoute(_ context.Context, routeID uuid.UUID) (*store.MCPRouteTarget, error) {
	return &store.MCPRouteTarget{AppFamilyID: routeID, AppID: routeID, Stable: false}, nil
}

// installDirectMCPRouteResolver gives transport-only tests the explicit resolver production requires.
func installDirectMCPRouteResolver(t *testing.T) {
	t.Helper()
	previous := globalMCPRouteResolver
	globalMCPRouteResolver = directMCPRouteResolverStub{}
	t.Cleanup(func() { globalMCPRouteResolver = previous })
}

// ResolveMCPRoute records admission lookups without requiring PostgreSQL in transport unit tests.
func (resolver *mcpRouteResolverStub) ResolveMCPRoute(context.Context, uuid.UUID) (*store.MCPRouteTarget, error) {
	resolver.calls++
	return resolver.target, resolver.err
}

// TestResolveMCPRouteUsesPersistedFamilyTarget verifies stable admission returns
// the exact immutable version selected by the persistence boundary.
func TestResolveMCPRouteUsesPersistedFamilyTarget(t *testing.T) {
	previous := globalMCPRouteResolver
	t.Cleanup(func() { globalMCPRouteResolver = previous })
	familyID, appID := uuid.New(), uuid.New()
	resolver := &mcpRouteResolverStub{target: &store.MCPRouteTarget{AppFamilyID: familyID, AppID: appID, Stable: true}}
	globalMCPRouteResolver = resolver

	target, err := resolveMCPRoute(context.Background(), familyID.String())
	// One database resolution must determine the immutable session target.
	if err != nil || resolver.calls != 1 || target.AppID != appID || !target.Stable {
		t.Fatalf("resolved target = %#v, calls %d, error %v", target, resolver.calls, err)
	}
}

// TestResolveMCPRouteFailsClosedWithoutPersistence verifies missing startup
// wiring cannot turn an arbitrary UUID into an executable MCP version route.
func TestResolveMCPRouteFailsClosedWithoutPersistence(t *testing.T) {
	previous := globalMCPRouteResolver
	globalMCPRouteResolver = nil
	// Restoring process-global test state prevents this fail-closed case from
	// changing the resolver used by transport tests in the same package.
	t.Cleanup(func() { globalMCPRouteResolver = previous })

	if _, err := resolveMCPRoute(context.Background(), uuid.NewString()); err == nil {
		t.Fatal("resolve MCP route without persistence succeeded, want denial")
	}
}

// TestResolveMCPRouteRejectsMalformedIdentityBeforePersistence verifies invalid
// public paths never issue a database lookup.
func TestResolveMCPRouteRejectsMalformedIdentityBeforePersistence(t *testing.T) {
	previous := globalMCPRouteResolver
	resolver := &mcpRouteResolverStub{}
	globalMCPRouteResolver = resolver
	// Restoring process-global test state keeps malformed-path isolation local.
	t.Cleanup(func() { globalMCPRouteResolver = previous })

	if _, err := resolveMCPRoute(context.Background(), "not-a-uuid"); err == nil {
		t.Fatal("resolve malformed MCP route succeeded, want rejection")
	}
	// Parsing owns malformed-path rejection, so persistence must remain untouched.
	if resolver.calls != 0 {
		t.Fatalf("resolver calls = %d, want 0", resolver.calls)
	}
}

// TestMCPStableRouteSessionSurvivesPromotion verifies a connected session keeps
// its original version even after the family pointer advances for new sessions.
func TestMCPStableRouteSessionSurvivesPromotion(t *testing.T) {
	previousResolver, previousValidator := globalMCPRouteResolver, globalTokenValidator
	t.Cleanup(func() {
		globalMCPRouteResolver, globalTokenValidator = previousResolver, previousValidator
	})
	familyID, oldAppID, newAppID, tokenID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	token := "family-token"
	resolver := &mcpRouteResolverStub{target: &store.MCPRouteTarget{AppFamilyID: familyID, AppID: newAppID, Stable: true}}
	globalMCPRouteResolver = resolver
	globalTokenValidator = &streamableTokenValidator{token: token, tokenID: tokenID}
	sess := &mcpSession{
		appID: oldAppID.String(), routeID: familyID.String(), sessionID: uuid.NewString(),
		tokenID: tokenID, protocolVersion: "2025-06-18", transport: mcpStreamableTransport,
		token: token, idleTimer: time.AfterFunc(time.Hour, func() {}),
	}
	mcpSessions.Lock()
	mcpSessions.m[sess.sessionID] = sess
	mcpSessions.Unlock()
	t.Cleanup(func() { terminateMCPSession(sess.sessionID, "test_cleanup") })

	got, status, err := authenticateMCPStreamableSession(context.Background(), familyID.String(), token, sess.sessionID, sess.protocolVersion)
	// Established sessions authenticate their captured app ID and never consult
	// the now-promoted family target on subsequent transport requests.
	if err != nil || status != 200 || got.appID != oldAppID.String() || resolver.calls != 0 {
		t.Fatalf("session auth = %#v, status %d, resolver calls %d, error %v", got, status, resolver.calls, err)
	}
}
