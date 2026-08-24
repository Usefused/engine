package sandbox

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/serverrouting"
	"github.com/google/uuid"
)

// TestAppServerVariableInjectionResolvesThroughOneBucketBatch exercises the
// stored app selection, dynamic bucket resolver, and service routing boundary.
func TestAppServerVariableInjectionResolvesThroughOneBucketBatch(t *testing.T) {
	appID, bucketID, serviceID := uuid.New(), uuid.New(), uuid.New()
	selections, err := json.Marshal([]models.SDKSelection{{
		ServiceID: serviceID, ServiceVersionID: uuid.New(), SchemaVersion: models.AppSelectionSchemaVersion,
		Injections: []models.SDKInjectionConfig{{
			Location: serverVariableBindingLocation, Name: "app_id", Value: "${bucket.values.SENDBIRD_APP_ID}", Mode: "force",
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	repository := &serverVariableResolverStore{
		resolverMockStore: &resolverMockStore{appRuntime: &store.AppRuntime{AppID: appID, BucketID: bucketID, ScopeSchemaVersion: models.AppScopeSchemaVersion, Selections: selections}},
		bucketValue:       store.BucketValue{BucketID: bucketID, ServiceID: serviceID, KeyName: "SENDBIRD_APP_ID", Value: "sandbox-123"},
	}
	resolver := NewSecretResolver(repository, []byte("12345678901234567890123456789012"))
	_, values, err := resolver.ResolveExecutionCredentials(context.Background(), CredentialRequest{
		AppID: appID, ServiceID: serviceID, Requirements: anonymousAuthRequirement(),
	})
	if err != nil {
		t.Fatalf("ResolveExecutionCredentials: %v", err)
	}
	if repository.getBucketValuesCalls != 1 || strings.Join(repository.getBucketValueKeys, ",") != "SENDBIRD_APP_ID" {
		t.Fatalf("bucket queries = %d keys=%v", repository.getBucketValuesCalls, repository.getBucketValueKeys)
	}
	metadata := sendbirdServerMetadata(serviceID)
	service, resolution, err := serviceForRuntimeEnvironment(metadata, "", nil, values)
	if err != nil {
		t.Fatalf("serviceForRuntimeEnvironment: %v", err)
	}
	if service.BaseURL != "https://api-sandbox-123.sendbird.com" || resolution.Source != serverVariableSourceApp {
		t.Fatalf("service=%#v resolution=%#v", service, resolution)
	}
}

type serverVariableResolverStore struct {
	*resolverMockStore
	bucketValue          store.BucketValue
	getBucketValuesCalls int
	getBucketValueKeys   []string
}

// GetBucketValues records the targeted batch contract and returns only the
// fixture row that a production SQL predicate would select.
func (s *serverVariableResolverStore) GetBucketValues(_ context.Context, bucketID, serviceID uuid.UUID, keyNames []string) ([]store.BucketValue, error) {
	s.getBucketValuesCalls++
	s.getBucketValueKeys = append(s.getBucketValueKeys, keyNames...)
	// Scope mismatches fail the fixture closed instead of masking a resolver bug.
	if s.bucketValue.BucketID != bucketID || s.bucketValue.ServiceID != serviceID || len(keyNames) != 1 || keyNames[0] != s.bucketValue.KeyName {
		return nil, nil
	}
	return []store.BucketValue{s.bucketValue}, nil
}

// sendbirdServerMetadata keeps the provider template identical across the
// success and rejection tests so only the resolved bucket value changes.
func sendbirdServerMetadata(serviceID uuid.UUID) *fusedobject.ServiceMetadata {
	return &fusedobject.ServiceMetadata{
		ID: serviceID,
		Servers: fusedobject.Servers{{
			URL: "https://api-{app_id}.sendbird.com", IsDefault: true,
			Variables: []serverrouting.Variable{{Name: "app_id", Required: true}},
		}},
	}
}
