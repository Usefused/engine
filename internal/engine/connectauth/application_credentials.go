package connectauth

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/credentialkeys"
	"github.com/google/uuid"
)

const callbackPath = "/workspace/connect/callback"

// ErrApplicationCredentialsUnavailable is the secret-safe resolver miss returned across connect and refresh.
var ErrApplicationCredentialsUnavailable = errors.New("oauth application credentials unavailable")

// ApplicationCredentialStore is the exact set-based lookup shared by every OAuth/OIDC runtime caller.
type ApplicationCredentialStore interface {
	GetFirstCompleteSecretSet(context.Context, uuid.UUID, uuid.UUID, []store.SecretKeyAlternative) ([]store.WorkspaceSecret, error)
}

// ApplicationCredentialResolver owns exact family lookup, reference rebasing, and decryption.
type ApplicationCredentialResolver struct {
	store       ApplicationCredentialStore
	masterKey   []byte
	redirectURI string
}

// ApplicationCredentialSource is the immutable app-selected storage family used for client registration lookup.
type ApplicationCredentialSource struct {
	ServiceID uuid.UUID
	AuthType  string
	AuthName  string
}

// NewApplicationCredentialResolver fixes shared dependencies without exposing credential values to adapters.
func NewApplicationCredentialResolver(s ApplicationCredentialStore, masterKey []byte, redirectURI string) *ApplicationCredentialResolver {
	return &ApplicationCredentialResolver{store: s, masterKey: append([]byte(nil), masterKey...), redirectURI: redirectURI}
}

// Resolve returns one complete direct or referenced OAuth/OIDC client registration.
func (r *ApplicationCredentialResolver) Resolve(ctx context.Context, bucketID, targetServiceID uuid.UUID, authType, authName string, sources ...ApplicationCredentialSource) (ClientCredentials, error) {
	authType = canonicalApplicationCredentialType(authType)
	// Only OAuth families may enter the application-registration resolver.
	if applicationResolverUnavailable(r) || applicationCredentialIdentityMissing(bucketID, targetServiceID) || authType == "" {
		return ClientCredentials{}, ErrApplicationCredentialsUnavailable
	}
	alternative, clientIDKey, clientSecretKey, ok := applicationCredentialAlternative(authType, authName, firstApplicationCredentialSource(sources))
	// An exact named family is required for deterministic direct and referenced lookup.
	if !ok {
		return ClientCredentials{}, ErrApplicationCredentialsUnavailable
	}
	secrets, err := r.store.GetFirstCompleteSecretSet(ctx, bucketID, targetServiceID, []store.SecretKeyAlternative{alternative})
	// Store failures remain distinguishable internally while absence is normalized below.
	if err != nil {
		return ClientCredentials{}, err
	}
	// Exactly two projected rows prove the selected direct or referenced family is complete.
	if len(secrets) != 2 {
		return ClientCredentials{}, ErrApplicationCredentialsUnavailable
	}
	values, err := decryptApplicationCredentialRows(secrets, r.masterKey, authType)
	if err != nil {
		return ClientCredentials{}, err
	}
	credentials, complete := resolvedApplicationCredentialValues(values, clientIDKey, clientSecretKey, r.redirectURI)
	// Empty or duplicate/missing decrypted values fail closed before provider traffic.
	if !complete {
		return ClientCredentials{}, ErrApplicationCredentialsUnavailable
	}
	return credentials, nil
}

// firstApplicationCredentialSource returns the optional immutable app source while preserving direct lookup by default.
func firstApplicationCredentialSource(sources []ApplicationCredentialSource) ApplicationCredentialSource {
	// Runtime callers supply at most one source because one app selection owns the target scheme.
	if len(sources) == 0 {
		return ApplicationCredentialSource{}
	}
	return sources[0]
}

// applicationResolverUnavailable keeps nil dependency admission separate from credential selection.
func applicationResolverUnavailable(resolver *ApplicationCredentialResolver) bool {
	return resolver == nil || resolver.store == nil
}

// applicationCredentialIdentityMissing rejects lookups that cannot identify one exact bucket and service.
func applicationCredentialIdentityMissing(bucketID, serviceID uuid.UUID) bool {
	return bucketID == uuid.Nil || serviceID == uuid.Nil
}

// applicationCredentialAlternative builds the one exact key family accepted by the set-based store lookup.
func applicationCredentialAlternative(authType, authName string, source ApplicationCredentialSource) (store.SecretKeyAlternative, string, string, bool) {
	clientIDKey, clientSecretKey, ok := credentialkeys.OAuthApplication(authName)
	// Invalid names cannot produce deterministic bucket keys.
	if !ok {
		return store.SecretKeyAlternative{}, "", "", false
	}
	name := strings.TrimSpace(authName)
	alternative := store.SecretKeyAlternative{
		Required:  []string{clientIDKey, clientSecretKey},
		AuthNames: map[string]string{clientIDKey: name, clientSecretKey: name},
		AuthTypes: map[string]string{clientIDKey: authType, clientSecretKey: authType},
	}
	// A present source must be complete and same-family before it can rebase storage keys.
	if source.ServiceID != uuid.Nil {
		sourceType := canonicalApplicationCredentialType(source.AuthType)
		if sourceType != authType || strings.TrimSpace(source.AuthName) == "" {
			return store.SecretKeyAlternative{}, "", "", false
		}
		alternative.SourceServiceID = source.ServiceID
		alternative.SourceAuthType = sourceType
		alternative.SourceAuthName = strings.TrimSpace(source.AuthName)
	}
	return alternative, clientIDKey, clientSecretKey, true
}

// resolvedApplicationCredentialValues converts a complete decrypted family into provider input.
func resolvedApplicationCredentialValues(values map[string]string, clientIDKey, clientSecretKey, redirectURI string) (ClientCredentials, bool) {
	clientID, idOK := values[clientIDKey]
	clientSecret, secretOK := values[clientSecretKey]
	// Both non-empty semantic values are required before provider traffic.
	if !idOK || !secretOK || strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return ClientCredentials{}, false
	}
	return ClientCredentials{ClientID: clientID, ClientSecret: clientSecret, RedirectURI: redirectURI}, true
}

// decryptApplicationCredentialRows decrypts only the two rows selected by the exact database query.
func decryptApplicationCredentialRows(secrets []store.WorkspaceSecret, masterKey []byte, authType string) (map[string]string, error) {
	values := make(map[string]string, len(secrets))
	for _, secret := range secrets {
		// Credential type must remain in the reviewed OAuth/OIDC family after reference rebasing.
		if canonicalApplicationCredentialType(secret.CredentialType) != authType {
			return nil, ErrApplicationCredentialsUnavailable
		}
		dek, err := store.UnwrapDEK(masterKey, secret.EncryptedDEK)
		if err != nil {
			return nil, err
		}
		value, err := store.DecryptWithDEK(dek, secret.EncryptedValue)
		if err != nil {
			return nil, err
		}
		// Duplicate projected keys indicate a violated exact-set invariant.
		if _, exists := values[secret.KeyName]; exists {
			return nil, ErrApplicationCredentialsUnavailable
		}
		values[secret.KeyName] = value
	}
	return values, nil
}

// canonicalApplicationCredentialType admits imported spellings but only returns OAuth/OIDC families.
func canonicalApplicationCredentialType(value string) string {
	normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), "-", "_"))
	switch normalized {
	case "oauth", "oauth2", "oauth2_authorization_code":
		return "oauth"
	case "oidc", "openidconnect", "open_id_connect":
		return "oidc"
	default:
		// Static and user-token credential types cannot be reinterpreted as app registrations.
		return ""
	}
}

// CanonicalCallbackURI validates the configured Engine public URL and appends the fixed callback path.
func CanonicalCallbackURI(publicURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(publicURL))
	// Relative, credential-bearing, or request-derived URLs are never valid public identity.
	if err != nil || parsed == nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("engine public URL must be an absolute origin")
	}
	// HTTPS is mandatory except for explicit loopback development origins.
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHostname(parsed.Hostname())) {
		return "", errors.New("engine public URL must use https or loopback http")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + callbackPath
	parsed.RawPath = ""
	return parsed.String(), nil
}

// isLoopbackHostname permits local fake-provider and developer flows without weakening remote redirects.
func isLoopbackHostname(host string) bool {
	// The conventional local hostname is accepted alongside numeric loopback addresses.
	if strings.EqualFold(strings.TrimSpace(host), "localhost") {
		return true
	}
	return net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}
