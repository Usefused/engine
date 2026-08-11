package sandbox

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

func singleAuthRequirement(name string) authrouting.Requirements {
	return authrouting.Requirements{{Schemes: []authrouting.Requirement{{Scheme: name}}}}
}

func anonymousAuthRequirement() authrouting.Requirements {
	return authrouting.Requirements{{Schemes: []authrouting.Requirement{}}}
}

func TestSecretResolverResolveExecutionCredentials(t *testing.T) {
	ctx := context.Background()
	appID := uuid.New()
	serviceID := uuid.New()

	masterKey := []byte("12345678901234567890123456789012") // 32 bytes

	// Encrypt dummy values
	wrappedDEK, dek, _ := store.WrapDEK(masterKey)
	encWorkspaceVal, _ := store.EncryptWithDEK(dek, "workspace-val")
	encOtherVal, _ := store.EncryptWithDEK(dek, "other-val")

	mockStore := &resolverMockStore{
		secrets: []store.WorkspaceSecret{
			// Workspace Default
			{
				WorkspaceSecretMeta: store.WorkspaceSecretMeta{
					ID:             uuid.New(),
					ServiceID:      serviceID,
					KeyName:        "API_KEY",
					CredentialType: "string",
				},
				EncryptedDEK:   wrappedDEK,
				EncryptedValue: encWorkspaceVal,
			},
			// Unselected service secret: exact resolution must not read or merge it.
			{
				WorkspaceSecretMeta: store.WorkspaceSecretMeta{
					ID:             uuid.New(),
					ServiceID:      serviceID,
					KeyName:        "OAUTH_TOKEN",
					CredentialType: "string",
				},
				EncryptedDEK:   wrappedDEK,
				EncryptedValue: encWorkspaceVal,
			},
			// Secret for a different service (should be ignored)
			{
				WorkspaceSecretMeta: store.WorkspaceSecretMeta{
					ID:             uuid.New(),
					ServiceID:      uuid.New(),
					KeyName:        "OTHER",
					CredentialType: "string",
				},
				EncryptedDEK:   wrappedDEK,
				EncryptedValue: encOtherVal,
			},
		},
	}

	resolver := NewSecretResolver(mockStore, masterKey)

	// We'll pass a passthrough cred that overrides the API_KEY
	passthrough := map[string]any{
		"API_KEY": "passthrough-val",
	}

	creds, _, err := resolver.ResolveExecutionCredentials(ctx, CredentialRequest{
		AppID: appID, ServiceID: serviceID,
		AuthType: "api_key",
		Auths: fusedobject.AuthConfigs{{
			Name:     "API_KEY",
			Type:     "apiKey",
			Location: "header",
			KeyName:  "X-API-Key",
		}},
		Requirements: singleAuthRequirement("API_KEY"),
		Passthrough:  passthrough,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if creds["API_KEY"] != "passthrough-val" {
		t.Errorf("expected passthrough to override, got %v", creds["API_KEY"])
	}

	if _, exists := creds["OAUTH_TOKEN"]; exists {
		t.Errorf("did not expect unselected secret to be resolved")
	}
	if _, exists := creds["OTHER"]; exists {
		t.Errorf("did not expect OTHER secret to be resolved")
	}
	if mockStore.listSecretsForBucketCalls != 0 {
		t.Fatalf("expected exact secret lookup, got broad list call")
	}
	if got := strings.Join(mockStore.getSecretsKeys, ","); got != "" {
		t.Fatalf("expected passthrough to skip secret lookups, got %q", got)
	}
}

// TestSecretResolverRecordsSanitizedProviderFailure verifies diagnostic writes
// use the Engine-issued connection ID and OTEL trace without changing auth state.
func TestSecretResolverRecordsSanitizedProviderFailure(t *testing.T) {
	connectionID := uuid.New()
	mockStore := &resolverMockStore{}
	resolver := &secretResolver{db: mockStore}
	traceID := trace.TraceID{1, 2, 3, 4}
	spanID := trace.SpanID{5, 6, 7, 8}
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))

	recorded, err := resolver.recordConnectedAuthFailure(ctx, map[string]any{
		"fused_connection_id": connectionID.String(),
		"fused_end_user_ref":  "must-not-be-persisted",
	}, "provider_unauthorized")
	if err != nil || !recorded {
		t.Fatalf("recordConnectedAuthFailure: recorded=%v err=%v", recorded, err)
	}
	if mockStore.failureConnectionID != connectionID || mockStore.failureCode != "provider_unauthorized" || mockStore.failureTraceID != traceID.String() {
		t.Fatalf("unexpected sanitized diagnostic: id=%s code=%q trace=%q", mockStore.failureConnectionID, mockStore.failureCode, mockStore.failureTraceID)
	}
}

// TestProviderAuthFailureCode keeps diagnostics narrow to authorization status
// while ordinary provider failures remain outside the connection lifecycle.
func TestProviderAuthFailureCode(t *testing.T) {
	tests := map[int]string{http.StatusUnauthorized: "provider_unauthorized", http.StatusForbidden: "provider_forbidden", http.StatusBadRequest: ""}
	for status, want := range tests {
		if got := providerAuthFailureCode(status); got != want {
			t.Fatalf("providerAuthFailureCode(%d) = %q, want %q", status, got, want)
		}
	}
}

// ─── GetWebhookSecret: bucket.<name>.secret.<key> reference resolution ─────
// (plans/plan-service-config-restructure.md item 4, task 9) -- these prove
// the real secretResolver, not a mock of it, correctly parses a stored
// SecretRef, resolves the referenced bucket, reads the generic named-secret
// row (service_id = uuid.Nil, "secret:"+key), and decrypts it end to end.

func TestGetWebhookSecret_ResolvesExplicitBucketReference(t *testing.T) {
	ctx := context.Background()
	masterKey := []byte("12345678901234567890123456789012")
	wrappedDEK, dek, _ := store.WrapDEK(masterKey)
	encVal, _ := store.EncryptWithDEK(dek, "whsec_real_secret")

	prodBucketID := uuid.New()
	mockStore := &resolverMockStore{
		bucketsByName: map[string]*store.Bucket{"prod": {ID: prodBucketID, Name: "prod"}},
		secrets: []store.WorkspaceSecret{{
			WorkspaceSecretMeta: store.WorkspaceSecretMeta{
				BucketID:  prodBucketID,
				ServiceID: uuid.Nil,
				KeyName:   "secret:webhook_signing",
			},
			EncryptedDEK:   wrappedDEK,
			EncryptedValue: encVal,
		}},
	}
	resolver := NewSecretResolver(mockStore, masterKey)

	got, err := resolver.GetWebhookSecret(ctx, uuid.New(), prodBucketID, "${bucket.prod.secret.webhook_signing}")
	if err != nil {
		t.Fatalf("GetWebhookSecret: %v", err)
	}
	if got != "whsec_real_secret" {
		t.Fatalf("expected decrypted secret, got %q", got)
	}
}

// TestGetWebhookSecret_ShorthandResolvesDefaultBucket proves the
// bucket-name-omitted "${bucket.secret.<key>}" form resolves against the
// "default" bucket, matching secretref.DefaultBucket.
func TestGetWebhookSecret_ShorthandResolvesDefaultBucket(t *testing.T) {
	ctx := context.Background()
	masterKey := []byte("12345678901234567890123456789012")
	wrappedDEK, dek, _ := store.WrapDEK(masterKey)
	encVal, _ := store.EncryptWithDEK(dek, "whsec_default_bucket")

	defaultBucketID := uuid.New()
	mockStore := &resolverMockStore{
		bucketsByName: map[string]*store.Bucket{"default": {ID: defaultBucketID, Name: "default"}},
		secrets: []store.WorkspaceSecret{{
			WorkspaceSecretMeta: store.WorkspaceSecretMeta{
				BucketID:  defaultBucketID,
				ServiceID: uuid.Nil,
				KeyName:   "secret:webhook_signing",
			},
			EncryptedDEK:   wrappedDEK,
			EncryptedValue: encVal,
		}},
	}
	resolver := NewSecretResolver(mockStore, masterKey)

	got, err := resolver.GetWebhookSecret(ctx, uuid.New(), defaultBucketID, "${bucket.secret.webhook_signing}")
	if err != nil {
		t.Fatalf("GetWebhookSecret: %v", err)
	}
	if got != "whsec_default_bucket" {
		t.Fatalf("expected decrypted secret, got %q", got)
	}
}

// TestGetWebhookSecret_EmptyRefReturnsEmptyWithoutStoreAccess mirrors the old
// empty-SigningSecret behavior: a registration with no secret configured
// verifies with an empty signing secret rather than erroring, and this must
// not cost a workspace-owner check or bucket lookup.
func TestGetWebhookSecret_EmptyRefReturnsEmptyWithoutStoreAccess(t *testing.T) {
	mockStore := &resolverMockStore{verifyWorkspaceOwner: errors.New("must not be called")}
	resolver := NewSecretResolver(mockStore, []byte("12345678901234567890123456789012"))

	got, err := resolver.GetWebhookSecret(context.Background(), uuid.New(), uuid.Nil, "")
	if err != nil {
		t.Fatalf("GetWebhookSecret: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty secret for an unconfigured reference, got %q", got)
	}
}

// TestGetWebhookSecret_RejectsMalformedStoredReference proves a corrupted or
// pre-migration SecretRef value fails closed instead of silently verifying
// with an empty secret.
func TestGetWebhookSecret_RejectsMalformedStoredReference(t *testing.T) {
	mockStore := &resolverMockStore{}
	resolver := NewSecretResolver(mockStore, []byte("12345678901234567890123456789012"))

	_, err := resolver.GetWebhookSecret(context.Background(), uuid.New(), uuid.New(), "not-a-valid-reference")
	if err == nil {
		t.Fatal("expected an error for a malformed stored secret reference")
	}
}

// TestGetWebhookSecret_PropagatesWorkspaceOwnerFailure proves the ownership
// check still gates resolution for a non-empty reference.
func TestGetWebhookSecret_PropagatesWorkspaceOwnerFailure(t *testing.T) {
	mockStore := &resolverMockStore{verifyWorkspaceOwner: errors.New("not the workspace owner")}
	resolver := NewSecretResolver(mockStore, []byte("12345678901234567890123456789012"))

	_, err := resolver.GetWebhookSecret(context.Background(), uuid.New(), uuid.New(), "${bucket.prod.secret.webhook_signing}")
	if err == nil {
		t.Fatal("expected workspace owner verification failure to propagate")
	}
}

// TestGetWebhookSecret_NoStoredSecretReturnsEmpty proves a syntactically
// valid reference to a bucket with no matching secret row returns an empty
// string rather than an error -- the caller (validateWebhookAuth) decides
// what an empty signing secret means for verification.
func TestGetWebhookSecret_NoStoredSecretReturnsEmpty(t *testing.T) {
	bucketID := uuid.New()
	mockStore := &resolverMockStore{bucketsByName: map[string]*store.Bucket{"prod": {ID: bucketID, Name: "prod"}}}
	resolver := NewSecretResolver(mockStore, []byte("12345678901234567890123456789012"))

	got, err := resolver.GetWebhookSecret(context.Background(), uuid.New(), bucketID, "${bucket.prod.secret.never_configured}")
	if err != nil {
		t.Fatalf("GetWebhookSecret: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty string for no matching secret row, got %q", got)
	}
}

// TestGetWebhookSecret_RejectsEnvKindReference proves a reference that
// parses fine but resolves to the bucket's env store (not its secret store)
// fails closed instead of silently reading the wrong table -- a signing
// secret can only ever be a secret-kind reference.
func TestGetWebhookSecret_RejectsEnvKindReference(t *testing.T) {
	mockStore := &resolverMockStore{bucketsByName: map[string]*store.Bucket{"prod": {ID: uuid.New(), Name: "prod"}}}
	resolver := NewSecretResolver(mockStore, []byte("12345678901234567890123456789012"))

	_, err := resolver.GetWebhookSecret(context.Background(), uuid.New(), uuid.New(), "${bucket.prod.env.webhook_signing}")
	if err == nil {
		t.Fatal("expected an error for an env-kind reference on a signing-secret field")
	}
}

// TestGetWebhookSecret_RejectsOldBracketlessFormat proves a pre-migration
// stored reference (the old "bucket.<name>.secret.<key>" grammar with no
// brackets) fails closed rather than silently resolving -- this is exactly
// the shape scripts/migrate_webhook_secret_refs.sql exists to rewrite before
// a binary shipping this parser goes live.
func TestGetWebhookSecret_RejectsOldBracketlessFormat(t *testing.T) {
	mockStore := &resolverMockStore{bucketsByName: map[string]*store.Bucket{"prod": {ID: uuid.New(), Name: "prod"}}}
	resolver := NewSecretResolver(mockStore, []byte("12345678901234567890123456789012"))

	_, err := resolver.GetWebhookSecret(context.Background(), uuid.New(), uuid.New(), "bucket.prod.secret.webhook_signing")
	if err == nil {
		t.Fatal("expected an error for the old bracket-less reference format")
	}
}

func TestGetWebhookSecretDoesNotSubstituteRecreatedBucketName(t *testing.T) {
	masterKey := []byte("12345678901234567890123456789012")
	originalID, replacementID := uuid.New(), uuid.New()
	wrappedOriginal, originalDEK, _ := store.WrapDEK(masterKey)
	originalValue, _ := store.EncryptWithDEK(originalDEK, "original-secret")
	wrappedReplacement, replacementDEK, _ := store.WrapDEK(masterKey)
	replacementValue, _ := store.EncryptWithDEK(replacementDEK, "replacement-secret")
	mockStore := &resolverMockStore{
		// The current name points elsewhere; runtime must ignore it.
		bucketsByName: map[string]*store.Bucket{"prod": {ID: replacementID, Name: "prod"}},
		secrets: []store.WorkspaceSecret{
			{WorkspaceSecretMeta: store.WorkspaceSecretMeta{BucketID: originalID, ServiceID: uuid.Nil, KeyName: "secret:signing"}, EncryptedDEK: wrappedOriginal, EncryptedValue: originalValue},
			{WorkspaceSecretMeta: store.WorkspaceSecretMeta{BucketID: replacementID, ServiceID: uuid.Nil, KeyName: "secret:signing"}, EncryptedDEK: wrappedReplacement, EncryptedValue: replacementValue},
		},
	}
	resolver := NewSecretResolver(mockStore, masterKey)
	got, err := resolver.GetWebhookSecret(context.Background(), uuid.New(), originalID, "${bucket.prod.secret.signing}")
	if err != nil || got != "original-secret" {
		t.Fatalf("immutable bucket lookup = %q, %v", got, err)
	}
}

// TestExtractDynamicKeys_RejectsNamedBucketReference proves an SDK/MCP
// injection value using the kind: webhook named-bucket grammar fails with a
// specific, named error at classification time instead of silently being
// dropped and surfacing later as interpolateValues' generic "missing
// required bucket value" message.
func TestExtractDynamicKeys_RejectsNamedBucketReference(t *testing.T) {
	values := []store.BucketValue{{Value: "Bearer ${bucket.prod.secrets.API_KEY}"}}

	_, _, err := extractDynamicKeys(values)
	if err == nil {
		t.Fatal("expected an error for a named-bucket reference in an injection value")
	}
	if !strings.Contains(err.Error(), "bucket.prod.secrets.API_KEY") {
		t.Fatalf("expected error to name the offending reference, got %v", err)
	}
}

// TestExtractDynamicKeys_AcceptsAmbientForms proves the three ambient forms
// (no bucket name -- always this artifact's own dispatch bucket) still
// classify correctly, including the env/values alias sharing one fetch path.
func TestExtractDynamicKeys_AcceptsAmbientForms(t *testing.T) {
	values := []store.BucketValue{
		{Value: "${bucket.env.A}"},
		{Value: "${bucket.values.B}"},
		{Value: "${bucket.secrets.C}"},
	}

	bucketKeys, secretKeys, err := extractDynamicKeys(values)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bucketKeys) != 2 || len(secretKeys) != 1 {
		t.Fatalf("got bucketKeys=%v secretKeys=%v, want 2 bucket keys and 1 secret key", bucketKeys, secretKeys)
	}
}

func TestSecretResolverResolveExecutionCredentialsUsesTargetedBindings(t *testing.T) {
	bucketID := uuid.New()
	serviceID := uuid.New()
	versionID := uuid.New()
	literal := "eu"
	mockStore := &resolverMockStore{
		appRuntime: &store.AppRuntime{BucketID: bucketID},
		bindings: []store.WorkspaceConnectionBinding{{
			SourceKind: "literal", LiteralValue: &literal,
			TargetLocation: "header", TargetName: "X-Region", Mode: "force",
		}},
	}
	resolver := NewSecretResolver(mockStore, []byte("12345678901234567890123456789012")).(*secretResolver)
	_, values, err := resolver.ResolveExecutionCredentials(context.Background(), CredentialRequest{
		AppID: uuid.New(), ServiceID: serviceID, ServiceVersionID: versionID,
		OperationID: "listAccounts", AuthType: "oauth",
		Requirements: anonymousAuthRequirement(),
	})
	if err != nil {
		t.Fatalf("ResolveExecutionCredentials: %v", err)
	}
	if len(values) != 1 || values[0].Value != literal || values[0].KeyName != "X-Region" {
		t.Fatalf("resolved bindings = %#v", values)
	}
	if mockStore.bindingLookup.ServiceID != serviceID || mockStore.bindingLookup.ServiceVersionID != versionID || mockStore.bindingLookup.AuthType != "oauth" || mockStore.bindingLookup.OperationID != "listAccounts" {
		t.Fatalf("binding lookup was not execution-scoped: %#v", mockStore.bindingLookup)
	}
}

// TestSecretResolverResolveExecutionCredentialsInjectsConnectedAuth covers the
// bucket-linked user credential path without involving dispatcher HTTP calls.
func TestSecretResolverResolveExecutionCredentialsInjectsConnectedAuth(t *testing.T) {
	ctx := context.Background()
	appID := uuid.New()
	bucketID := uuid.New()
	serviceID := uuid.New()
	masterKey := []byte("12345678901234567890123456789012")
	conn := encryptedAuthConnection(t, masterKey, bucketID, serviceID, "user_123", "connected-token", "", time.Time{})
	mockStore := &resolverMockStore{
		appRuntime:       &store.AppRuntime{AppID: appID, BucketID: bucketID},
		authConnection:   &conn,
		authConnectionID: conn.ID,
	}

	resolver := NewSecretResolver(mockStore, masterKey)
	creds, _, err := resolver.ResolveExecutionCredentials(ctx, CredentialRequest{
		AppID: appID, ServiceID: serviceID, AuthType: "oauth",
		Auths:        fusedobject.AuthConfigs{{Name: "bearerAuth", Type: "oauth2"}},
		Requirements: singleAuthRequirement("bearerAuth"),
		Passthrough: map[string]any{
			"fused_end_user_ref": "user_123",
			"fused_auth_name":    "bearerAuth",
		},
	})

	if err != nil {
		t.Fatalf("ResolveExecutionCredentials failed: %v", err)
	}
	if creds["bearerAuth"] != "connected-token" {
		t.Fatalf("expected connected token under auth name, got %#v", creds)
	}
	if got := strings.Join(mockStore.getSecretsKeys, ","); got != "" {
		t.Fatalf("expected connected auth to skip static secret lookups, got %q", got)
	}
	if mockStore.touchedConnectionID != conn.ID {
		t.Fatalf("expected connection last_used_at touch for %s, got %s", conn.ID, mockStore.touchedConnectionID)
	}
}

func TestSecretResolverResolveExecutionCredentialsRejectsBlankOAuthAuthName(t *testing.T) {
	ctx := context.Background()
	appID := uuid.New()
	bucketID := uuid.New()
	serviceID := uuid.New()
	masterKey := []byte("12345678901234567890123456789012")
	conn := encryptedAuthConnection(t, masterKey, bucketID, serviceID, "user_123", "connected-token", "", time.Time{})
	mockStore := &resolverMockStore{
		appRuntime:       &store.AppRuntime{AppID: appID, BucketID: bucketID},
		authConnection:   &conn,
		authConnectionID: conn.ID,
	}

	resolver := NewSecretResolver(mockStore, masterKey)
	creds, _, err := resolver.ResolveExecutionCredentials(ctx, CredentialRequest{
		AppID: appID, ServiceID: serviceID, AuthType: "oauth",
		Auths:        fusedobject.AuthConfigs{{Type: "oauth2"}},
		Requirements: singleAuthRequirement(""),
		Passthrough: map[string]any{
			"fused_end_user_ref": "user_123",
		},
	})

	if err == nil || creds != nil {
		t.Fatalf("unnamed auth must fail closed, creds=%#v err=%v", creds, err)
	}
}

// TestSecretResolverResolveExecutionCredentialsInjectsConnectionResource verifies the
// selected resource URL remains Engine-internal while the SDK uses an opaque ID.
func TestSecretResolverResolveExecutionCredentialsInjectsConnectionResource(t *testing.T) {
	masterKey := []byte("12345678901234567890123456789012")
	bucketID, serviceID, connectionID, resourceID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	wrapper, dek, _ := store.WrapDEK(masterKey)
	access, _ := store.EncryptWithDEK(dek, "access-token")
	mockStore := &resolverMockStore{

		appRuntime: &store.AppRuntime{AppID: uuid.New(), BucketID: bucketID},
		authConnection: &store.AuthConnection{
			ID: connectionID, BucketID: bucketID, ServiceID: serviceID, EndUserRef: "user_123",
			AuthName: "oauth", EncryptedDEK: wrapper, EncryptedAccessToken: access,
		},
		connectionResource: &store.ConnectionResource{
			ID: resourceID, ConnectionID: connectionID, BucketID: bucketID, ServiceID: serviceID,
			ResourceType: "jira_site", BaseURL: "https://api.atlassian.com/ex/jira/cloud-a",
		},
		connectionResourceCount: 2,
	}
	resolver := NewSecretResolver(mockStore, masterKey)
	credentials, _, err := resolver.ResolveExecutionCredentials(context.Background(), CredentialRequest{
		AppID: mockStore.appRuntime.AppID, ServiceID: serviceID, AuthType: "oauth",
		Auths:        fusedobject.AuthConfigs{{Name: "oauth", Type: "oauth2"}},
		Requirements: singleAuthRequirement("oauth"),
		Passthrough: map[string]any{
			"fused_end_user_ref": "user_123", "fused_resource_id": resourceID.String(),
		},
	})
	if err != nil {
		t.Fatalf("ResolveExecutionCredentials: %v", err)
	}
	if credentials["fused_resource_base_url"] != mockStore.connectionResource.BaseURL || credentials["fused_resource_type"] != "jira_site" {
		t.Fatalf("resource routing was not injected: %#v", credentials)
	}
}

// TestSecretResolverResolveExecutionCredentialsRequiresResourceChoice proves multiple
// active resources without a default fail before provider dispatch.
func TestSecretResolverResolveExecutionCredentialsRequiresResourceChoice(t *testing.T) {
	masterKey := []byte("12345678901234567890123456789012")
	bucketID, serviceID := uuid.New(), uuid.New()
	wrapper, dek, _ := store.WrapDEK(masterKey)
	access, _ := store.EncryptWithDEK(dek, "access-token")
	mockStore := &resolverMockStore{

		appRuntime:              &store.AppRuntime{AppID: uuid.New(), BucketID: bucketID},
		authConnection:          &store.AuthConnection{ID: uuid.New(), BucketID: bucketID, ServiceID: serviceID, EndUserRef: "user_123", AuthName: "oauth", EncryptedDEK: wrapper, EncryptedAccessToken: access},
		connectionResourceCount: 2,
	}
	resolver := NewSecretResolver(mockStore, masterKey)
	_, _, err := resolver.ResolveExecutionCredentials(context.Background(), CredentialRequest{
		AppID: mockStore.appRuntime.AppID, ServiceID: serviceID, AuthType: "oauth",
		Auths:        fusedobject.AuthConfigs{{Name: "oauth", Type: "oauth2"}},
		Requirements: singleAuthRequirement("oauth"),
		Passthrough: map[string]any{
			"fused_end_user_ref": "user_123", "fused_resource_required": "true",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "resource_selection_required") {
		t.Fatalf("expected structured selection error, got %v", err)
	}
}

// TestSecretResolverResolveExecutionCredentialsSelectsConnectedAuthType proves SDKs can
// choose OAuth/OIDC by family instead of leaking provider security-scheme names.
func TestSecretResolverResolveExecutionCredentialsSelectsConnectedAuthType(t *testing.T) {
	ctx := context.Background()
	appID := uuid.New()
	bucketID := uuid.New()
	serviceID := uuid.New()
	masterKey := []byte("12345678901234567890123456789012")
	conn := encryptedAuthConnection(t, masterKey, bucketID, serviceID, "user_123", "connected-token", "", time.Time{})
	mockStore := &resolverMockStore{
		appRuntime:       &store.AppRuntime{AppID: appID, BucketID: bucketID},
		authConnection:   &conn,
		authConnectionID: conn.ID,
	}

	resolver := NewSecretResolver(mockStore, masterKey)
	creds, _, err := resolver.ResolveExecutionCredentials(ctx, CredentialRequest{
		AppID: appID, ServiceID: serviceID, AuthType: "oauth",
		Auths: fusedobject.AuthConfigs{{
			Name: "bearerAuth",
			Type: "oauth2",
		}},
		Requirements: singleAuthRequirement("bearerAuth"),
		Passthrough: map[string]any{
			"fused_end_user_ref": "user_123",
			"fused_auth_type":    "oauth",
		},
	})

	if err != nil {
		t.Fatalf("ResolveExecutionCredentials failed: %v", err)
	}
	if creds["bearerAuth"] != "connected-token" {
		t.Fatalf("expected connected token under selected auth name, got %#v", creds)
	}
	if got := strings.Join(mockStore.getSecretsKeys, ","); got != "" {
		t.Fatalf("expected connected auth to skip static secret lookups, got %q", got)
	}
	if mockStore.touchedConnectionID != conn.ID {
		t.Fatalf("expected connection last_used_at touch for %s, got %s", conn.ID, mockStore.touchedConnectionID)
	}
}

// TestSecretResolverResolveExecutionCredentialsSkipsConnectedLookupForBasic keeps
// static bucket auth usable even when app code carries an end-user reference.
func TestSecretResolverResolveExecutionCredentialsSkipsConnectedLookupForBasic(t *testing.T) {
	ctx := context.Background()
	appID := uuid.New()
	bucketID := uuid.New()
	serviceID := uuid.New()
	masterKey := []byte("12345678901234567890123456789012")
	wrappedDEK, dek, err := store.WrapDEK(masterKey)
	if err != nil {
		t.Fatalf("wrap DEK: %v", err)
	}
	username, err := store.EncryptWithDEK(dek, "alice")
	if err != nil {
		t.Fatalf("encrypt username: %v", err)
	}
	password, err := store.EncryptWithDEK(dek, "s3cr3t")
	if err != nil {
		t.Fatalf("encrypt password: %v", err)
	}
	mockStore := &resolverMockStore{
		appRuntime: &store.AppRuntime{AppID: appID, BucketID: bucketID},
		secrets: []store.WorkspaceSecret{
			{
				WorkspaceSecretMeta: store.WorkspaceSecretMeta{
					ID:             uuid.New(),
					ServiceID:      serviceID,
					KeyName:        "basicAuth_username",
					CredentialType: "string",
					BucketID:       bucketID,
				},
				EncryptedDEK:   wrappedDEK,
				EncryptedValue: username,
			},
			{
				WorkspaceSecretMeta: store.WorkspaceSecretMeta{
					ID:             uuid.New(),
					ServiceID:      serviceID,
					KeyName:        "basicAuth_password",
					CredentialType: "string",
					BucketID:       bucketID,
				},
				EncryptedDEK:   wrappedDEK,
				EncryptedValue: password,
			},
		},
	}

	resolver := NewSecretResolver(mockStore, masterKey)
	creds, _, err := resolver.ResolveExecutionCredentials(ctx, CredentialRequest{
		AppID: appID, ServiceID: serviceID, AuthType: "basic",
		Auths: fusedobject.AuthConfigs{{
			Name:              "basicAuth",
			Type:              "http",
			Scheme:            "basic",
			BasicPasswordMode: authrouting.BasicPasswordRequired,
		}},
		Requirements: singleAuthRequirement("basicAuth"),
		Passthrough: map[string]any{
			"fused_end_user_ref": "user_123",
			"fused_auth_type":    "basic",
		},
	})

	if err != nil {
		t.Fatalf("ResolveExecutionCredentials failed: %v", err)
	}
	if creds["basicAuth_username"] != "alice" || creds["basicAuth_password"] != "s3cr3t" {
		t.Fatalf("expected basic credentials from bucket, got %#v", creds)
	}
	if got := strings.Join(mockStore.getSecretsKeys, ","); got != "basicAuth_username,basicAuth_password" {
		t.Fatalf("expected exact basic secret batch lookup, got %q", got)
	}
	if mockStore.getSecretsCalls != 1 {
		t.Fatalf("expected one basic secret batch lookup, got %d", mockStore.getSecretsCalls)
	}
	if mockStore.listSecretsForBucketCalls != 0 {
		t.Fatalf("expected no broad secret list, got %d calls", mockStore.listSecretsForBucketCalls)
	}
	if mockStore.touchedConnectionID != uuid.Nil {
		t.Fatalf("did not expect connected auth lookup/touch, got %s", mockStore.touchedConnectionID)
	}
}

// TestSecretResolverResolveExecutionCredentialsRefreshesExpiringConnectedAuth proves
// dispatch refreshes bucket-owned user tokens before injecting provider auth.
func TestSecretResolverResolveExecutionCredentialsRefreshesExpiringConnectedAuth(t *testing.T) {
	ctx := context.Background()
	appID := uuid.New()
	bucketID := uuid.New()
	serviceID := uuid.New()
	masterKey := []byte("12345678901234567890123456789012")
	conn := encryptedAuthConnection(t, masterKey, bucketID, serviceID, "user_123", "old-access", "old-refresh", time.Now().UTC().Add(time.Minute))
	conn.Scopes = []string{"account:read"}
	conn.ScopeSource = "request"
	failedAt := time.Now().UTC().Add(-time.Minute)
	conn.LastFailureCode, conn.LastFailureAt, conn.LastFailureTraceID = "provider_unauthorized", &failedAt, "old-trace"
	cfg := encryptedResolverConnectConfig(t, masterKey, bucketID, serviceID)
	mockStore := &resolverMockStore{
		appRuntime:     &store.AppRuntime{AppID: appID, BucketID: bucketID},
		authConnection: &conn,
		connectConfig:  &cfg,
	}
	restoreClient := replaceResolverHTTPClient(refreshRoundTripper(t))
	defer restoreClient()

	resolver := NewSecretResolver(mockStore, masterKey)
	creds, _, err := resolver.ResolveExecutionCredentials(ctx, CredentialRequest{
		AppID: appID, ServiceID: serviceID, AuthType: "oauth",
		Auths: fusedobject.AuthConfigs{{
			Name:                    "bearerAuth",
			Type:                    "oauth2",
			TokenURL:                "https://provider.example/token",
			Scopes:                  []string{"account:read", "account:write"},
			TokenEndpointAuthMethod: fusedobject.TokenEndpointAuthMethodClientSecretPost,
		}},
		Requirements: singleAuthRequirement("bearerAuth"),
		Passthrough: map[string]any{
			"fused_end_user_ref": "user_123",
			"fused_auth_name":    "bearerAuth",
		},
	})

	if err != nil {
		t.Fatalf("ResolveExecutionCredentials refresh failed: %v", err)
	}
	if creds["bearerAuth"] != "new-access" {
		t.Fatalf("expected refreshed access token, got %#v", creds)
	}
	if testDecryptAuthConnectionToken(t, masterKey, mockStore.authConnection.EncryptedDEK, mockStore.authConnection.EncryptedRefreshToken) != "new-refresh" {
		t.Fatalf("expected rotated refresh token to be stored")
	}
	if strings.Join(mockStore.authConnection.Scopes, " ") != "account:read" || mockStore.authConnection.ScopeSource != "request" {
		t.Fatalf("expected refresh to preserve requested scopes, got source=%q scopes=%#v", mockStore.authConnection.ScopeSource, mockStore.authConnection.Scopes)
	}
	if mockStore.authConnection.LastFailureCode != "" || mockStore.authConnection.LastFailureAt != nil || mockStore.authConnection.LastFailureTraceID != "" {
		t.Fatalf("successful refresh must clear stale diagnostics: %#v", mockStore.authConnection)
	}
}

// TestSecretResolver_InvalidGrantRequiresReconnect verifies a permanent grant
// failure becomes durable state and a typed SDK-facing execution decision.
func TestSecretResolver_InvalidGrantRequiresReconnect(t *testing.T) {
	ctx := context.Background()
	bucketID, serviceID, appID := uuid.New(), uuid.New(), uuid.New()
	masterKey := []byte("12345678901234567890123456789012")
	conn := encryptedAuthConnection(t, masterKey, bucketID, serviceID, "user_123", "old-access", "old-refresh", time.Now().UTC().Add(time.Minute))
	cfg := encryptedResolverConnectConfig(t, masterKey, bucketID, serviceID)
	mockStore := &resolverMockStore{
		appRuntime:     &store.AppRuntime{AppID: appID, BucketID: bucketID},
		authConnection: &conn, connectConfig: &cfg,
	}
	restoreClient := replaceResolverHTTPClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`)),
		}, nil
	}))
	defer restoreClient()

	resolver := NewSecretResolver(mockStore, masterKey)
	_, _, err := resolver.ResolveExecutionCredentials(ctx, CredentialRequest{
		AccountID: uuid.New(), AppID: appID, ServiceID: serviceID, AuthType: "oauth",
		Auths: fusedobject.AuthConfigs{{
			Name: "bearerAuth", Type: "oauth2", TokenURL: "https://provider.example/token",
			TokenEndpointAuthMethod: fusedobject.TokenEndpointAuthMethodClientSecretPost,
		}},
		Requirements: singleAuthRequirement("bearerAuth"),
		Passthrough:  map[string]any{"fused_end_user_ref": "user_123", "fused_auth_name": "bearerAuth"},
	})
	var reconnectErr *ReconnectRequiredError
	if !errors.As(err, &reconnectErr) {
		t.Fatalf("ResolveExecutionCredentials error = %T %v, want ReconnectRequiredError", err, err)
	}
	if reconnectErr.Reason != "refresh_token_rejected" || reconnectErr.EndUserRef != "user_123" {
		t.Fatalf("unexpected reconnect contract: %#v", reconnectErr)
	}
	if mockStore.authConnection.RefreshState != reconnectRequiredCode {
		t.Fatalf("refresh state = %q, want %q", mockStore.authConnection.RefreshState, reconnectRequiredCode)
	}
	if mockStore.authConnection.LastFailureCode != "refresh_token_rejected" || mockStore.authConnection.LastFailureAt == nil {
		t.Fatalf("reconnect diagnostic was not persisted: %#v", mockStore.authConnection)
	}
}

// TestSecretResolver_ExpiredAccessWithoutRefreshRequiresReconnect covers OAuth
// providers that issue access-only grants where another consent is the repair.
func TestSecretResolver_ExpiredAccessWithoutRefreshRequiresReconnect(t *testing.T) {
	ctx := context.Background()
	bucketID, serviceID, appID := uuid.New(), uuid.New(), uuid.New()
	masterKey := []byte("12345678901234567890123456789012")
	conn := encryptedAuthConnection(t, masterKey, bucketID, serviceID, "user_456", "expired-access", "", time.Now().UTC().Add(-time.Minute))
	mockStore := &resolverMockStore{
		appRuntime: &store.AppRuntime{AppID: appID, BucketID: bucketID}, authConnection: &conn,
	}

	resolver := NewSecretResolver(mockStore, masterKey)
	_, _, err := resolver.ResolveExecutionCredentials(ctx, CredentialRequest{
		AccountID: uuid.New(), AppID: appID, ServiceID: serviceID, AuthType: "oauth",
		Auths:        fusedobject.AuthConfigs{{Name: "bearerAuth", Type: "oauth2", TokenURL: "https://provider.example/token"}},
		Requirements: singleAuthRequirement("bearerAuth"),
		Passthrough:  map[string]any{"fused_end_user_ref": "user_456", "fused_auth_name": "bearerAuth"},
	})
	var reconnectErr *ReconnectRequiredError
	if !errors.As(err, &reconnectErr) || reconnectErr.Reason != "refresh_token_missing" {
		t.Fatalf("expired access error = %T %#v, want refresh_token_missing reconnect", err, err)
	}
	if mockStore.authConnection.RefreshState != reconnectRequiredCode {
		t.Fatalf("refresh state = %q, want %q", mockStore.authConnection.RefreshState, reconnectRequiredCode)
	}
}

type resolverMockStore struct {
	store.Store
	appRuntime                *store.AppRuntime
	secrets                   []store.WorkspaceSecret
	getSecretKeys             []string
	getSecretsKeys            []string
	getSecretsCalls           int
	listSecretsForBucketCalls int
	authConnection            *store.AuthConnection
	authConnectionID          uuid.UUID
	touchedConnectionID       uuid.UUID
	connectConfig             *store.ConnectConfig
	connectionResource        *store.ConnectionResource
	connectionResourceCount   int
	bindings                  []store.WorkspaceConnectionBinding
	bindingLookup             CredentialRequest
	failureConnectionID       uuid.UUID
	failureCode               string
	failureTraceID            string
	// bucketsByName lets webhook-secret-resolution tests pin a bucket to a
	// deterministic ID so a stored WorkspaceSecret's BucketID actually matches
	// what GetBucketByName resolves for the same name -- without this,
	// GetBucketByName's default of minting a fresh random ID per call would
	// never match a secret seeded ahead of time.
	bucketsByName        map[string]*store.Bucket
	verifyWorkspaceOwner error
}

// VerifyWorkspaceOwner defaults to success; set verifyWorkspaceOwner to
// exercise GetWebhookSecret's ownership-check failure path.
func (m *resolverMockStore) VerifyWorkspaceOwner(ctx context.Context, accountID uuid.UUID) error {
	return m.verifyWorkspaceOwner
}

// RecordAuthConnectionFailure captures only the diagnostic arguments so tests
// can prove no end-user reference or provider response is persisted.
func (m *resolverMockStore) RecordAuthConnectionFailure(_ context.Context, id uuid.UUID, code, traceID string, _ time.Time) error {
	m.failureConnectionID = id
	m.failureCode = code
	m.failureTraceID = traceID
	return nil
}

func (m *resolverMockStore) ListWorkspaceBindingsForExecution(_ context.Context, bucketID, serviceID, serviceVersionID uuid.UUID, authType, operationID string) ([]store.WorkspaceConnectionBinding, error) {
	m.bindingLookup = CredentialRequest{ServiceID: serviceID, ServiceVersionID: serviceVersionID, AuthType: authType, OperationID: operationID}
	return m.bindings, nil
}

// GetConnectionResourceForExecution returns the exact/default result prepared
// by each test without broad resource-list behavior.
func (m *resolverMockStore) GetConnectionResourceForExecution(ctx context.Context, connectionID uuid.UUID, resourceID *uuid.UUID) (*store.ConnectionResource, int, error) {
	return m.connectionResource, m.connectionResourceCount, nil
}

func (m *resolverMockStore) GetWorkspaceIDForAccount(ctx context.Context, accountID uuid.UUID) (uuid.UUID, error) {
	return uuid.New(), nil
}

func (m *resolverMockStore) GetAppRuntime(ctx context.Context, appID uuid.UUID) (*store.AppRuntime, error) {
	if m.appRuntime != nil {
		return m.appRuntime, nil
	}
	return &store.AppRuntime{BucketID: uuid.New()}, nil
}

func (m *resolverMockStore) ListSecretsForBucket(ctx context.Context, bucketID, serviceID uuid.UUID) ([]store.WorkspaceSecret, error) {
	m.listSecretsForBucketCalls++
	var filtered []store.WorkspaceSecret
	for _, s := range m.secrets {
		if s.ServiceID == serviceID && s.BucketID == bucketID {
			filtered = append(filtered, s)
		}
	}
	return filtered, nil
}

func (m *resolverMockStore) ListSecretsForBuckets(ctx context.Context, bucketIDs []uuid.UUID, serviceID uuid.UUID) ([]store.WorkspaceSecret, error) {
	var filtered []store.WorkspaceSecret
	bucketSet := make(map[uuid.UUID]bool)
	for _, id := range bucketIDs {
		bucketSet[id] = true
	}
	for _, s := range m.secrets {
		if s.ServiceID == serviceID && bucketSet[s.BucketID] {
			filtered = append(filtered, s)
		}
	}
	return filtered, nil
}

func (m *resolverMockStore) ListBucketValues(ctx context.Context, bucketID uuid.UUID) ([]store.BucketValue, error) {
	return nil, nil
}

func (m *resolverMockStore) GetBucketValues(ctx context.Context, bucketID, serviceID uuid.UUID, keyNames []string) ([]store.BucketValue, error) {
	return nil, nil
}

func (m *resolverMockStore) ListBucketValuesForBuckets(ctx context.Context, bucketIDs []uuid.UUID) ([]store.BucketValue, error) {
	return nil, nil
}

func (m *resolverMockStore) GetSecret(ctx context.Context, bucketID, serviceID uuid.UUID, keyName string) (*store.WorkspaceSecret, error) {
	m.getSecretKeys = append(m.getSecretKeys, keyName)
	for _, s := range m.secrets {
		if s.BucketID == bucketID && s.ServiceID == serviceID && s.KeyName == keyName {
			sec := s
			return &sec, nil
		}
	}
	return nil, nil
}

func (m *resolverMockStore) GetSecrets(ctx context.Context, bucketID, serviceID uuid.UUID, keyNames []string) ([]store.WorkspaceSecret, error) {
	m.getSecretsCalls++
	m.getSecretsKeys = append(m.getSecretsKeys, keyNames...)
	var out []store.WorkspaceSecret
	for _, keyName := range keyNames {
		for _, s := range m.secrets {
			if s.BucketID == bucketID && s.ServiceID == serviceID && s.KeyName == keyName {
				out = append(out, s)
			}
		}
	}
	return out, nil
}

func (m *resolverMockStore) GetFirstCompleteSecretSet(_ context.Context, bucketID, serviceID uuid.UUID, alternatives []store.SecretKeyAlternative) ([]store.WorkspaceSecret, error) {
	m.getSecretsCalls++
	for _, alternative := range alternatives {
		selected := m.availableSecrets(bucketID, serviceID, alternative)
		if containsRequiredSecrets(selected, alternative.Required) {
			for _, key := range alternative.Required {
				m.getSecretsKeys = append(m.getSecretsKeys, key)
			}
			for _, key := range alternative.Optional {
				m.getSecretsKeys = append(m.getSecretsKeys, key)
			}
			return selected, nil
		}
	}
	return nil, nil
}

func containsRequiredSecrets(secrets []store.WorkspaceSecret, required []string) bool {
	for _, key := range required {
		found := false
		for _, secret := range secrets {
			found = found || secret.KeyName == key
		}
		if !found {
			return false
		}
	}
	return true
}

func (m *resolverMockStore) availableSecrets(bucketID, serviceID uuid.UUID, alternative store.SecretKeyAlternative) []store.WorkspaceSecret {
	wanted := append(append([]string{}, alternative.Required...), alternative.Optional...)
	var selected []store.WorkspaceSecret
	for _, key := range wanted {
		for _, secret := range m.secrets {
			if secret.BucketID == bucketID && secret.ServiceID == serviceID && secret.KeyName == key && (secret.ExpiresAt == nil || secret.ExpiresAt.After(time.Now())) {
				selected = append(selected, secret)
			}
		}
	}
	return selected
}

func (m *resolverMockStore) LinkBucketToSDK(ctx context.Context, appID, bucketID uuid.UUID) error {
	return nil
}
func (m *resolverMockStore) ListBucketsForSDK(ctx context.Context, appID uuid.UUID) ([]store.Bucket, error) {
	return []store.Bucket{{ID: appID}}, nil
}

func (m *resolverMockStore) GetBucketByName(ctx context.Context, name string) (*store.Bucket, error) {
	if bucket, ok := m.bucketsByName[name]; ok {
		return bucket, nil
	}
	return &store.Bucket{ID: uuid.New(), Name: name}, nil
}

// GetAuthConnection lets the resolver test prove the lookup is bucket/service
// scoped rather than a broad user-ref search.
func (m *resolverMockStore) GetAuthConnection(ctx context.Context, bucketID, serviceID uuid.UUID, endUserRef, authName string) (*store.AuthConnection, error) {
	if m.authConnection == nil {
		return nil, nil
	}
	if m.authConnection.BucketID == bucketID && m.authConnection.ServiceID == serviceID && m.authConnection.EndUserRef == endUserRef && m.authConnection.AuthName == authName {
		return m.authConnection, nil
	}
	return nil, nil
}

// TouchAuthConnectionLastUsed records audit behavior without needing a real
// database timestamp update.
func (m *resolverMockStore) TouchAuthConnectionLastUsed(ctx context.Context, id uuid.UUID, usedAt time.Time) error {
	m.touchedConnectionID = id
	return nil
}

// GetConnectConfig lets refresh tests prove OAuth app credentials stay bucket
// scoped instead of being supplied by SDK runtime input.
func (m *resolverMockStore) GetConnectConfig(ctx context.Context, bucketID, serviceID uuid.UUID) (*store.ConnectConfig, error) {
	if m.connectConfig == nil {
		return nil, nil
	}
	if m.connectConfig.BucketID == bucketID && m.connectConfig.ServiceID == serviceID {
		return m.connectConfig, nil
	}
	return nil, nil
}

// UpsertAuthConnection updates the mock row by natural key so refresh tests
// exercise the same replace-in-place behavior as the postgres store.
func (m *resolverMockStore) UpsertAuthConnection(ctx context.Context, conn store.AuthConnection) (*store.AuthConnection, error) {
	if conn.ID == uuid.Nil && m.authConnection != nil {
		conn.ID = m.authConnection.ID
	}
	m.authConnection = &conn
	return &conn, nil
}

// encryptedAuthConnection builds realistic encrypted token rows so resolver
// tests exercise decryption instead of trusting plaintext fixtures.
func encryptedAuthConnection(t *testing.T, masterKey []byte, bucketID, serviceID uuid.UUID, endUserRef, token, refreshToken string, expiresAt time.Time) store.AuthConnection {
	t.Helper()
	wrappedDEK, dek, err := store.WrapDEK(masterKey)
	if err != nil {
		t.Fatalf("wrap DEK: %v", err)
	}
	encryptedToken, err := store.EncryptWithDEK(dek, token)
	if err != nil {
		t.Fatalf("encrypt token: %v", err)
	}
	encryptedRefreshToken := ""
	if refreshToken != "" {
		encryptedRefreshToken, err = store.EncryptWithDEK(dek, refreshToken)
		if err != nil {
			t.Fatalf("encrypt refresh token: %v", err)
		}
	}
	var expires *time.Time
	if !expiresAt.IsZero() {
		expires = &expiresAt
	}
	return store.AuthConnection{
		ID:                    uuid.New(),
		BucketID:              bucketID,
		ServiceID:             serviceID,
		EndUserRef:            endUserRef,
		AuthType:              "oauth",
		AuthName:              "bearerAuth",
		EncryptedDEK:          wrappedDEK,
		EncryptedAccessToken:  encryptedToken,
		EncryptedRefreshToken: encryptedRefreshToken,
		TokenType:             "Bearer",
		ExpiresAt:             expires,
		RefreshState:          "ok",
	}
}

// encryptedResolverConnectConfig mirrors apply-time connect config encryption
// so refresh tests decrypt real client credentials.
func encryptedResolverConnectConfig(t *testing.T, masterKey []byte, bucketID, serviceID uuid.UUID) store.ConnectConfig {
	t.Helper()
	wrappedDEK, dek, err := store.WrapDEK(masterKey)
	if err != nil {
		t.Fatalf("wrap connect config DEK: %v", err)
	}
	clientID, err := store.EncryptWithDEK(dek, "client-id")
	if err != nil {
		t.Fatalf("encrypt client id: %v", err)
	}
	clientSecret, err := store.EncryptWithDEK(dek, "client-secret")
	if err != nil {
		t.Fatalf("encrypt client secret: %v", err)
	}
	return store.ConnectConfig{
		BucketID:              bucketID,
		ServiceID:             serviceID,
		AuthType:              "oauth",
		AuthName:              "bearerAuth",
		Enabled:               true,
		EncryptedDEK:          wrappedDEK,
		EncryptedClientID:     clientID,
		EncryptedClientSecret: clientSecret,
		RedirectURI:           "https://engine.example.com/workspace/connect/callback",
	}
}

// refreshRoundTripper validates the refresh grant form before returning new
// provider token material to persist.
func refreshRoundTripper(t *testing.T) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		values, _ := url.ParseQuery(string(raw))
		if values.Get("grant_type") != "refresh_token" || values.Get("refresh_token") != "old-refresh" {
			t.Fatalf("unexpected refresh request form: %#v", values)
		}
		if values.Get("client_id") != "client-id" || values.Get("client_secret") != "client-secret" {
			t.Fatalf("expected connect config client credentials, got %#v", values)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"new-access","refresh_token":"new-refresh","token_type":"Bearer","expires_in":3600}`)),
		}, nil
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip adapts a function into http.RoundTripper so refresh tests can
// inspect provider requests without opening a network listener.
func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

// replaceResolverHTTPClient scopes the global HTTP client swap used by the
// resolver refresh path to one test.
func replaceResolverHTTPClient(transport http.RoundTripper) func() {
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: transport}
	return func() { http.DefaultClient = previous }
}

// testDecryptAuthConnectionToken verifies persisted refresh results using the
// same DEK unwrap/decrypt sequence Engine uses at dispatch.
func testDecryptAuthConnectionToken(t *testing.T, masterKey []byte, encryptedDEK, encryptedValue string) string {
	t.Helper()
	dek, err := store.UnwrapDEK(masterKey, encryptedDEK)
	if err != nil {
		t.Fatalf("unwrap auth connection DEK: %v", err)
	}
	value, err := store.DecryptWithDEK(dek, encryptedValue)
	if err != nil {
		t.Fatalf("decrypt auth connection token: %v", err)
	}
	return value
}
