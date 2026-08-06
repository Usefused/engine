package browserauth

import (
	"context"
	"errors"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

const (
	defaultBrowserSessionTTL = 8 * time.Hour
	providerLogoutTimeout    = 10 * time.Second
)

var ErrAPIKeyRequired = errors.New("valid API Key required")

// ErrLicenseKeyRequired remains an alias for callers compiled against the
// original bootstrap-only name. The exchange now accepts any active Engine
// control credential and never sends that source credential to Logto.
var ErrLicenseKeyRequired = ErrAPIKeyRequired

func IsBrowserSessionActor(actor accesscontrol.Actor) bool {
	return actor.CredentialSource == "managed_login" || actor.CredentialSource == "license_exchange" || actor.CredentialSource == "api_key_exchange"
}

type Authenticator interface {
	AuthenticateControlCredential(context.Context, string) (accesscontrol.Actor, error)
	SetRevision(int64) bool
}

type ManagedLogoutRegistry interface {
	StartManagedLogout(context.Context, string, string) (string, error)
}

type LogoutResult struct {
	LogoutURL string
}

type Service struct {
	store     store.BrowserSessionStore
	auth      Authenticator
	cookies   *CookieManager
	registry  ManagedLogoutRegistry
	masterKey []byte
	now       func() time.Time
	ttl       time.Duration
}

func NewService(
	repository store.BrowserSessionStore,
	authenticator Authenticator,
	cookies *CookieManager,
	registry ManagedLogoutRegistry,
	masterKey []byte,
) (*Service, error) {
	if repository == nil || authenticator == nil || cookies == nil || registry == nil || len(masterKey) != 32 {
		return nil, errors.New("invalid browser authentication configuration")
	}
	return &Service{
		store: repository, auth: authenticator, cookies: cookies, registry: registry,
		masterKey: append([]byte(nil), masterKey...), now: time.Now, ttl: defaultBrowserSessionTTL,
	}, nil
}

func (s *Service) ExchangeLicenseKey(ctx context.Context, licenseKey string) (store.BrowserSessionCredential, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.identity.api_key.exchange")
	defer span.End()
	span.SetAttributes(attribute.String("actor.type", "user"), attribute.String("identity.auth_method", "api_key"))
	actor, err := s.auth.AuthenticateControlCredential(ctx, licenseKey)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "denied"))
		return store.BrowserSessionCredential{}, ErrAPIKeyRequired
	}
	credential, err := s.store.IssueBrowserSession(ctx, actor, "api_key", s.now().UTC().Add(s.ttl))
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "failed"))
		return store.BrowserSessionCredential{}, err
	}
	s.auth.SetRevision(credential.AuthorizationRevision)
	span.SetAttributes(attribute.String("outcome", "authenticated"))
	return credential, nil
}

func (s *Service) Session(ctx context.Context, rawCredential string) (accesscontrol.Actor, error) {
	actor, err := s.auth.AuthenticateControlCredential(ctx, rawCredential)
	if err != nil || !IsBrowserSessionActor(actor) {
		return accesscontrol.Actor{}, accesscontrol.ErrAuthenticationRequired
	}
	return actor, nil
}

func (s *Service) Logout(ctx context.Context, rawCredential, returnURL string) (LogoutResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.identity.session.logout")
	defer span.End()
	span.SetAttributes(attribute.String("actor.type", "user"))
	actor, err := s.auth.AuthenticateControlCredential(ctx, rawCredential)
	if err != nil || !IsBrowserSessionActor(actor) {
		span.SetAttributes(attribute.String("outcome", "denied"))
		return LogoutResult{}, accesscontrol.ErrAuthenticationRequired
	}
	logout, err := s.store.RevokeBrowserSession(ctx, actor, s.now().UTC())
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "failed"))
		return LogoutResult{}, err
	}
	s.auth.SetRevision(logout.AuthorizationRevision)
	result, providerOutcome := s.startManagedLogout(ctx, logout, returnURL)
	span.SetAttributes(
		attribute.String("outcome", "logged_out"),
		attribute.String("identity.provider_logout", providerOutcome),
	)
	return result, nil
}

func (s *Service) startManagedLogout(ctx context.Context, logout store.BrowserLogoutContext, returnURL string) (LogoutResult, string) {
	if logout.EncryptedLogoutToken == "" || !logout.ExpiresAt.After(s.now().UTC()) {
		return LogoutResult{}, "not_applicable"
	}
	dek, err := store.UnwrapDEK(s.masterKey, logout.EncryptedDEK)
	if err != nil {
		return LogoutResult{}, "unavailable"
	}
	token, err := store.DecryptWithDEK(dek, logout.EncryptedLogoutToken)
	if err != nil {
		return LogoutResult{}, "unavailable"
	}
	// Local revocation already committed, so a browser disconnect must not
	// cancel the bounded Registry handoff halfway through.
	registryContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), providerLogoutTimeout)
	defer cancel()
	logoutURL, err := s.registry.StartManagedLogout(registryContext, token, returnURL)
	if err != nil {
		return LogoutResult{}, "unavailable"
	}
	return LogoutResult{LogoutURL: logoutURL}, "initiated"
}

func (s *Service) Cookies() *CookieManager { return s.cookies }
