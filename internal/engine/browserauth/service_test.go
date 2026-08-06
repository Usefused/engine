package browserauth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type browserAuthStoreFixture struct {
	issued     store.BrowserSessionCredential
	issueActor accesscontrol.Actor
	revoked    accesscontrol.Actor
	logout     store.BrowserLogoutContext
}

func (f *browserAuthStoreFixture) IssueBrowserSession(_ context.Context, actor accesscontrol.Actor, method string, expiresAt time.Time) (store.BrowserSessionCredential, error) {
	f.issueActor = actor
	if method != "api_key" || expiresAt.IsZero() {
		return store.BrowserSessionCredential{}, errors.New("unexpected browser session input")
	}
	return f.issued, nil
}

func (f *browserAuthStoreFixture) RevokeBrowserSession(_ context.Context, actor accesscontrol.Actor, _ time.Time) (store.BrowserLogoutContext, error) {
	f.revoked = actor
	return f.logout, nil
}

type browserAuthRegistryFixture struct {
	token, returnURL, logoutURL string
	err                         error
}

func (f *browserAuthRegistryFixture) StartManagedLogout(_ context.Context, token, returnURL string) (string, error) {
	f.token, f.returnURL = token, returnURL
	return f.logoutURL, f.err
}

var browserAuthTestMasterKey = []byte("01234567890123456789012345678901")

type browserAuthAuthenticatorFixture struct {
	actor    accesscontrol.Actor
	revision int64
}

func (f *browserAuthAuthenticatorFixture) AuthenticateControlCredential(context.Context, string) (accesscontrol.Actor, error) {
	return f.actor, nil
}

func (f *browserAuthAuthenticatorFixture) SetRevision(revision int64) bool {
	f.revision = revision
	return true
}

func TestServiceExchangesAPIKeyAndRevokesBrowserSession(t *testing.T) {
	actor := accesscontrol.Actor{SubjectID: uuid.New(), CredentialID: uuid.New(), Kind: accesscontrol.SubjectBootstrap}
	repository := &browserAuthStoreFixture{
		issued: store.BrowserSessionCredential{CredentialID: uuid.New(), RawKey: "fsk_derived", ExpiresAt: time.Now().Add(time.Hour), AuthorizationRevision: 8},
		logout: store.BrowserLogoutContext{AuthorizationRevision: 9},
	}
	authenticator := &browserAuthAuthenticatorFixture{actor: actor}
	service, err := NewService(repository, authenticator, testCookieManager(t), &browserAuthRegistryFixture{}, browserAuthTestMasterKey)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	credential, err := service.ExchangeLicenseKey(t.Context(), "license-secret")
	if err != nil || credential.RawKey != "fsk_derived" || repository.issueActor.SubjectID != actor.SubjectID || authenticator.revision != 8 {
		t.Fatalf("ExchangeLicenseKey = %#v/%v actor=%#v revision=%d", credential, err, repository.issueActor, authenticator.revision)
	}
	authenticator.actor.CredentialID = credential.CredentialID
	authenticator.actor.CredentialSource = "license_exchange"
	if _, err := service.Logout(t.Context(), credential.RawKey, "https://engine.test/login"); err != nil || repository.revoked.CredentialID != credential.CredentialID || authenticator.revision != 9 {
		t.Fatalf("Logout = %v actor=%#v revision=%d", err, repository.revoked, authenticator.revision)
	}
}

func TestServiceRejectsOrdinaryCredentialAsBrowserSession(t *testing.T) {
	authenticator := &browserAuthAuthenticatorFixture{actor: accesscontrol.Actor{
		SubjectID: uuid.New(), CredentialID: uuid.New(), Kind: accesscontrol.SubjectUser,
		CredentialSource: "api_key",
	}}
	service, err := NewService(&browserAuthStoreFixture{}, authenticator, testCookieManager(t), &browserAuthRegistryFixture{}, browserAuthTestMasterKey)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := service.Session(t.Context(), "ordinary-key"); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("Session error = %v", err)
	}
	if _, err := service.Logout(t.Context(), "ordinary-key", "https://engine.test/login"); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("Logout error = %v", err)
	}
}

func TestServiceAcceptsUserAPIKey(t *testing.T) {
	authenticator := &browserAuthAuthenticatorFixture{actor: accesscontrol.Actor{
		SubjectID: uuid.New(), CredentialID: uuid.New(), Kind: accesscontrol.SubjectUser,
	}}
	repository := &browserAuthStoreFixture{issued: store.BrowserSessionCredential{CredentialID: uuid.New(), RawKey: "browser", ExpiresAt: time.Now().Add(time.Hour)}}
	service, err := NewService(repository, authenticator, testCookieManager(t), &browserAuthRegistryFixture{}, browserAuthTestMasterKey)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := service.ExchangeLicenseKey(t.Context(), "user-key"); err != nil || repository.issueActor.SubjectID != authenticator.actor.SubjectID {
		t.Fatalf("user credential exchange error = %v", err)
	}
}

func TestServiceRevokesLocallyBeforeStartingManagedProviderLogout(t *testing.T) {
	token := "opaque-registry-logout-capability"
	wrapped, dek, err := store.WrapDEK(browserAuthTestMasterKey)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	encrypted, err := store.EncryptWithDEK(dek, token)
	if err != nil {
		t.Fatalf("EncryptWithDEK: %v", err)
	}
	actor := accesscontrol.Actor{
		SubjectID: uuid.New(), CredentialID: uuid.New(), Kind: accesscontrol.SubjectUser,
		CredentialSource: "managed_login",
	}
	repository := &browserAuthStoreFixture{logout: store.BrowserLogoutContext{
		AuthorizationRevision: 11, EncryptedDEK: wrapped, EncryptedLogoutToken: encrypted,
		ExpiresAt: time.Now().Add(time.Hour),
	}}
	registry := &browserAuthRegistryFixture{logoutURL: "https://tenant.logto.test/oidc/session/end?state=safe"}
	authenticator := &browserAuthAuthenticatorFixture{actor: actor}
	service, err := NewService(repository, authenticator, testCookieManager(t), registry, browserAuthTestMasterKey)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	result, err := service.Logout(t.Context(), "browser-session", "https://engine.test/login")
	if err != nil || result.LogoutURL != registry.logoutURL {
		t.Fatalf("Logout = %#v, %v", result, err)
	}
	if repository.revoked.SubjectID != actor.SubjectID || repository.revoked.CredentialID != actor.CredentialID || authenticator.revision != 11 || registry.token != token || registry.returnURL != "https://engine.test/login" {
		t.Fatalf("managed logout inputs: revoked=%#v revision=%d registry=%#v", repository.revoked, authenticator.revision, registry)
	}
}

func TestServiceKeepsLocalLogoutSuccessfulWhenRegistryIsUnavailable(t *testing.T) {
	wrapped, dek, err := store.WrapDEK(browserAuthTestMasterKey)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	encrypted, err := store.EncryptWithDEK(dek, "opaque-registry-logout-capability")
	if err != nil {
		t.Fatalf("EncryptWithDEK: %v", err)
	}
	actor := accesscontrol.Actor{
		SubjectID: uuid.New(), CredentialID: uuid.New(), Kind: accesscontrol.SubjectUser,
		CredentialSource: "managed_login",
	}
	repository := &browserAuthStoreFixture{logout: store.BrowserLogoutContext{
		AuthorizationRevision: 12, EncryptedDEK: wrapped, EncryptedLogoutToken: encrypted,
		ExpiresAt: time.Now().Add(time.Hour),
	}}
	registry := &browserAuthRegistryFixture{err: errors.New("registry unavailable")}
	authenticator := &browserAuthAuthenticatorFixture{actor: actor}
	service, err := NewService(repository, authenticator, testCookieManager(t), registry, browserAuthTestMasterKey)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	result, err := service.Logout(t.Context(), "browser-session", "https://engine.test/login")
	if err != nil || result.LogoutURL != "" || repository.revoked.CredentialID != actor.CredentialID || authenticator.revision != 12 {
		t.Fatalf("local logout after Registry failure = %#v/%v revoked=%#v revision=%d", result, err, repository.revoked, authenticator.revision)
	}
}

func TestServiceKeepsBrowserCredentialsOutOfOTEL(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(t.Context())
		otel.SetTracerProvider(previous)
	})

	licenseKey := "license-secret-not-for-telemetry"
	sessionKey := "fsk_session-secret-not-for-telemetry"
	credentialID := uuid.New()
	authenticator := &browserAuthAuthenticatorFixture{actor: accesscontrol.Actor{
		SubjectID: uuid.New(), CredentialID: uuid.New(), Kind: accesscontrol.SubjectBootstrap,
	}}
	repository := &browserAuthStoreFixture{
		issued: store.BrowserSessionCredential{
			CredentialID: credentialID, RawKey: sessionKey,
			ExpiresAt: time.Now().Add(time.Hour), AuthorizationRevision: 4,
		},
		logout: store.BrowserLogoutContext{AuthorizationRevision: 5},
	}
	service, err := NewService(repository, authenticator, testCookieManager(t), &browserAuthRegistryFixture{}, browserAuthTestMasterKey)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	credential, err := service.ExchangeLicenseKey(t.Context(), licenseKey)
	if err != nil {
		t.Fatalf("ExchangeLicenseKey: %v", err)
	}
	authenticator.actor.CredentialID = credentialID
	authenticator.actor.CredentialSource = "license_exchange"
	if _, err := service.Logout(t.Context(), credential.RawKey, "https://engine.test/login"); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	var telemetry []string
	for _, span := range exporter.GetSpans() {
		telemetry = append(telemetry, span.Name)
		for _, item := range span.Attributes {
			telemetry = append(telemetry, string(item.Key), fmt.Sprint(item.Value.AsInterface()))
		}
	}
	joined := strings.Join(telemetry, " ")
	for _, secret := range []string{licenseKey, sessionKey} {
		if strings.Contains(joined, secret) {
			t.Fatalf("browser credential leaked into OTEL: %q", secret)
		}
	}
}
