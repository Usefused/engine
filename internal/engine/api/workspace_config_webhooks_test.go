package api

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
)

// webhookExecutionPolicyOverrideStore adds the local-override read/write
// methods on top of workspaceTestStore's zero-value store.Store embed, so
// resolveWebhookAuthShape's `s.(executionPolicyOverrideStore)` assertion
// succeeds -- proving the override path is actually exercised, not silently
// skipped the way it is for a plain *workspaceTestStore.
type webhookExecutionPolicyOverrideStore struct {
	workspaceTestStore
	override *store.WorkspaceExecutionPolicyOverride
	err      error
}

func (s *webhookExecutionPolicyOverrideStore) GetEffectiveWorkspaceExecutionPolicyOverride(context.Context, uuid.UUID, uuid.UUID) (*store.WorkspaceExecutionPolicyOverride, error) {
	return s.override, s.err
}

func (s *webhookExecutionPolicyOverrideStore) UpsertWorkspaceExecutionPolicyOverride(context.Context, store.WorkspaceExecutionPolicyOverride) (*store.WorkspaceExecutionPolicyOverride, error) {
	return nil, nil
}

func (s *webhookExecutionPolicyOverrideStore) ResetWorkspaceExecutionPolicyOverride(context.Context, uuid.UUID, *uuid.UUID) error {
	return nil
}

// ─── resolveWebhookAuthShape (shared by kind: webhook's plan/apply) ───────
// These used to be exercised only through the now-removed
// runtime_config.webhooks wrapper (upsertWorkspaceServiceWebhooks -- deleted
// with no backward compatibility, see plans/plan-webhook-kind.md); calling
// resolveWebhookAuthShape directly preserves the same override-precedence
// coverage against the function kind: webhook's handlers actually use now.

// TestResolveWebhookAuthShape_LocalOverrideWinsPerField proves the gap this
// closed: a workspace-local execution_policy override (the kind a non-owner,
// or an owner who hasn't published, can set) now actually changes what real
// inbound webhooks are verified/parsed with -- not just what SDK
// dispatch/RuntimeEnforcer read. Only event_extraction_path is overridden
// here; incoming_webhook_config is left nil on the override to prove the two
// fields win independently, not as an all-or-nothing swap.
func TestResolveWebhookAuthShape_LocalOverrideWinsPerField(t *testing.T) {
	overriddenPath := "body.custom_event_type"
	s := &webhookExecutionPolicyOverrideStore{
		override: &store.WorkspaceExecutionPolicyOverride{EventExtractionPath: &overriddenPath},
	}
	verifier := &mockVerifier{
		serviceMetadata: &fusedobject.ServiceMetadata{
			EventExtractionPath: "event.type",
			IncomingWebhookConfig: &fusedobject.IncomingWebhookConfig{
				AuthType:        "hmac_signature",
				SignatureHeader: "X-Signature",
			},
		},
	}

	authShape, eventExtractionPath, err := resolveWebhookAuthShape(context.Background(), s, verifier, uuid.New(), "2026-07-01", uuid.New())
	if err != nil {
		t.Fatalf("resolveWebhookAuthShape: %v", err)
	}
	if eventExtractionPath != overriddenPath {
		t.Errorf("expected local override to win for event_extraction_path, got %q", eventExtractionPath)
	}
	// IncomingWebhookConfig had no override set, so the Registry-sourced auth
	// shape must still come through untouched.
	if authShape.AuthType != "hmac_signature" || authShape.SignatureHeader != "X-Signature" {
		t.Errorf("expected registry-sourced auth shape to fall through when override leaves it unset, got %#v", authShape)
	}
}

// TestResolveWebhookAuthShape_NoOverrideUsesRegistryValue is the control:
// when GetEffectiveWorkspaceExecutionPolicyOverride returns nil (no override
// exists), the Registry-sourced value must be used unchanged -- same as
// before this override lookup existed.
func TestResolveWebhookAuthShape_NoOverrideUsesRegistryValue(t *testing.T) {
	s := &webhookExecutionPolicyOverrideStore{override: nil}
	verifier := &mockVerifier{
		serviceMetadata: &fusedobject.ServiceMetadata{EventExtractionPath: "event.type"},
	}

	_, eventExtractionPath, err := resolveWebhookAuthShape(context.Background(), s, verifier, uuid.New(), "2026-07-01", uuid.New())
	if err != nil {
		t.Fatalf("resolveWebhookAuthShape: %v", err)
	}
	if eventExtractionPath != "event.type" {
		t.Errorf("expected registry value when no override exists, got %q", eventExtractionPath)
	}
}

// TestResolveWebhookAuthShape_OverrideLookupErrorFallsBackToRegistry proves a
// store error resolving the override soft-fails to the Registry-sourced
// value instead of aborting the whole plan/apply -- the same soft-fail
// discipline cache.go's applyExecutionPolicyOverride uses on the read side.
func TestResolveWebhookAuthShape_OverrideLookupErrorFallsBackToRegistry(t *testing.T) {
	s := &webhookExecutionPolicyOverrideStore{err: errors.New("boom")}
	verifier := &mockVerifier{
		serviceMetadata: &fusedobject.ServiceMetadata{EventExtractionPath: "event.type"},
	}

	_, eventExtractionPath, err := resolveWebhookAuthShape(context.Background(), s, verifier, uuid.New(), "2026-07-01", uuid.New())
	if err != nil {
		t.Fatalf("resolveWebhookAuthShape: %v", err)
	}
	if eventExtractionPath != "event.type" {
		t.Errorf("expected fallback to registry value on override lookup error, got %q", eventExtractionPath)
	}
}

// ─── upsertOneWorkspaceWebhook: secret reference (plan item 4) ────────────
// owningConfigKey is now always a real kind: webhook config_key (never nil
// -- the legacy runtime_config.webhooks caller that used to pass nil was
// removed, see plans/plan-webhook-kind.md), so these pass a placeholder one.

const testWebhookOwningConfigKey = "webhook:test-artifact"

func testWebhookOwningConfigKeyPtr() *string {
	key := testWebhookOwningConfigKey
	return &key
}

// TestUpsertOneWorkspaceWebhook_StoresCanonicalSecretRef proves a valid
// shorthand reference is normalized to its explicit form before being
// persisted, so every stored row resolves the same way at verification time
// regardless of which form the config author wrote.
func TestUpsertOneWorkspaceWebhook_StoresCanonicalSecretRef(t *testing.T) {
	s := &workspaceTestStore{}
	cfg := WebhookConfig{Secret: "${bucket.secret.whsec_a}"}

	saved, err := upsertOneWorkspaceWebhook(context.Background(), s, uuid.New(), uuid.New(), "repo-a", cfg, fusedobject.IncomingWebhookConfig{}, "", testWebhookOwningConfigKeyPtr(), workspaceConnectBucketCache{})
	if err != nil {
		t.Fatalf("upsertOneWorkspaceWebhook: %v", err)
	}
	if saved.SecretRef != "${bucket.default.secret.whsec_a}" {
		t.Fatalf("expected canonical secret ref, got %q", saved.SecretRef)
	}
	if len(s.bucketLookupNames) != 1 || s.bucketLookupNames[0] != "default" {
		t.Fatalf("expected the default bucket to be resolved once, got %#v", s.bucketLookupNames)
	}
}

// TestUpsertOneWorkspaceWebhook_EmptySecretMeansNoSigningSecret proves an
// omitted `secret` field stores an empty SecretRef and never touches the
// bucket store.
func TestUpsertOneWorkspaceWebhook_EmptySecretMeansNoSigningSecret(t *testing.T) {
	s := &workspaceTestStore{}

	saved, err := upsertOneWorkspaceWebhook(context.Background(), s, uuid.New(), uuid.New(), "repo-a", WebhookConfig{}, fusedobject.IncomingWebhookConfig{}, "", testWebhookOwningConfigKeyPtr(), workspaceConnectBucketCache{})
	if err != nil {
		t.Fatalf("upsertOneWorkspaceWebhook: %v", err)
	}
	if saved.SecretRef != "" {
		t.Fatalf("expected empty SecretRef, got %q", saved.SecretRef)
	}
	if len(s.bucketLookupNames) != 0 {
		t.Fatalf("expected no bucket lookup for an unconfigured secret, got %#v", s.bucketLookupNames)
	}
}

// TestUpsertOneWorkspaceWebhook_RejectsMalformedSecretRef proves a value that
// doesn't match the ${bucket.<name>.secret.<key>} grammar fails apply
// immediately instead of being silently stored as an unresolvable reference.
func TestUpsertOneWorkspaceWebhook_RejectsMalformedSecretRef(t *testing.T) {
	s := &workspaceTestStore{}
	cfg := WebhookConfig{Secret: "whsec_a"}

	_, err := upsertOneWorkspaceWebhook(context.Background(), s, uuid.New(), uuid.New(), "repo-a", cfg, fusedobject.IncomingWebhookConfig{}, "", testWebhookOwningConfigKeyPtr(), workspaceConnectBucketCache{})
	if err == nil {
		t.Fatal("expected an error for a malformed secret reference")
	}
	if len(s.upsertedWebhooks) != 0 {
		t.Fatalf("expected no webhook row written when the secret reference is invalid, got %#v", s.upsertedWebhooks)
	}
}

// TestUpsertOneWorkspaceWebhook_RejectsSecretRefWithUnknownBucket proves a
// syntactically valid reference to a bucket that doesn't exist fails apply
// immediately -- a typo'd bucket name should not silently resolve to nothing
// on the first inbound delivery.
func TestUpsertOneWorkspaceWebhook_RejectsSecretRefWithUnknownBucket(t *testing.T) {
	s := &workspaceTestStore{bucketsByName: map[string]*store.Bucket{}}
	cfg := WebhookConfig{Secret: "${bucket.prod.secret.whsec_a}"}

	_, err := upsertOneWorkspaceWebhook(context.Background(), s, uuid.New(), uuid.New(), "repo-a", cfg, fusedobject.IncomingWebhookConfig{}, "", testWebhookOwningConfigKeyPtr(), workspaceConnectBucketCache{})
	if err == nil {
		t.Fatal("expected an error when the referenced bucket does not exist")
	}
	if len(s.upsertedWebhooks) != 0 {
		t.Fatalf("expected no webhook row written when the referenced bucket is missing, got %#v", s.upsertedWebhooks)
	}
}

// TestUpsertOneWorkspaceWebhook_ReusesBucketCacheAcrossLabels proves the
// shared workspaceConnectBucketCache prevents a second lookup for the same
// bucket name (no N+1 queries across labels sharing a bucket).
func TestUpsertOneWorkspaceWebhook_ReusesBucketCacheAcrossLabels(t *testing.T) {
	s := &workspaceTestStore{}
	buckets := workspaceConnectBucketCache{}
	serviceID := uuid.New()

	for _, label := range []string{"repo-a", "repo-b"} {
		cfg := WebhookConfig{Secret: "${bucket.prod.secret.whsec_" + label + "}"}
		if _, err := upsertOneWorkspaceWebhook(context.Background(), s, serviceID, uuid.New(), label, cfg, fusedobject.IncomingWebhookConfig{}, "", testWebhookOwningConfigKeyPtr(), buckets); err != nil {
			t.Fatalf("upsertOneWorkspaceWebhook(%s): %v", label, err)
		}
	}
	if len(s.bucketLookupNames) != 1 {
		t.Fatalf("expected exactly one bucket lookup shared across both labels, got %#v", s.bucketLookupNames)
	}
}

// ─── removeManagedWorkspaceService ─────────────────────────────────────────

func TestRemoveManagedWorkspaceService_PrunesAllWebhooksForRemovedService(t *testing.T) {
	s := &workspaceTestStore{}
	serviceID := uuid.New()

	err := removeManagedWorkspaceService(context.Background(), s, workspaceDesiredState{}, serviceID)
	if err != nil {
		t.Fatalf("removeManagedWorkspaceService: %v", err)
	}

	if len(s.prunedWebhookCalls) != 1 {
		t.Fatalf("expected one prune call for the removed service, got %d", len(s.prunedWebhookCalls))
	}
	if keep := s.prunedWebhookCalls[0]; len(keep) != 0 {
		t.Fatalf("expected an empty keep-set (remove everything) for a fully removed service, got %#v", keep)
	}
}

func TestRemoveManagedWorkspaceService_DeprecatedServiceKeepsItsWebhooks(t *testing.T) {
	s := &workspaceTestStore{}
	serviceID := uuid.New()
	desired := workspaceDesiredState{
		Deprecations: map[uuid.UUID][]workspaceDeprecation{
			serviceID: {{ServiceID: serviceID, EffectiveAt: "2026-08-01"}},
		},
	}

	if err := removeManagedWorkspaceService(context.Background(), s, desired, serviceID); err != nil {
		t.Fatalf("removeManagedWorkspaceService: %v", err)
	}

	// A deprecation directive is not a removal -- the service (and therefore
	// its webhook registrations) stays intact.
	if len(s.prunedWebhookCalls) != 0 {
		t.Fatalf("expected no webhook pruning for a deprecated (not removed) service, got %#v", s.prunedWebhookCalls)
	}
	if len(s.removedWorkspaceServices) != 0 {
		t.Fatalf("expected no workspace service removal for a deprecated service, got %#v", s.removedWorkspaceServices)
	}
}
