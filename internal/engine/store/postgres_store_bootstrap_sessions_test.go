package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
)

// TestPostgresBootstrapPreservesSessionsUntilLicenseRotation covers hosted restarts without weakening credential rotation.
func TestPostgresBootstrapPreservesSessionsUntilLicenseRotation(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	accountID := uuid.New()
	_, err := repository.BootstrapWorkspace(ctx, accountID, "Upgrade session test")
	// A real isolated workspace exercises the same SQL transaction as Engine startup.
	if err != nil {
		t.Fatal(err)
	}
	owner, err := accesscontrol.BootstrapOwner(ctx, repository, accountID, "unchanged-upgrade-license")
	// Session issuance must derive from a successfully bootstrapped root.
	if err != nil {
		t.Fatal(err)
	}
	actor := accesscontrol.Actor{SubjectID: owner.SubjectID, CredentialID: owner.CredentialID, Kind: accesscontrol.SubjectBootstrap}
	sessions := issueUpgradeBrowserSessions(t, ctx, repository, actor)
	restarted, err := accesscontrol.BootstrapOwner(ctx, repository, accountID, "unchanged-upgrade-license")
	// An unchanged root must preserve both identity and the revision established by the latest session.
	if err != nil || restarted.Changed || restarted.CredentialID != owner.CredentialID || restarted.Revision != sessions[len(sessions)-1].AuthorizationRevision {
		t.Fatalf("unchanged restart mutated bootstrap: result=%+v err=%v", restarted, err)
	}
	assertUpgradeSessionsUsable(t, ctx, repository, sessions)
	assertUpgradeLicenseRotationRevokes(t, ctx, repository, accountID, owner, sessions)
}

// issueUpgradeBrowserSessions covers the legacy and current browser exchange provenance.
func issueUpgradeBrowserSessions(t *testing.T, ctx context.Context, repository *postgresStore, actor accesscontrol.Actor) []BrowserSessionCredential {
	t.Helper()
	var sessions []BrowserSessionCredential
	// Both legacy and current browser exchanges must survive a software restart.
	for _, method := range []string{"license_key", "api_key"} {
		session, err := repository.IssueBrowserSession(ctx, actor, method, time.Now().Add(time.Hour))
		// Failed fixture issuance must not silently reduce coverage.
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, session)
	}
	return sessions
}

// assertUpgradeSessionsUsable verifies persisted credentials instead of relying on a process-local authentication cache.
func assertUpgradeSessionsUsable(t *testing.T, ctx context.Context, repository *postgresStore, sessions []BrowserSessionCredential) {
	t.Helper()
	// Loading through the real principal query proves each persisted browser credential remains usable.
	for _, session := range sessions {
		principal, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(session.RawKey))
		// Equal credential IDs exclude replacement with a newly issued session.
		if err != nil || principal.CredentialID != session.CredentialID {
			t.Fatalf("session did not survive restart: %v", err)
		}
	}
}

// assertUpgradeLicenseRotationRevokes ensures restart preservation does not weaken deliberate license rotation.
func assertUpgradeLicenseRotationRevokes(t *testing.T, ctx context.Context, repository *postgresStore, accountID uuid.UUID, owner accesscontrol.BootstrapResult, sessions []BrowserSessionCredential) {
	t.Helper()
	rotated, err := accesscontrol.BootstrapOwner(ctx, repository, accountID, "rotated-upgrade-license")
	// A genuinely different license must retain the existing revocation behavior.
	if err != nil || !rotated.Changed || rotated.CredentialID == owner.CredentialID {
		t.Fatalf("license rotation failed: result=%+v err=%v", rotated, err)
	}
	// Rotation must invalidate both the prior root and all sessions derived from its bootstrap subject.
	for _, credential := range []string{"unchanged-upgrade-license", sessions[0].RawKey, sessions[1].RawKey} {
		_, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(credential))
		// Do not accept storage failures as evidence that revocation was enforced.
		if !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
			t.Fatalf("rotated credential was not denied: %v", err)
		}
	}
}
