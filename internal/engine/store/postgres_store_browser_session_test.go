package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func TestPostgresBrowserSessionDerivesAndRevokesCredentialAtomically(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	_, owner, _ := bootstrapUserTest(t, ctx, repository, "browser-session")
	actor := accesscontrol.Actor{SubjectID: owner.SubjectID, CredentialID: owner.CredentialID, Kind: accesscontrol.SubjectBootstrap}

	credential, err := repository.IssueBrowserSession(ctx, actor, "license_key", time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("IssueBrowserSession: %v", err)
	}
	principal, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(credential.RawKey))
	if err != nil || principal.SubjectID != owner.SubjectID || principal.CredentialID != credential.CredentialID {
		t.Fatalf("browser session principal = %#v, %v", principal, err)
	}
	var keyHash, source, authMethod string
	var metadata []byte
	if err := pool.QueryRow(ctx, `SELECT key_hash, source, auth_method FROM fused_control_credentials WHERE id = $1`, credential.CredentialID).Scan(&keyHash, &source, &authMethod); err != nil {
		t.Fatalf("load browser credential hash: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT metadata FROM fused_audit_events WHERE action = 'user.browser_session.create' AND metadata->>'session_credential_id' = $1`, credential.CredentialID.String()).Scan(&metadata); err != nil {
		t.Fatalf("load browser session audit: %v", err)
	}
	if keyHash == credential.RawKey || source != "license_exchange" || authMethod != "license_key" || strings.Contains(string(metadata), credential.RawKey) {
		t.Fatal("browser session credential leaked into persistence or audit metadata")
	}
	if principal.CredentialSource != "license_exchange" || principal.AuthenticationMethod != "license_key" {
		t.Fatalf("browser principal provenance = %q/%q", principal.CredentialSource, principal.AuthenticationMethod)
	}
	second, err := repository.IssueBrowserSession(ctx, actor, "license_key", time.Now().UTC().Add(time.Hour))
	if err != nil || second.CredentialID == credential.CredentialID {
		t.Fatalf("concurrent browser session = %#v, %v", second, err)
	}

	logoutActor := accesscontrol.Actor{SubjectID: owner.SubjectID, CredentialID: credential.CredentialID, Kind: accesscontrol.SubjectBootstrap}
	logout, err := repository.RevokeBrowserSession(ctx, logoutActor, time.Now().UTC())
	if err != nil || logout.AuthorizationRevision <= credential.AuthorizationRevision {
		t.Fatalf("RevokeBrowserSession = %#v, %v", logout, err)
	}
	if _, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(credential.RawKey)); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("revoked browser credential auth error = %v", err)
	}
}
