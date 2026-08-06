package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
)

func TestPostgresCLILoginCreatesSubjectCredentialOnce(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	_, owner, _ := bootstrapUserTest(t, ctx, repository, "cli-login")
	bootstrap := accesscontrol.Actor{SubjectID: owner.SubjectID, CredentialID: owner.CredentialID, Kind: accesscontrol.SubjectBootstrap}
	browser, err := repository.IssueBrowserSession(ctx, bootstrap, "api_key", time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("IssueBrowserSession: %v", err)
	}
	actor := accesscontrol.Actor{
		SubjectID: owner.SubjectID, CredentialID: browser.CredentialID,
		CredentialSource: "api_key_exchange", AuthenticationMethod: "api_key",
	}
	rawKey := "fsk_cli-login-raw-secret"
	now := time.Now().UTC()
	transaction := CLILoginTransaction{
		ID: uuid.New(), PollSecretHash: "poll-hash", BrowserSecretHash: "browser-hash",
		CredentialHash: accesscontrol.HashControlCredential(rawKey), CredentialPrefix: accesscontrol.CredentialPrefix(rawKey),
		ExpiresAt: now.Add(time.Minute), CredentialExpiresAt: now.Add(24 * time.Hour),
	}
	if err := repository.CreateCLILoginTransaction(ctx, transaction); err != nil {
		t.Fatalf("CreateCLILoginTransaction: %v", err)
	}
	if err := repository.ApproveCLILoginTransaction(ctx, transaction.ID, transaction.BrowserSecretHash, actor, now); err != nil {
		t.Fatalf("ApproveCLILoginTransaction: %v", err)
	}
	issued, err := repository.ConsumeCLILoginTransaction(ctx, transaction.ID, transaction.PollSecretHash, now)
	if err != nil {
		t.Fatalf("ConsumeCLILoginTransaction: %v", err)
	}
	retried, err := repository.ConsumeCLILoginTransaction(ctx, transaction.ID, transaction.PollSecretHash, now.Add(time.Minute))
	if err != nil || retried.CredentialID != issued.CredentialID {
		t.Fatalf("retry = %#v, %v", retried, err)
	}
	principal, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(rawKey))
	if err != nil || principal.SubjectID != owner.SubjectID || principal.CredentialSource != "managed_cli_login" {
		t.Fatalf("CLI principal = %#v, %v", principal, err)
	}
	var metadata string
	if err := pool.QueryRow(ctx, `SELECT metadata::text FROM fused_audit_events WHERE action = 'user.cli_credential.create' AND actor_credential_id = $1`, browser.CredentialID).Scan(&metadata); err != nil {
		t.Fatalf("load CLI audit: %v", err)
	}
	for _, secret := range []string{rawKey, transaction.CredentialHash, transaction.PollSecretHash, transaction.BrowserSecretHash} {
		if strings.Contains(metadata, secret) {
			t.Fatalf("CLI audit leaked secret material: %s", metadata)
		}
	}

	_, err = repository.RevokeCurrentCLICredential(ctx, MutationActor{
		SubjectID: owner.SubjectID, CredentialID: browser.CredentialID,
		RequestID: "cli-logout-wrong-source", TraceID: "trace-cli-logout-wrong-source",
	})
	if !errors.Is(err, ErrCLILogoutDenied) {
		t.Fatalf("browser credential self-revoke error = %v", err)
	}
	if _, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(browser.RawKey)); err != nil {
		t.Fatalf("browser credential was revoked by CLI logout: %v", err)
	}

	logout, err := repository.RevokeCurrentCLICredential(ctx, MutationActor{
		SubjectID: owner.SubjectID, CredentialID: issued.CredentialID,
		RequestID: "cli-logout", TraceID: "trace-cli-logout",
	})
	if err != nil || logout.AuthorizationRevision <= issued.AuthorizationRevision {
		t.Fatalf("RevokeCurrentCLICredential = %#v, %v", logout, err)
	}
	if _, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(rawKey)); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("revoked CLI credential authentication error = %v", err)
	}
	var source, requestID, traceID string
	if err := pool.QueryRow(ctx, `
		SELECT metadata->>'credential_source', request_id, trace_id
		FROM fused_audit_events
		WHERE action = 'user.cli_credential.revoke' AND actor_credential_id = $1
	`, issued.CredentialID).Scan(&source, &requestID, &traceID); err != nil {
		t.Fatalf("load CLI logout audit: %v", err)
	}
	if source != "managed_cli_login" || requestID != "cli-logout" || traceID != "trace-cli-logout" {
		t.Fatalf("CLI logout audit = %q/%q/%q", source, requestID, traceID)
	}
}
