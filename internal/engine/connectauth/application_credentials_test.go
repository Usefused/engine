package connectauth

import (
	"context"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

type applicationCredentialTestStore struct {
	secrets      []store.WorkspaceSecret
	alternatives []store.SecretKeyAlternative
}

// GetFirstCompleteSecretSet captures the exact lookup and returns its bounded fixture rows.
func (s *applicationCredentialTestStore) GetFirstCompleteSecretSet(_ context.Context, _, _ uuid.UUID, alternatives []store.SecretKeyAlternative) ([]store.WorkspaceSecret, error) {
	s.alternatives = alternatives
	return s.secrets, nil
}

// TestApplicationCredentialResolverDecryptsDeterministicFamily covers the common connect/refresh resolver contract.
func TestApplicationCredentialResolverDecryptsDeterministicFamily(t *testing.T) {
	masterKey := []byte("12345678901234567890123456789012")
	bucketID, serviceID := uuid.New(), uuid.New()
	repository := &applicationCredentialTestStore{secrets: []store.WorkspaceSecret{
		encryptedApplicationCredentialTestRow(t, masterKey, bucketID, serviceID, "oauth2_client_id", "oauth", "client-id"),
		encryptedApplicationCredentialTestRow(t, masterKey, bucketID, serviceID, "oauth2_client_secret", "oauth", "client-secret"),
	}}
	resolver := NewApplicationCredentialResolver(repository, masterKey, "https://engine.example.com/workspace/connect/callback")
	credentials, err := resolver.Resolve(context.Background(), bucketID, serviceID, "oauth2", "oauth2")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	// Values and the configured callback must survive without exposing storage details to callers.
	if credentials.ClientID != "client-id" || credentials.ClientSecret != "client-secret" || credentials.RedirectURI != "https://engine.example.com/workspace/connect/callback" {
		t.Fatalf("Resolve() = %#v", credentials)
	}
	if len(repository.alternatives) != 1 || len(repository.alternatives[0].Required) != 2 {
		t.Fatalf("exact alternatives = %#v", repository.alternatives)
	}
}

// TestCanonicalCallbackURIRejectsRemoteHTTP proves redirects cannot derive from unsafe request origins.
func TestCanonicalCallbackURIRejectsRemoteHTTP(t *testing.T) {
	callback, err := CanonicalCallbackURI("https://engine.example.com/base/")
	if err != nil || callback != "https://engine.example.com/base/workspace/connect/callback" {
		t.Fatalf("CanonicalCallbackURI() = %q, %v", callback, err)
	}
	// Remote cleartext origins are not valid OAuth public identity.
	if _, err := CanonicalCallbackURI("http://engine.example.com"); err == nil {
		t.Fatal("CanonicalCallbackURI accepted remote HTTP")
	}
	loopback, err := CanonicalCallbackURI("http://127.0.0.1:8081")
	if err != nil || loopback != "http://127.0.0.1:8081/workspace/connect/callback" {
		t.Fatalf("loopback callback = %q, %v", loopback, err)
	}
}

// encryptedApplicationCredentialTestRow creates one production-shaped encrypted fixture row.
func encryptedApplicationCredentialTestRow(t *testing.T, masterKey []byte, bucketID, serviceID uuid.UUID, keyName, credentialType, value string) store.WorkspaceSecret {
	t.Helper()
	wrappedDEK, dek, err := store.WrapDEK(masterKey)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	encrypted, err := store.EncryptWithDEK(dek, value)
	if err != nil {
		t.Fatalf("EncryptWithDEK: %v", err)
	}
	return store.WorkspaceSecret{WorkspaceSecretMeta: store.WorkspaceSecretMeta{
		BucketID: bucketID, ServiceID: serviceID, KeyName: keyName, CredentialType: credentialType,
	}, EncryptedDEK: wrappedDEK, EncryptedValue: encrypted}
}
