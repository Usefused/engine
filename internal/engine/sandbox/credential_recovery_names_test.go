package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
	"strings"
	"testing"
)

type namedCredentialStore struct {
	*resolverMockStore
	serviceID, bucketID uuid.UUID
}

// CredentialRecoveryNames records the failing routing targets without reading secret values.
func (s *namedCredentialStore) CredentialRecoveryNames(_ context.Context, serviceID, bucketID uuid.UUID) (string, string, error) {
	s.serviceID, s.bucketID = serviceID, bucketID
	return "@stripe/payments", "team's production", nil
}

// TestCredentialRecoveryUsesNames verifies the resolver enriches the exact selected service and bucket after failure.
func TestCredentialRecoveryUsesNames(t *testing.T) {
	bucketID, serviceID := uuid.New(), uuid.New()
	db := &namedCredentialStore{resolverMockStore: &resolverMockStore{appRuntime: &store.AppRuntime{BucketID: bucketID}}}
	resolver := NewSecretResolver(db, []byte("12345678901234567890123456789012"))
	_, _, err := resolver.ResolveExecutionCredentials(context.Background(), CredentialRequest{AppID: uuid.New(), ServiceID: serviceID, AuthType: "api_key", Auths: fusedobject.AuthConfigs{{Name: "providerKey", Type: "apiKey"}}, Requirements: singleAuthRequirement("providerKey")})
	var missing *CredentialMaterialMissingError
	// Typed machine identity remains unchanged even though the interactive command becomes readable.
	if !errors.As(err, &missing) || db.serviceID != serviceID || db.bucketID != bucketID {
		t.Fatalf("wrong identity: %v", err)
	}
	command := missing.Command()
	// Apostrophes must stay inside one shell argument, not terminate the bucket name.
	if !strings.Contains(command, "secret set '@stripe/payments' --bucket 'team'\"'\"'s production'") {
		t.Fatalf("wrong command: %s", command)
	}
	raw, _ := json.Marshal(missing)
	// SDK adapters receive both immutable IDs and human-readable targets in structured data.
	if !strings.Contains(string(raw), `"service_slug":"@stripe/payments"`) || !strings.Contains(string(raw), serviceID.String()) {
		t.Fatalf("incomplete metadata: %s", raw)
	}
}
