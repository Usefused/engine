package cliauth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type cliLoginStoreFixture struct {
	created      store.CLILoginTransaction
	approvedID   uuid.UUID
	approvedHash string
	approved     accesscontrol.Actor
	consumedHash string
	credential   store.CLILoginCredential
	consumeErr   error
	logoutActor  store.MutationActor
	logout       store.CLILogoutResult
	logoutErr    error
}

func (f *cliLoginStoreFixture) CreateCLILoginTransaction(_ context.Context, transaction store.CLILoginTransaction) error {
	f.created = transaction
	return nil
}

func (f *cliLoginStoreFixture) ApproveCLILoginTransaction(_ context.Context, id uuid.UUID, hash string, actor accesscontrol.Actor, _ time.Time) error {
	f.approvedID, f.approvedHash, f.approved = id, hash, actor
	return nil
}

func (f *cliLoginStoreFixture) ConsumeCLILoginTransaction(_ context.Context, _ uuid.UUID, hash string, _ time.Time) (store.CLILoginCredential, error) {
	f.consumedHash = hash
	return f.credential, f.consumeErr
}

func (f *cliLoginStoreFixture) RevokeCurrentCLICredential(_ context.Context, actor store.MutationActor) (store.CLILogoutResult, error) {
	f.logoutActor = actor
	return f.logout, f.logoutErr
}

type revisionFixture struct{ revision int64 }

func (f *revisionFixture) SetRevision(revision int64) bool { f.revision = revision; return true }

func TestCLILoginStartStoresOnlyHashedCapabilities(t *testing.T) {
	repository, revisions := &cliLoginStoreFixture{}, &revisionFixture{}
	service, err := NewService(repository, revisions)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 6, 1, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	result, err := service.Start(t.Context(), StartInput{
		CredentialHash:   "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		CredentialPrefix: "fsk_abcd",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if repository.created.PollSecretHash == result.PollToken || repository.created.BrowserSecretHash == result.BrowserToken {
		t.Fatal("raw CLI login capability persisted")
	}
	if repository.created.ExpiresAt != now.Add(10*time.Minute) || repository.created.CredentialExpiresAt != now.Add(30*24*time.Hour) {
		t.Fatalf("unexpected expiry: %#v", repository.created)
	}
}

func TestCLILoginApproveAndPollUseIndependentCapabilities(t *testing.T) {
	repository := &cliLoginStoreFixture{credential: store.CLILoginCredential{
		SubjectID: uuid.New(), CredentialID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour), AuthorizationRevision: 7,
	}}
	revisions := &revisionFixture{}
	service, _ := NewService(repository, revisions)
	id := uuid.New()
	browserToken, _ := randomToken()
	pollToken, _ := randomToken()
	actor := accesscontrol.Actor{
		SubjectID: uuid.New(), CredentialID: uuid.New(), CredentialSource: "managed_login",
		AuthenticationMethod: "google",
	}
	if err := service.Approve(t.Context(), id, browserToken, actor); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if repository.approvedID != id || repository.approvedHash != hashSecret(browserToken) {
		t.Fatal("browser approval capability was not hashed")
	}
	result, err := service.Poll(t.Context(), id, pollToken)
	if err != nil || result.CredentialID != repository.credential.CredentialID {
		t.Fatalf("Poll = %#v, %v", result, err)
	}
	if repository.consumedHash != hashSecret(pollToken) || revisions.revision != 7 {
		t.Fatal("poll capability or authorization revision was not applied")
	}
}

func TestCLILoginRejectsInvalidCommitmentAndNonBrowserActor(t *testing.T) {
	service, _ := NewService(&cliLoginStoreFixture{}, &revisionFixture{})
	if _, err := service.Start(t.Context(), StartInput{CredentialHash: "secret", CredentialPrefix: "fsk_abcd"}); !errors.Is(err, store.ErrCLILoginDenied) {
		t.Fatalf("invalid commitment error = %v", err)
	}
	token, _ := randomToken()
	actor := accesscontrol.Actor{SubjectID: uuid.New(), CredentialID: uuid.New(), CredentialSource: "api_key"}
	if err := service.Approve(t.Context(), uuid.New(), token, actor); !errors.Is(err, store.ErrCLILoginDenied) {
		t.Fatalf("non-browser approval error = %v", err)
	}
}

func TestCLILogoutRevokesOnlyManagedCurrentCredential(t *testing.T) {
	previousTracer := otel.GetTracerProvider()
	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := trace.NewTracerProvider(trace.WithSpanProcessor(spanRecorder))
	otel.SetTracerProvider(tracerProvider)
	defer func() {
		_ = tracerProvider.Shutdown(context.Background())
		otel.SetTracerProvider(previousTracer)
	}()
	actor := accesscontrol.Actor{
		SubjectID: uuid.New(), CredentialID: uuid.New(), CredentialSource: "managed_cli_login",
		Kind: accesscontrol.SubjectUser,
	}
	repository := &cliLoginStoreFixture{logout: store.CLILogoutResult{AuthorizationRevision: 9}}
	revisions := &revisionFixture{}
	service, _ := NewService(repository, revisions)
	if err := service.Logout(t.Context(), LogoutInput{Actor: actor, RequestID: "cli-logout"}); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if repository.logoutActor.SubjectID != actor.SubjectID || repository.logoutActor.CredentialID != actor.CredentialID || repository.logoutActor.RequestID != "cli-logout" {
		t.Fatalf("logout actor = %#v", repository.logoutActor)
	}
	if revisions.revision != 9 {
		t.Fatalf("authorization revision = %d, want 9", revisions.revision)
	}
	assertCLILogoutSpan(t, spanRecorder)

	actor.CredentialSource = "license_exchange"
	if err := service.Logout(t.Context(), LogoutInput{Actor: actor}); !errors.Is(err, store.ErrCLILogoutDenied) {
		t.Fatalf("non-CLI logout error = %v", err)
	}
}

func assertCLILogoutSpan(t *testing.T, recorder *tracetest.SpanRecorder) {
	t.Helper()
	for _, span := range recorder.Ended() {
		if span.Name() != "engine.identity.cli.logout" {
			continue
		}
		for _, item := range span.Attributes() {
			if string(item.Key) == "outcome" && item.Value.AsString() == "revoked" {
				return
			}
		}
	}
	t.Fatal("successful CLI logout OTEL span was not recorded")
}
