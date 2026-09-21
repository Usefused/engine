package connectauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
)

type delegatedFixture struct {
	source        uuid.UUID
	name          string
	applicationID string
	calls         int
	unavailable   bool
}

// ClientID models discovery without exporting application credentials.
func (f *delegatedFixture) ClientID(_ context.Context, _ uuid.UUID, _ string, ids ...string) (string, error) {
	f.calls++
	f.applicationID = ids[0]
	// Unavailable remote registration must never fall back to a local credential.
	if f.unavailable {
		return "", errors.New("unavailable")
	}
	return "remote-client", nil
}

// Exchange captures the selected source, which can differ from the target connection service.
func (f *delegatedFixture) Exchange(_ context.Context, id uuid.UUID, name, _ string, _ fusedobject.AuthConfig, _ fusedobject.OAuth2FlowContract, _, _ string, ids ...string) (TokenResponse, error) {
	f.source, f.name = id, name
	f.applicationID = ids[0]
	f.calls++
	return TokenResponse{AccessToken: "remote-access"}, nil
}

// Refresh uses the same exact delegated identity as exchange instead of re-deriving it from a target connection.
func (f *delegatedFixture) Refresh(ctx context.Context, id uuid.UUID, name, redirect string, auth fusedobject.AuthConfig, flow fusedobject.OAuth2FlowContract, token string, ids ...string) (TokenResponse, error) {
	return f.Exchange(ctx, id, name, redirect, auth, flow, token, "", ids...)
}

// TestOAuthApplicationDelegatesExactSource keeps source selection out of connection and refresh orchestration.
func TestOAuthApplicationDelegatesExactSource(t *testing.T) {
	repository := &applicationCredentialTestStore{}
	resolver := NewApplicationCredentialResolver(repository, make([]byte, 32), "https://consumer.example/callback")
	source := ApplicationCredentialSource{ServiceID: uuid.New(), AuthType: "oauth", AuthName: "SourceOAuth", Managed: true, ManagedApplicationID: uuid.NewString()}
	request := ApplicationRequest{BucketID: uuid.New(), ServiceID: uuid.New(), AuthType: "oauth", AuthName: "TargetOAuth", Source: source}
	remote := &delegatedFixture{}
	creds, grant, err := resolver.ResolveApplication(t.Context(), request, http.DefaultClient, remote)
	// Delegated discovery must not query a local bucket or return an application secret.
	if err != nil || creds.ClientSecret != "" || len(repository.alternatives) != 0 {
		t.Fatal("delegated resolution crossed credential boundary", err)
	}
	_, err = grant.Exchange(t.Context(), "code", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	_, err = grant.Refresh(t.Context(), "refresh")
	if err != nil {
		t.Fatal(err)
	}
	// Both operations must target the resolved source, never the consuming service's identity.
	if remote.source != source.ServiceID || remote.name != source.AuthName || remote.calls != 3 || remote.applicationID != source.ManagedApplicationID {
		t.Fatal("delegated source identity drifted")
	}
}

// TestOAuthApplicationNoImplicitFallback prevents missing local credentials and missing remote authority from changing source ownership.
func TestOAuthApplicationNoImplicitFallback(t *testing.T) {
	resolver := NewApplicationCredentialResolver(&applicationCredentialTestStore{}, make([]byte, 32), "")
	remote := &delegatedFixture{}
	request := ApplicationRequest{BucketID: uuid.New(), ServiceID: uuid.New(), AuthType: "oauth", AuthName: "OAuth"}
	// A configured broker is not permission to use it when local lookup fails.
	if _, _, err := resolver.ResolveApplication(t.Context(), request, http.DefaultClient, remote); err == nil || remote.calls != 0 {
		t.Fatal("local lookup silently delegated")
	}
	request.Source = ApplicationCredentialSource{ServiceID: uuid.New(), AuthName: "OAuth", Managed: true}
	// Explicit delegation still requires an available transport and registration.
	if _, _, err := resolver.ResolveApplication(t.Context(), request, http.DefaultClient, nil); err == nil {
		t.Fatal("missing delegation authority accepted")
	}
	remote.unavailable = true
	if _, _, err := resolver.ResolveApplication(t.Context(), request, http.DefaultClient, remote); err == nil {
		t.Fatal("unavailable delegation accepted")
	}
}

type applicationTransport func(*http.Request) (*http.Response, error)

// RoundTrip observes the ordinary core OAuth helper without introducing an external provider dependency.
func (f applicationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestOAuthApplicationLocalUsesExistingHelpers proves local exchange/refresh retain encryption resolution and PKCE semantics.
func TestOAuthApplicationLocalUsesExistingHelpers(t *testing.T) {
	key := []byte("12345678901234567890123456789012")
	bucket, service := uuid.New(), uuid.New()
	repository := &applicationCredentialTestStore{secrets: []store.WorkspaceSecret{
		encryptedApplicationCredentialTestRow(t, key, bucket, service, "OAuth_client_id", "oauth", "local-client"),
		encryptedApplicationCredentialTestRow(t, key, bucket, service, "OAuth_client_secret", "oauth", "local-secret"),
	}}
	request := ApplicationRequest{BucketID: bucket, ServiceID: service, AuthType: "oauth", AuthName: "OAuth", Auth: fusedobject.AuthConfig{Type: "oauth2", PKCERequired: true, TokenEndpointAuthMethod: fusedobject.TokenEndpointAuthMethodClientSecretPost}, Flow: fusedobject.OAuth2FlowContract{TokenURL: "https://provider.example/token"}}
	calls := 0
	client := &http.Client{Transport: applicationTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		// Both local grant types continue to use the ordinary resolved bucket pair.
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("client_secret") != "local-secret" {
			t.Fatal("local credential missing")
		}
		// Existing PKCE policy must survive the adapter extraction without changing refresh grant semantics.
		if r.Form.Get("grant_type") == "authorization_code" && r.Form.Get("code_verifier") != "verifier" {
			t.Fatal("PKCE was lost")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"access_token":"local-access"}`))}, nil
	})}
	_, grant, err := NewApplicationCredentialResolver(repository, key, "https://consumer.example/callback").ResolveApplication(t.Context(), request, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = grant.Exchange(t.Context(), "code", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	_, err = grant.Refresh(t.Context(), "refresh")
	if err != nil || calls != 2 {
		t.Fatal("local grant lifecycle changed", err)
	}
}
