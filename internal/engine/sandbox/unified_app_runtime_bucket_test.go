package sandbox

import (
	"context"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
	"testing"
)

type runtimeBucketResolverFixture struct {
	store.Store
	allowed  bool
	fallback uuid.UUID
	checks   int
}

// AuthorizeUnifiedAppRuntimeBucket models permission revocation between physical operations.
func (f *runtimeBucketResolverFixture) AuthorizeUnifiedAppRuntimeBucket(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) {
	f.checks++
	return f.allowed, nil
}

// ResolveAppFamilyServiceBucket returns static routing only when no exact invocation override applies.
func (f *runtimeBucketResolverFixture) ResolveAppFamilyServiceBucket(context.Context, uuid.UUID, uuid.UUID) (*store.AppFamilyBucket, error) {
	return &store.AppFamilyBucket{BucketID: f.fallback}, nil
}

// TestRuntimeBucketIsolationAndRevocation proves concurrent contexts and nested app identity cannot share credentials.
func TestRuntimeBucketIsolationAndRevocation(t *testing.T) {
	f := &runtimeBucketResolverFixture{allowed: true, fallback: uuid.New()}
	app, a, b := uuid.New(), uuid.New(), uuid.New()
	request := CredentialRequest{AppID: app, AccountID: uuid.New(), TokenID: uuid.New()}
	first := WithUnifiedAppBucket(context.Background(), app, &a)
	second := WithUnifiedAppBucket(context.Background(), app, &b)
	for _, tc := range []struct {
		ctx  context.Context
		want uuid.UUID
	}{{first, a}, {second, b}, {context.Background(), f.fallback}} {
		got, err := resolveUnifiedAppExecutionBucket(tc.ctx, f, uuid.New(), request)
		// One invocation cannot overwrite another invocation's or the family's routing.
		if err != nil || got.BucketID != tc.want {
			t.Fatalf("bucket=%v err=%v", got, err)
		}
	}
	child := request
	child.AppID = uuid.New()
	got, err := resolveUnifiedAppExecutionBucket(first, f, uuid.New(), child)
	// A nested app must explicitly select its own credentials and never inherit the parent bucket.
	if err != nil || got.BucketID != f.fallback {
		t.Fatal("parent selection leaked to child")
	}
	f.allowed = false
	if _, err := resolveUnifiedAppExecutionBucket(first, f, uuid.New(), request); err == nil {
		t.Fatal("revoked authority still resolved credentials")
	}
	if f.checks != 3 {
		t.Fatalf("live authorization count=%d", f.checks)
	}
}

type runtimeBucketSecretFixture struct{ *resolverMockStore }

// AuthorizeUnifiedAppRuntimeBucket isolates credential routing from the independently tested SQL policy.
func (f runtimeBucketSecretFixture) AuthorizeUnifiedAppRuntimeBucket(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) {
	return true, nil
}

// TestRuntimeBucketCredentialsNeverFallback exercises actual decryption with competing default and invocation buckets.
func TestRuntimeBucketCredentialsNeverFallback(t *testing.T) {
	app, service, defaultBucket, selected := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	key := []byte("12345678901234567890123456789012")
	wrapped, dek, err := store.WrapDEK(key)
	// The fixture must use real encrypted envelopes so it traverses the production secret resolver.
	if err != nil {
		t.Fatal(err)
	}
	static, err := store.EncryptWithDEK(dek, "default-value")
	if err != nil {
		t.Fatal(err)
	}
	dynamic, err := store.EncryptWithDEK(dek, "selected-value")
	if err != nil {
		t.Fatal(err)
	}
	fixture := runtimeBucketSecretFixture{&resolverMockStore{appRuntime: &store.AppRuntime{BucketID: defaultBucket}, secrets: []store.WorkspaceSecret{
		{WorkspaceSecretMeta: store.WorkspaceSecretMeta{ServiceID: service, BucketID: defaultBucket, KeyName: "API_KEY", CredentialType: "api_key"}, EncryptedDEK: wrapped, EncryptedValue: static},
		{WorkspaceSecretMeta: store.WorkspaceSecretMeta{ServiceID: service, BucketID: selected, KeyName: "API_KEY", CredentialType: "api_key"}, EncryptedDEK: wrapped, EncryptedValue: dynamic},
	}}}
	resolver := NewSecretResolver(fixture, key)
	request := CredentialRequest{AppID: app, ServiceID: service, AuthType: "api_key", Auths: fusedobject.AuthConfigs{{Name: "API_KEY", Type: "apiKey", Location: "header", KeyName: "X-API-Key"}}, Requirements: singleAuthRequirement("API_KEY")}
	ctx := WithUnifiedAppBucket(context.Background(), app, &selected)
	credentials, _, err := resolver.ResolveExecutionCredentials(ctx, request)
	// The configured default's same-named key must not win over the invocation selection.
	if err != nil || credentials["API_KEY"] != "selected-value" {
		t.Fatalf("selected credential mismatch: %v", err)
	}
	fixture.secrets = fixture.secrets[:1]
	_, _, err = resolver.ResolveExecutionCredentials(ctx, request)
	if err == nil {
		t.Fatal("missing selected credentials fell back to the default bucket")
	}
}
