package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/workspaceplan"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/connectionprofile"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"go.opentelemetry.io/otel"
)

type workspaceCapacityStoreStub struct {
	store.Store
	current      int
	projected    int
	desiredIDs   []uuid.UUID
	removableIDs []uuid.UUID
}

func (s *workspaceCapacityStoreStub) CountProjectedActiveServices(_ context.Context, desiredIDs, removableIDs []uuid.UUID) (int, int, error) {
	s.desiredIDs = append([]uuid.UUID(nil), desiredIDs...)
	s.removableIDs = append([]uuid.UUID(nil), removableIDs...)
	return s.current, s.projected, nil
}

func TestCheckWorkspaceServiceLimitUsesLiveProjectedCount(t *testing.T) {
	original := entitlement.LiveEntitlement.Load()
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{MaxServices: models.IntPtr(5)})
	t.Cleanup(func() { entitlement.LiveEntitlement.Store(original) })

	existingID, replacementID := uuid.New(), uuid.New()
	desired := workspaceDesiredState{Services: map[uuid.UUID]workspaceDesiredService{
		existingID: {ServiceID: existingID}, replacementID: {ServiceID: replacementID},
	}}
	removedID := uuid.New()
	previous := map[uuid.UUID]workspaceManagedService{
		existingID: {ServiceID: existingID.String()},
		removedID:  {ServiceID: removedID.String()},
	}
	capacity := &workspaceCapacityStoreStub{current: 5, projected: 5}
	_, span := otel.Tracer("test").Start(context.Background(), "service-capacity")
	defer span.End()

	if err := checkWorkspaceServiceLimit(context.Background(), span, capacity, desired, previous); err != nil {
		t.Fatalf("replacement at the ceiling should be allowed: %v", err)
	}
	if len(capacity.desiredIDs) != 2 || len(capacity.removableIDs) != 1 {
		t.Fatalf("capacity inputs desired=%v removable=%v", capacity.desiredIDs, capacity.removableIDs)
	}

	capacity.projected = 6
	if err := checkWorkspaceServiceLimit(context.Background(), span, capacity, desired, previous); err == nil || err.Error() != "services limit reached (5/5)" {
		t.Fatalf("net addition at the ceiling should fail with a stable limit error, got %v", err)
	}
}

type requiredPermissionResponse struct {
	Permission   string    `json:"permission"`
	ResourceType string    `json:"resource_type"`
	ResourceID   uuid.UUID `json:"resource_id"`
	DisplayName  string    `json:"display_name"`
}

func TestResolvedInlineWorkspaceProfileUsesApplyMaterial(t *testing.T) {
	profile := connectionprofile.Profile{
		AuthType: "oauth",
		ResourceInput: &connectionprofile.ResourceInputConfig{
			Fields:          []connectionprofile.ResourceInputField{{Name: "shop"}},
			BaseURLTemplate: "https://{shop}.myshopify.com", ResourceType: "shop",
			AllowedHosts: []string{"*.myshopify.com"},
		},
		Bindings: []connectionprofile.Binding{{
			Value: "$SHOPIFY_API_VERSION", Location: "header", Name: "X-Shopify-API-Version", Mode: "force",
		}},
	}
	resolved, err := resolvedInlineWorkspaceProfile(profile, map[string]string{"SHOPIFY_API_VERSION": "2026-07"})
	if err != nil {
		t.Fatalf("resolvedInlineWorkspaceProfile: %v", err)
	}
	if resolved.Bindings[0].Value != "2026-07" || profile.Bindings[0].Value != "$SHOPIFY_API_VERSION" {
		t.Fatalf("resolved=%#v original=%#v", resolved.Bindings, profile.Bindings)
	}
}

// TestPrepareWorkspaceProfileSeparatesDeclarationFromCompiledValues ensures
// sync sees $ENV while runtime binding rows receive only resolved material.
func TestPrepareWorkspaceProfileSeparatesDeclarationFromCompiledValues(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	profile := connectionprofile.Profile{AuthType: "oauth", Bindings: []connectionprofile.Binding{{
		Value: "$SHOPIFY_API_VERSION", Location: "header", Name: "X-Shopify-API-Version", Mode: "force", ProviderExtension: true,
	}}}
	svc := workspaceDesiredService{Key: "shopify", ServiceID: serviceID}
	plan := workspaceProfilePlan{}
	err := prepareWorkspaceProfile(
		svc,
		workspaceConnectMaterial{BindingValues: map[string]string{"SHOPIFY_API_VERSION": "2026-07"}},
		nil,
		workspaceVersionProfile{Version: "v1", VersionID: versionID, AuthType: "oauth", Profile: profile},
		connectionprofile.Contract{AuthConfigs: []connectionprofile.AuthConfig{{Name: "oauth", Type: "oauth"}}, Complete: true}, nil, &plan,
	)
	if err != nil {
		t.Fatalf("prepareWorkspaceProfile: %v", err)
	}
	var snapshot connectionprofile.Profile
	if err := json.Unmarshal(plan.Replacements[0].Profile.ProfileSnapshot, &snapshot); err != nil {
		t.Fatalf("decode profile snapshot: %v", err)
	}
	if snapshot.Bindings[0].Value != "$SHOPIFY_API_VERSION" {
		t.Fatalf("declarative binding was resolved in snapshot: %#v", snapshot.Bindings)
	}
	if plan.Replacements[0].Profile.Layer != "override" {
		t.Fatalf("inline profile should materialize as an override, got layer %q", plan.Replacements[0].Profile.Layer)
	}
	value := plan.Replacements[0].Bindings[0].LiteralValue
	if value == nil || *value != "2026-07" {
		t.Fatalf("compiled binding value = %#v", value)
	}
}

func TestDesiredConnectionProfileActionsNeverExposeLiteralValues(t *testing.T) {
	serviceID := uuid.New()
	versionID := uuid.New()
	actions := desiredConnectionProfileActions(workspaceDesiredService{
		ServiceID: serviceID, Versions: []string{"2026-07-01"}, VersionIDs: map[string]uuid.UUID{"2026-07-01": versionID},
		ConnectionProfiles: []workspaceDesiredConnectionProfile{{
			Version: "2026-07-01", VersionID: versionID, AuthType: "oauth",
			Profile: &connectionprofile.Profile{
				AuthType: "oauth",
				Bindings: []connectionprofile.Binding{
					{Value: "private-literal", Location: "header", Name: "X-Version", Mode: "force"},
					{Value: "$SHOPIFY_VERSION", Location: "header", Name: "X-API-Version", Mode: "force"},
					{Value: "${resource.base_url}", Location: "base_url", Mode: "force"},
				},
			},
		}},
	})
	payload, _ := json.Marshal(actions)
	text := string(payload)
	if strings.Contains(text, "private-literal") {
		t.Fatalf("plan actions exposed a literal value: %s", text)
	}
	if !strings.Contains(text, "$SHOPIFY_VERSION") || !strings.Contains(text, "${resource.base_url}") {
		t.Fatalf("plan actions omitted safe structural sources: %s", text)
	}
}

// TestResolveWorkspaceConnectionProfilesBatchesAndSelectsSoleMatch proves exact names share one batch without mixing same-family streams.
func TestResolveWorkspaceConnectionProfilesBatchesAndSelectsSoleMatch(t *testing.T) {
	versionA := uuid.New()
	versionB := uuid.New()
	profileA := sandbox.ConnectionProfileRevision{
		ProfileID: uuid.New(), ServiceVersionID: versionA, AuthType: "oauth", AuthName: "jiraOAuth", Revision: 2,
		ProfileHash: "hash-a", Provenance: "provider", Config: connectionprofile.Profile{AuthType: "oauth", AuthName: "jiraOAuth"},
	}
	profileB := sandbox.ConnectionProfileRevision{
		ProfileID: uuid.New(), ServiceVersionID: versionB, AuthType: "oidc", Revision: 4,
		ProfileHash: "hash-b", Provenance: "fused", Config: connectionprofile.Profile{AuthType: "oidc"},
	}
	distractor := sandbox.ConnectionProfileRevision{
		ProfileID: uuid.New(), ServiceVersionID: versionA, AuthType: "oauth", AuthName: "adminOAuth", Revision: 1,
		ProfileHash: "other", Provenance: "provider", Config: connectionprofile.Profile{AuthType: "oauth", AuthName: "adminOAuth"},
	}
	resolver := &workspaceProfileResolver{profiles: []sandbox.ConnectionProfileRevision{profileA, distractor, profileB}}
	doc := workspaceConfigDocument{Services: map[string]workspaceConfigService{
		"a": {
			Versions: []workspaceConfigServiceVersion{{
				Version: "v1", ServiceVersionID: versionA.String(),
				ConnectionProfiles: []workspaceConfigConnectionProfileIntent{{AuthType: "oauth", AuthName: "jiraOAuth"}},
			}},
		},
		"b": {
			Versions: []workspaceConfigServiceVersion{{
				Version: "v2", ServiceVersionID: versionB.String(),
				ConnectionProfiles: []workspaceConfigConnectionProfileIntent{{AuthType: "oidc"}},
			}},
		},
	}}
	if err := resolveWorkspaceConnectionProfiles(context.Background(), resolver, "api-key", &doc); err != nil {
		t.Fatalf("resolveWorkspaceConnectionProfiles: %v", err)
	}
	// Both intents must reach Registry in one bounded lookup.
	if resolver.calls != 1 || len(resolver.refs) != 2 {
		t.Fatalf("expected one batched profile lookup, calls=%d refs=%#v", resolver.calls, resolver.refs)
	}
	// The named Jira stream and unnamed legacy stream must remain exact.
	if resolver.refs[0].AuthName != "jiraOAuth" || resolver.refs[1].AuthName != "" {
		t.Fatalf("named and legacy profile refs changed: %#v", resolver.refs)
	}
	resolvedA, resolvedB := doc.Services["a"].Versions[0].ConnectionProfiles[0].Resolved, doc.Services["b"].Versions[0].ConnectionProfiles[0].Resolved
	// The same-family distractor must not make Jira ambiguous or replace its approved profile.
	if resolvedA == nil || resolvedA.ProfileID != profileA.ProfileID.String() || resolvedB == nil || resolvedB.ProfileID != profileB.ProfileID.String() {
		t.Fatalf("sole profiles were not attached: %#v", doc.Services)
	}
}

func TestResolveWorkspaceConnectionProfilesCoversEveryConfiguredVersion(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	profiles := []sandbox.ConnectionProfileRevision{
		{ProfileID: uuid.New(), ServiceVersionID: first, AuthType: "oauth", Revision: 2, ProfileHash: "first", Config: connectionprofile.Profile{AuthType: "oauth"}},
		{ProfileID: uuid.New(), ServiceVersionID: second, AuthType: "oauth", Revision: 3, ProfileHash: "second", Config: connectionprofile.Profile{AuthType: "oauth"}},
	}
	resolver := &workspaceProfileResolver{profiles: profiles}
	doc := workspaceConfigDocument{Services: map[string]workspaceConfigService{
		"multi": {
			Versions: []workspaceConfigServiceVersion{
				{Version: "v1", ServiceVersionID: first.String(), ConnectionProfiles: []workspaceConfigConnectionProfileIntent{{AuthType: "oauth"}}},
				{Version: "v2", ServiceVersionID: second.String(), ConnectionProfiles: []workspaceConfigConnectionProfileIntent{{AuthType: "oauth"}}},
			},
		},
	}}
	if err := resolveWorkspaceConnectionProfiles(context.Background(), resolver, "api-key", &doc); err != nil {
		t.Fatalf("resolveWorkspaceConnectionProfiles: %v", err)
	}
	resolvedCount := 0
	for _, version := range doc.Services["multi"].Versions {
		for _, intent := range version.ConnectionProfiles {
			if intent.Resolved != nil {
				resolvedCount++
			}
		}
	}
	if len(resolver.refs) != 2 || resolvedCount != 2 {
		t.Fatalf("refs=%#v resolvedCount=%d", resolver.refs, resolvedCount)
	}
}

func TestSelectWorkspaceConnectionProfileRequiresExplicitChoiceWhenAmbiguous(t *testing.T) {
	profiles := []sandbox.ConnectionProfileRevision{{ProfileID: uuid.New()}, {ProfileID: uuid.New()}}
	ambiguous, err := selectWorkspaceConnectionProfile("", profiles)
	if err != nil {
		t.Fatalf("automatic ambiguity should not be fatal: %v", err)
	}
	if !ambiguous.Unresolved || ambiguous.Profile != nil || !strings.Contains(ambiguous.Reason, "profile_id") {
		t.Fatalf("ambiguous profiles should require an explicit profile_id: %#v", ambiguous)
	}
	selection, err := selectWorkspaceConnectionProfile(profiles[1].ProfileID.String(), profiles)
	if err != nil {
		t.Fatalf("explicit eligible profile should resolve: %v", err)
	}
	if selection.Unresolved || selection.Profile == nil || selection.Profile.ProfileID != profiles[1].ProfileID {
		t.Fatalf("explicit eligible profile was not selected: %#v", selection)
	}
	if _, err := selectWorkspaceConnectionProfile(uuid.NewString(), profiles); err == nil {
		t.Fatal("ineligible explicit profile_id was accepted")
	}
}

// TestWorkspaceProfileDetachSkipsRegistryResolution ensures an explicit
// removal cannot be overwritten by automatic sole-profile selection.
func TestWorkspaceProfileDetachSkipsRegistryResolution(t *testing.T) {
	versionID := uuid.New()
	services := map[string]workspaceConfigService{
		"jira": {
			Versions: []workspaceConfigServiceVersion{{
				Version: "v1", ServiceVersionID: versionID.String(),
				ConnectionProfiles: []workspaceConfigConnectionProfileIntent{{AuthType: "oauth", Reset: true}},
			}},
		},
	}
	refs, targets, err := workspaceConnectionProfileRequests(services)
	if err != nil {
		t.Fatalf("workspaceConnectionProfileRequests: %v", err)
	}
	if len(refs) != 0 || len(targets) != 0 {
		t.Fatalf("detach requested Registry profiles: refs=%#v targets=%#v", refs, targets)
	}
}

// TestWorkspaceExecutionPolicy_PaginationSurvivesJSONRoundTrip pins the
// wiring plans/plan-service-config-restructure.md item 1 depends on:
// workspaceExecutionPolicy is never field-copied by hand on the way to the
// Registry -- applyWorkspaceExecutionPolicyPublishActions round-trips it
// through the config plan's JSON blob, and the Registry publish client
// marshals it wholesale as the HTTP body. A typo'd json tag on Pagination
// would silently drop it at either hop without ever failing to compile, so
// this pins the tag names directly.
func TestWorkspaceExecutionPolicy_PaginationSurvivesJSONRoundTrip(t *testing.T) {
	timeoutMs := 45000
	initialCursor := ""
	original := workspaceExecutionPolicy{
		Public:    boolPtrEngineTest(true),
		RateLimit: apiRateLimitFixture(10),
		TimeoutMs: &timeoutMs,
		Pagination: &paginationConfig{
			Version:      paginationpolicy.Version,
			Request:      []paginationpolicy.RequestStep{{State: "cursor", Target: paginationpolicy.RequestTarget{Location: "query", Name: "after"}, ValueType: "string", Initial: &paginationpolicy.Scalar{Type: "string", String: &initialCursor}, Apply: "all"}},
			Response:     paginationpolicy.ResponsePlan{Items: paginationpolicy.ItemsSource{Path: "$.items"}, Values: []paginationpolicy.ResponseValue{{Name: "next", Source: paginationpolicy.ValueSource{Location: "body", Path: "$.page.next", ValueType: "string"}}}},
			Continuation: []paginationpolicy.ContinuationStep{{Kind: "token", State: "cursor", ResponseValue: "next"}},
			Termination:  paginationpolicy.Termination{StopOnEmptyItems: true, StopOnMissingValues: []string{"next"}, RepeatedValue: "error"},
			Limits:       paginationpolicy.Limits{MaxPages: 100, MaxItems: 10_000, MaxBytes: 16_777_216, MaxDurationMs: 120_000},
		},
	}

	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"pagination":{"version":3`) || !strings.Contains(string(raw), `"items":{"path":"$.items"}`) {
		t.Fatalf("expected pagination to marshal under the registry's json tags, got %s", raw)
	}

	var roundTripped workspaceExecutionPolicy
	if err := json.Unmarshal(raw, &roundTripped); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if roundTripped.Pagination == nil || !reflect.DeepEqual(roundTripped.Pagination, original.Pagination) {
		t.Fatalf("pagination did not survive round-trip: got %#v, want %#v", roundTripped.Pagination, original.Pagination)
	}
	if roundTripped.TimeoutMs == nil || *roundTripped.TimeoutMs != timeoutMs {
		t.Fatalf("timeout_ms did not survive round-trip: got %v", roundTripped.TimeoutMs)
	}
}

func apiRateLimitFixture(limit int64) *rateLimitConfig {
	return &rateLimitConfig{Version: ratelimitpolicy.Version, Policies: []ratelimitpolicy.Policy{{
		Name: "requests", Mode: ratelimitpolicy.ModeEnforce, Unit: ratelimitpolicy.UnitRequests,
		Identity: ratelimitpolicy.BucketIdentity{Inputs: []ratelimitpolicy.IdentityInput{{Kind: ratelimitpolicy.IdentityServiceVersion}}}, Cost: ratelimitpolicy.CostPlan{Default: 1}, Algorithm: ratelimitpolicy.AlgorithmFixedWindow,
		FixedWindow: &ratelimitpolicy.FixedWindow{Limit: limit, DurationMs: 1_000},
	}}}
}

func TestValidateWorkspaceConfigDocumentRejectsInvalidExecutionTimeout(t *testing.T) {
	invalid := 0
	doc := workspaceConfigDocument{Services: map[string]workspaceConfigService{
		"payments": {ExecutionPolicy: &workspaceExecutionPolicy{TimeoutMs: &invalid}},
	}}

	err := validateWorkspaceConfigDocument(doc)
	if err == nil || !strings.Contains(err.Error(), "timeout_ms") {
		t.Fatalf("validation error = %v, want timeout_ms error", err)
	}
}

func boolPtrEngineTest(b bool) *bool { return &b }

// TestWorkspaceExecutionPolicy_WebhookConfigSurvivesJSONRoundTrip is
// IncomingWebhookConfig's sibling to the pagination round-trip test above:
// this one specifically pins the wire name to "incoming_webhook_config", not
// a "webhook_config" alias, since this same struct value both decodes the
// CLI's config JSON and is re-marshaled verbatim by the Registry publish
// client -- a mismatch here would round-trip fine within the Engine but
// silently fail to reach the Registry.
func TestWorkspaceExecutionPolicy_WebhookConfigSurvivesJSONRoundTrip(t *testing.T) {
	path := "$.data.event"
	original := workspaceExecutionPolicy{
		Public:              boolPtrEngineTest(true),
		EventExtractionPath: &path,
		IncomingWebhookConfig: &webhookVerifyConfig{
			AuthType: "hmac_signature", SignatureHeader: "X-Signature",
			VerificationHeaders: []string{"X-Signature", "X-Timestamp"},
		},
	}

	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"incoming_webhook_config":{`) {
		t.Fatalf("expected wire key to be incoming_webhook_config (matching the Registry's field, not a webhook_config alias), got %s", raw)
	}

	var roundTripped workspaceExecutionPolicy
	if err := json.Unmarshal(raw, &roundTripped); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if roundTripped.IncomingWebhookConfig == nil ||
		roundTripped.IncomingWebhookConfig.AuthType != original.IncomingWebhookConfig.AuthType ||
		roundTripped.IncomingWebhookConfig.SignatureHeader != original.IncomingWebhookConfig.SignatureHeader ||
		len(roundTripped.IncomingWebhookConfig.VerificationHeaders) != len(original.IncomingWebhookConfig.VerificationHeaders) {
		t.Fatalf("webhook config did not survive round-trip: got %#v, want %#v", roundTripped.IncomingWebhookConfig, original.IncomingWebhookConfig)
	}
	if roundTripped.EventExtractionPath == nil || *roundTripped.EventExtractionPath != path {
		t.Fatalf("event_extraction_path did not survive round-trip: got %v", roundTripped.EventExtractionPath)
	}
}

// TestNormalizeWorkspaceBucketSecrets_RequiresEnvRef pins
// plans/plan-service-config-restructure.md item 4's core safety property: a
// literal value in buckets.<name>.secrets.<key> must be rejected at plan time
// (same $ENV-only discipline as Auth/Connect), never stored or forwarded.
func TestNormalizeWorkspaceBucketSecrets_RequiresEnvRef(t *testing.T) {
	buckets := map[string]workspaceConfigBucket{
		"prod": {Secrets: map[string]string{"webhook_signing": "literal-value-not-allowed"}},
	}
	if _, err := normalizeWorkspaceBucketSecrets(buckets); err == nil {
		t.Fatal("expected a literal secret value to be rejected")
	}
}

// TestNormalizeWorkspaceBucketSecrets_AcceptsEnvRefAndDefaultsBucketName
// covers the happy path plus the "default bucket shorthand" from item 4: an
// empty bucket name normalizes to "default", matching
// workspaceConnectBucketName's existing behavior for Auth/Connect.
func TestNormalizeWorkspaceBucketSecrets_AcceptsEnvRefAndDefaultsBucketName(t *testing.T) {
	buckets := map[string]workspaceConfigBucket{
		"": {Secrets: map[string]string{"webhook_signing": "$WEBHOOK_SIGNING_SECRET"}},
	}
	out, err := normalizeWorkspaceBucketSecrets(buckets)
	if err != nil {
		t.Fatalf("normalizeWorkspaceBucketSecrets: %v", err)
	}
	if len(out) != 1 || out[0].BucketName != "default" || out[0].Key != "webhook_signing" || out[0].EnvRef != "$WEBHOOK_SIGNING_SECRET" {
		t.Fatalf("unexpected normalized bucket secret: %#v", out)
	}
}

// workspaceBucketSecretApplyStore is a minimal store.Store double for
// prepareWorkspaceBucketSecrets -- it only needs bucket name resolution and
// records what would have been encrypted/upserted.
type workspaceBucketSecretApplyStore struct {
	store.Store
	buckets map[string]*store.Bucket
}

func (s *workspaceBucketSecretApplyStore) GetBucketByName(_ context.Context, name string) (*store.Bucket, error) {
	b, ok := s.buckets[name]
	if !ok {
		return nil, fmt.Errorf("bucket not found: %s", name)
	}
	return b, nil
}

// TestPrepareWorkspaceBucketSecrets_EncryptsResolvedMaterial confirms the
// out-of-band material resolution pattern (mirroring workspaceAuthMaterial):
// the desired-state intent carries only the $ENV ref, the actual value comes
// from the apply request's BucketSecretMaterials map, and the result is
// keyed with the "secret:" prefix and the uuid.Nil service sentinel so it
// never collides with a service-scoped "webhook_secret:<label>" row.
func TestPrepareWorkspaceBucketSecrets_EncryptsResolvedMaterial(t *testing.T) {
	bucketID := uuid.New()
	s := &workspaceBucketSecretApplyStore{buckets: map[string]*store.Bucket{
		"default": {ID: bucketID, Name: "default"},
	}}
	desired := workspaceDesiredState{
		BucketSecrets: []workspaceDesiredBucketSecret{
			{BucketName: "default", Key: "webhook_signing", EnvRef: "$WEBHOOK_SIGNING_SECRET"},
		},
	}
	materials := map[string]string{
		workspaceBucketSecretMaterialKey("default", "webhook_signing"): "super-secret-value",
	}
	masterKey := []byte("12345678901234567890123456789012")

	secrets, err := prepareWorkspaceBucketSecrets(context.Background(), s, desired, materials, masterKey)
	if err != nil {
		t.Fatalf("prepareWorkspaceBucketSecrets: %v", err)
	}
	if len(secrets) != 1 {
		t.Fatalf("expected one prepared secret, got %d", len(secrets))
	}
	got := secrets[0]
	if got.BucketID != bucketID {
		t.Fatalf("expected bucket ID %s, got %s", bucketID, got.BucketID)
	}
	if got.ServiceID != uuid.Nil {
		t.Fatalf("expected service_id sentinel uuid.Nil for a bucket-scoped secret, got %s", got.ServiceID)
	}
	if got.KeyName != "secret:webhook_signing" {
		t.Fatalf("expected key_name to carry the secret: prefix, got %q", got.KeyName)
	}
	if got.EncryptedValue == "" || got.EncryptedValue == "super-secret-value" {
		t.Fatalf("expected the value to be encrypted, not stored in plaintext")
	}
}

// TestPrepareWorkspaceBucketSecrets_ErrorsWhenMaterialMissing ensures a plan
// whose apply request omits the resolved value for a declared secret fails
// loudly rather than silently upserting an empty/stale secret.
func TestPrepareWorkspaceBucketSecrets_ErrorsWhenMaterialMissing(t *testing.T) {
	s := &workspaceBucketSecretApplyStore{buckets: map[string]*store.Bucket{
		"default": {ID: uuid.New(), Name: "default"},
	}}
	desired := workspaceDesiredState{
		BucketSecrets: []workspaceDesiredBucketSecret{
			{BucketName: "default", Key: "webhook_signing", EnvRef: "$WEBHOOK_SIGNING_SECRET"},
		},
	}
	if _, err := prepareWorkspaceBucketSecrets(context.Background(), s, desired, nil, []byte("12345678901234567890123456789012")); err == nil {
		t.Fatal("expected an error when the apply request has no matching material")
	}
}

// TestVerifyResolvedWorkspaceProfilesRejectsRevisionDrift confirms apply rechecks the approved named stream before rejecting changed content.
func TestVerifyResolvedWorkspaceProfilesRejectsRevisionDrift(t *testing.T) {
	serviceID := uuid.New()
	versionID := uuid.New()
	profileID := uuid.New()
	desired := workspaceDesiredState{Services: map[uuid.UUID]workspaceDesiredService{
		serviceID: {
			Key: "jira", ServiceID: serviceID, Versions: []string{"v1"}, VersionIDs: map[string]uuid.UUID{"v1": versionID},
			ConnectionProfiles: []workspaceDesiredConnectionProfile{{
				Version: "v1", VersionID: versionID, AuthType: "oauth", AuthName: "jiraOAuth",
				Resolved: &workspaceResolvedConnectionProfile{ProfileID: profileID.String(), Revision: 3, ProfileHash: "planned", Provenance: "provider", Config: connectionprofile.Profile{AuthType: "oauth", AuthName: "jiraOAuth"}},
			}},
		},
	}}
	resolver := &workspaceProfileResolver{profiles: []sandbox.ConnectionProfileRevision{{
		ProfileID: profileID, ServiceVersionID: versionID, AuthType: "oauth", AuthName: "jiraOAuth", Revision: 4, ProfileHash: "changed",
	}}}
	// Revision drift remains a hard apply-time failure after exact-name transport.
	if err := verifyResolvedWorkspaceProfiles(context.Background(), resolver, "api-key", desired); err == nil || !strings.Contains(err.Error(), "run plan again") {
		t.Fatalf("revision drift should block apply: %v", err)
	}
	// Verification must query the same named stream approved by plan.
	if len(resolver.refs) != 1 || resolver.refs[0].AuthName != "jiraOAuth" {
		t.Fatalf("apply verification omitted the approved auth_name: %#v", resolver.refs)
	}
}

func TestPrepareWorkspaceProfilesMaterializesEveryVersion(t *testing.T) {
	serviceID := uuid.New()
	first, second := uuid.New(), uuid.New()
	svc := workspaceDesiredService{
		Key: "multi", ServiceID: serviceID, Versions: []string{"v1", "v2"},
		VersionIDs: map[string]uuid.UUID{"v1": first, "v2": second},
		ConnectionProfiles: []workspaceDesiredConnectionProfile{
			{Version: "v1", VersionID: first, AuthType: "oauth", Profile: &connectionprofile.Profile{AuthType: "oauth"}},
			{Version: "v2", VersionID: second, AuthType: "oauth", Profile: &connectionprofile.Profile{AuthType: "oauth"}},
		},
	}
	contracts := map[uuid.UUID]connectionprofile.Contract{
		first: {AuthConfigs: []connectionprofile.AuthConfig{{Name: "oauth", Type: "oauth2"}}, Complete: true}, second: {AuthConfigs: []connectionprofile.AuthConfig{{Name: "oauth", Type: "oauth2"}}, Complete: true},
	}
	var plan workspaceProfilePlan
	if err := prepareWorkspaceServiceProfilePlan(svc, workspaceConnectMaterial{}, nil, contracts, map[string]*store.WorkspaceConnectionProfile{}, &plan); err != nil {
		t.Fatalf("prepareWorkspaceServiceProfilePlan: %v", err)
	}
	if replacements := plan.Replacements; len(replacements) != 2 || replacements[0].Profile.ServiceVersionID != first || replacements[1].Profile.ServiceVersionID != second {
		t.Fatalf("profile replacements = %#v", replacements)
	}
}

// TestWorkspaceApplyPreservesProfileOmittedFromConfig prevents a partial or
// freshly synced workspace file from overwriting an existing workspace profile.
func TestWorkspaceApplyPreservesProfileOmittedFromConfig(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	svc := workspaceDesiredService{
		Key: "jira", ServiceID: serviceID, Versions: []string{"v1"}, VersionIDs: map[string]uuid.UUID{"v1": versionID},
		// Resolved-but-not-explicit mirrors a prior automatic Registry
		// selection (no inline Profile, no explicit ProfileID) -- only an
		// explicit choice may replace a tuple that already has a current profile.
		ConnectionProfiles: []workspaceDesiredConnectionProfile{{
			Version: "v1", VersionID: versionID, AuthType: "oauth",
			Resolved: &workspaceResolvedConnectionProfile{ProfileID: uuid.NewString(), Revision: 2, ProfileHash: "auto", Config: connectionprofile.Profile{AuthType: "oauth"}},
		}},
	}
	current := map[string]*store.WorkspaceConnectionProfile{
		workspaceProfileRefKey(store.WorkspaceProfileRef{ServiceID: serviceID, ServiceVersionID: versionID, AuthType: "oauth"}): {},
	}
	var plan workspaceProfilePlan
	if err := prepareWorkspaceServiceProfilePlan(svc, workspaceConnectMaterial{}, nil, nil, current, &plan); err != nil {
		t.Fatalf("prepareWorkspaceServiceProfilePlan: %v", err)
	}
	if len(plan.Deletes) != 0 || len(plan.Replacements) != 0 {
		t.Fatalf("omission changed profile state: %#v", plan)
	}
	profileStore := &workspaceProfileApplyStore{}
	if err := reconcileWorkspaceProfilePlan(context.Background(), profileStore, plan); err != nil {
		t.Fatalf("reconcileWorkspaceProfilePlan: %v", err)
	}
	if len(profileStore.deleted) != 0 || len(profileStore.replacements) != 0 {
		t.Fatalf("preserved profile was mutated: deletes=%#v replacements=%#v", profileStore.deleted, profileStore.replacements)
	}
}

// profileDetachScenario builds one stable tuple so plan authorization and
// storage reconciliation tests cannot drift onto different service versions.
func profileDetachScenario() (workspaceDesiredState, workspaceDesiredService, *workspaceProfileApplyStore, uuid.UUID) {
	serviceID, versionID := uuid.New(), uuid.New()
	svc := workspaceDesiredService{
		Key: "jira", ServiceID: serviceID, Versions: []string{"v1"}, VersionIDs: map[string]uuid.UUID{"v1": versionID},
		ConnectionProfiles: []workspaceDesiredConnectionProfile{{Version: "v1", VersionID: versionID, AuthType: "oauth", Reset: true}},
	}
	desired := workspaceDesiredState{Services: map[uuid.UUID]workspaceDesiredService{serviceID: svc}}
	profileStore := &workspaceProfileApplyStore{current: []store.WorkspaceConnectionProfile{{
		ServiceID: serviceID, ServiceVersionID: versionID, AuthType: "oauth", Layer: "override",
	}}}
	return desired, svc, profileStore, versionID
}

// TestWorkspaceApplyDetachesProfileOnlyWithExplicitMode proves destructive
// reconciliation requires the canonical detach declaration.
func TestWorkspaceApplyDetachesProfileOnlyWithExplicitMode(t *testing.T) {
	desired, svc, profileStore, _ := profileDetachScenario()
	current := map[string]*store.WorkspaceConnectionProfile{}
	for _, profile := range profileStore.current {
		ref := store.WorkspaceProfileRef{ServiceID: profile.ServiceID, ServiceVersionID: profile.ServiceVersionID, AuthType: profile.AuthType}
		p := profile
		current[workspaceProfileRefKey(ref)] = &p
	}
	var plan workspaceProfilePlan
	if err := prepareWorkspaceServiceProfilePlan(desired.Services[svc.ServiceID], workspaceConnectMaterial{}, nil, nil, current, &plan); err != nil {
		t.Fatalf("prepareWorkspaceServiceProfilePlan: %v", err)
	}
	if len(plan.Deletes) != 1 {
		t.Fatalf("explicit detach did not prepare one delete: %#v", plan.Deletes)
	}
	if err := reconcileWorkspaceProfilePlan(context.Background(), profileStore, plan); err != nil {
		t.Fatalf("reconcileWorkspaceProfilePlan: %v", err)
	}
	if len(profileStore.deleted) != 1 || profileStore.deleted[0].ServiceID != svc.ServiceID {
		t.Fatalf("profile deletes = %#v", profileStore.deleted)
	}
}

// TestWorkspaceProfileDetachRequiresExactPlanTuple rejects missing or altered
// approval even when the action ID still names the same service.
func TestWorkspaceProfileDetachRequiresExactPlanTuple(t *testing.T) {
	desired, svc, _, _ := profileDetachScenario()
	actions := desiredConnectionProfileActions(svc)
	if len(actions) != 1 || actions[0].Type != "detach_connection_profile" {
		t.Fatalf("detach plan action = %#v", actions)
	}
	rawActions, _ := json.Marshal(actions)
	if err := validateWorkspaceProfileResetApproved(desired, rawActions); err != nil {
		t.Fatalf("approved detach was rejected: %v", err)
	}
	tampered := append([]workspacePlanAction(nil), actions...)
	tampered[0].AuthType = "oidc"
	tamperedActions, _ := json.Marshal(tampered)
	if err := validateWorkspaceProfileResetApproved(desired, tamperedActions); err == nil {
		t.Fatal("detach approved for a different auth type was accepted")
	}
	if err := validateWorkspaceProfileResetApproved(desired, nil); err == nil {
		t.Fatal("detach without an approved plan action was accepted")
	}
}

// TestWorkspacePlanSuppressesPreservedAutomaticProfile keeps the reviewed
// action list aligned with apply's implicit-selection preservation rule.
func TestWorkspacePlanSuppressesPreservedAutomaticProfile(t *testing.T) {
	desired, svc, profileStore := automaticProfilePlanScenario(false)
	summary := workspacePlanSummary{Actions: desiredConnectionProfileActions(svc)}
	if err := reconcileWorkspaceProfilePlanActions(context.Background(), profileStore, desired, &summary); err != nil {
		t.Fatalf("reconcileWorkspaceProfilePlanActions: %v", err)
	}
	if len(summary.Actions) != 0 {
		t.Fatalf("preserved automatic profile actions = %#v", summary.Actions)
	}
}

// TestWorkspacePlanKeepsExplicitProfileReplacement ensures a user-selected
// profile remains visible even when a profile already occupies the tuple.
func TestWorkspacePlanKeepsExplicitProfileReplacement(t *testing.T) {
	desired, svc, profileStore := automaticProfilePlanScenario(true)
	summary := workspacePlanSummary{Actions: desiredConnectionProfileActions(svc)}
	if err := reconcileWorkspaceProfilePlanActions(context.Background(), profileStore, desired, &summary); err != nil {
		t.Fatalf("reconcileWorkspaceProfilePlanActions: %v", err)
	}
	if len(summary.Actions) != 2 || summary.Actions[0].Type != "attach_connection_profile" {
		t.Fatalf("explicit replacement actions = %#v", summary.Actions)
	}
}

// automaticProfilePlanScenario creates the same current tuple for implicit and
// explicit cases so only authored replacement intent changes the result.
func automaticProfilePlanScenario(explicit bool) (workspaceDesiredState, workspaceDesiredService, *workspaceProfileApplyStore) {
	serviceID, versionID, profileID := uuid.New(), uuid.New(), uuid.New()
	intent := workspaceDesiredConnectionProfile{
		Version: "v1", VersionID: versionID, AuthType: "oauth",
		Resolved: &workspaceResolvedConnectionProfile{ProfileID: profileID.String(), Revision: 2, ProfileHash: "hash", Config: connectionprofile.Profile{
			AuthType: "oauth", Bindings: []connectionprofile.Binding{{Value: "${resource.base_url}", Location: "base_url", Mode: "force"}},
		}},
	}
	if explicit {
		intent.ProfileID = profileID.String()
	}
	svc := workspaceDesiredService{
		Key: "jira", ServiceID: serviceID, Versions: []string{"v1"}, VersionIDs: map[string]uuid.UUID{"v1": versionID},
		ConnectionProfiles: []workspaceDesiredConnectionProfile{intent},
	}
	profileStore := &workspaceProfileApplyStore{
		current: []store.WorkspaceConnectionProfile{{ServiceID: serviceID, ServiceVersionID: versionID, AuthType: "oauth", Layer: "baseline"}},
	}
	return workspaceDesiredState{Services: map[uuid.UUID]workspaceDesiredService{serviceID: svc}}, svc, profileStore
}

// TestValidateWorkspaceConnectProfileIntentRejectsConflictingDetach keeps
// replacement and deletion mutually exclusive at the backend boundary.
func TestValidateWorkspaceConnectProfileIntentRejectsConflictingDetach(t *testing.T) {
	versionID := uuid.New()
	item := workspaceConfigConnectionProfileIntent{AuthType: "oauth", Reset: true, ProfileID: uuid.NewString()}
	if _, err := normalizeWorkspaceConnectionProfileIntent("jira", "v1", item, map[string]uuid.UUID{"v1": versionID}); err == nil {
		t.Fatal("detach with profile_id was accepted")
	}
}

// TestNormalizeWorkspaceConnectionProfileAuthNamePreservesCompatibility pins outer selection, nested-only inference, and exact mismatch handling.
func TestNormalizeWorkspaceConnectionProfileAuthNamePreservesCompatibility(t *testing.T) {
	versionID := uuid.New()
	versions := map[string]uuid.UUID{"v1": versionID}
	legacy := workspaceConfigConnectionProfileIntent{AuthType: "oauth", Profile: &connectionprofile.Profile{AuthType: "oauth", AuthName: "jiraOAuth"}}
	desired, err := normalizeWorkspaceConnectionProfileIntent("jira", "v1", legacy, versions)
	// Nested-only profiles are the deployed compatibility shape this transport must retain.
	if err != nil || desired.AuthName != "jiraOAuth" {
		t.Fatalf("nested-only auth_name was not inferred: desired=%#v err=%v", desired, err)
	}
	unnamed, err := normalizeWorkspaceConnectionProfileIntent("jira", "v1", workspaceConfigConnectionProfileIntent{AuthType: "oauth", ProfileID: uuid.NewString()}, versions)
	// Empty names continue to identify only the legacy unnamed Registry stream.
	if err != nil || unnamed.AuthName != "" {
		t.Fatalf("legacy unnamed selector changed: desired=%#v err=%v", unnamed, err)
	}
	mismatch := legacy
	mismatch.AuthName = "otherOAuth"
	// Two authored identities cannot safely select and persist different schemes.
	if _, err := normalizeWorkspaceConnectionProfileIntent("jira", "v1", mismatch, versions); err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("conflicting auth names were accepted: %v", err)
	}
	invalid := legacy
	invalid.Profile = &connectionprofile.Profile{AuthType: "oauth", AuthName: strings.Repeat("x", 129)}
	// Inference must not bypass the explicit outer selector's Registry validation contract.
	if _, err := normalizeWorkspaceConnectionProfileIntent("jira", "v1", invalid, versions); err == nil || !strings.Contains(err.Error(), "profile.auth_name is invalid") {
		t.Fatalf("invalid inferred auth name was accepted: %v", err)
	}
}

type workspaceProfileApplyStore struct {
	store.Store
	current      []store.WorkspaceConnectionProfile
	replacements []workspaceProfileReplacement
	deleted      []store.WorkspaceProfileRef
}

// ReconcileWorkspaceProfiles captures the one batch workspace apply sends to
// the concrete transactional store.
func (s *workspaceProfileApplyStore) ReconcileWorkspaceProfiles(_ context.Context, replacements []store.WorkspaceProfileReplacement, deletes []store.WorkspaceProfileRef) error {
	s.replacements = append(s.replacements, replacements...)
	s.deleted = append(s.deleted, deletes...)
	return nil
}

func (s *workspaceProfileApplyStore) UpsertConnectConfig(_ context.Context, config store.ConnectConfig) (*store.ConnectConfig, error) {
	return &config, nil
}

func (s *workspaceProfileApplyStore) GetEffectiveWorkspaceProfiles(context.Context, []store.WorkspaceProfileRef) ([]store.WorkspaceConnectionProfile, error) {
	return append([]store.WorkspaceConnectionProfile(nil), s.current...), nil
}

func (s *workspaceProfileApplyStore) UpsertWorkspaceProfileOverride(_ context.Context, profile store.WorkspaceConnectionProfile, bindings []store.WorkspaceConnectionBinding) (*store.WorkspaceConnectionProfile, error) {
	s.replacements = append(s.replacements, workspaceProfileReplacement{Profile: profile, Bindings: bindings})
	return &profile, nil
}

func (s *workspaceProfileApplyStore) ResetWorkspaceProfile(_ context.Context, serviceID, versionID uuid.UUID, authType string) error {
	s.deleted = append(s.deleted, store.WorkspaceProfileRef{ServiceID: serviceID, ServiceVersionID: versionID, AuthType: authType})
	return nil
}

func (s *workspaceProfileApplyStore) GetEffectiveWorkspaceProfile(context.Context, uuid.UUID, uuid.UUID, string) (*store.WorkspaceConnectionProfile, error) {
	return nil, nil
}

func (s *workspaceProfileApplyStore) ListWorkspaceProfileBindings(context.Context, uuid.UUID, uuid.UUID, string) ([]store.WorkspaceConnectionBinding, error) {
	return nil, nil
}

func (s *workspaceProfileApplyStore) ListWorkspaceBindingsForExecution(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string) ([]store.WorkspaceConnectionBinding, error) {
	return nil, nil
}

func (s *workspaceProfileApplyStore) MarkWorkspaceProfilePublished(context.Context, uuid.UUID, uuid.UUID, string) error {
	return nil
}

type workspaceProfileResolver struct {
	profiles []sandbox.ConnectionProfileRevision
	refs     []sandbox.ConnectionProfileRef
	calls    int
}

func (r *workspaceProfileResolver) FetchEligibleConnectionProfiles(_ context.Context, refs []sandbox.ConnectionProfileRef, _ string) ([]sandbox.ConnectionProfileRevision, error) {
	r.calls++
	r.refs = append([]sandbox.ConnectionProfileRef(nil), refs...)
	return r.profiles, nil
}

// testMasterKey is a fixed 32-byte stand-in for FUSED_ENCRYPTION_KEY, shared
// by every test in this package that needs to call a handler taking a
// masterKey []byte -- matches the convention already used by
// workspace_handlers_test.go's dummyMasterKey.
var testMasterKey = []byte("12345678901234567890123456789012")
var testAppOwnerTeamID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

type mockConfigStore struct {
	state            *store.ConfigState
	states           []store.ConfigState
	plan             *store.ConfigPlan
	err              error
	upsertErr        error
	markErr          error
	markApplied      bool
	createdPlan      *store.CreateConfigPlanParams
	upserted         *store.UpsertConfigStateParams
	notifications    []store.WorkspaceNotification
	createdNotes     []store.CreateWorkspaceNotificationParams
	artifactApply    *store.ApplyAppConfigPlanParams
	artifactApplyErr error
	appRuntimeSink   func(store.AppRuntime) error
	webhookApply     *store.ApplyWebhookConfigPlanParams
	webhookResult    *store.ApplyWebhookConfigPlanResult
	webhookErr       error
	applyLeaseID     uuid.UUID
	renewLeaseErr    error
	renewLeaseCalled chan struct{}
}

func (m *mockConfigStore) GetConfigState(ctx context.Context, configKey string) (*store.ConfigState, error) {
	return m.state, m.err
}

func (m *mockConfigStore) GetConfigStatesByKeys(_ context.Context, configKeys []string) (map[string]store.ConfigState, error) {
	result := make(map[string]store.ConfigState, len(configKeys))
	for _, state := range m.states {
		result[state.ConfigKey] = state
	}
	if m.state != nil {
		result[m.state.ConfigKey] = *m.state
	}
	return result, m.err
}

func (m *mockConfigStore) ListConfigStates(ctx context.Context, configType store.ConfigType) ([]store.ConfigState, error) {
	return m.states, m.err
}

func (m *mockConfigStore) UpsertConfigState(ctx context.Context, params store.UpsertConfigStateParams) (*store.ConfigState, error) {
	m.upserted = &params
	if m.upsertErr != nil {
		return m.state, m.upsertErr
	}
	return m.state, m.err
}

// ApplyConfigPlan mirrors the repository's atomic boundary while preserving
// the focused failure controls used by handler tests.
func (m *mockConfigStore) ApplyConfigPlan(ctx context.Context, params store.ApplyConfigPlanParams) (*store.ConfigState, error) {
	state, err := m.UpsertConfigState(ctx, params.State)
	if err != nil {
		return state, err
	}
	if m.markErr != nil {
		return state, m.markErr
	}
	m.markApplied = true
	return state, nil
}

func (m *mockConfigStore) ApplyAppConfigPlan(ctx context.Context, params store.ApplyAppConfigPlanParams) (*store.ApplyAppConfigPlanResult, error) {
	m.artifactApply = &params
	if m.artifactApplyErr != nil {
		return nil, m.artifactApplyErr
	}
	state, err := m.ApplyConfigPlan(ctx, params.Plan)
	if err != nil {
		return nil, err
	}
	if m.appRuntimeSink != nil {
		if err := m.appRuntimeSink(params.Scope); err != nil {
			return nil, err
		}
	}
	created := m.state == nil || m.state.LatestResourceID == nil
	return &store.ApplyAppConfigPlanResult{
		State: state, AppFamilyID: uuid.New(), AppID: params.Scope.AppID,
		VersionCreated: created, TokenCreated: created,
	}, nil
}

func (m *mockConfigStore) ApplyWebhookConfigPlan(ctx context.Context, params store.ApplyWebhookConfigPlanParams) (*store.ApplyWebhookConfigPlanResult, error) {
	m.webhookApply = &params
	if m.webhookErr != nil {
		return nil, m.webhookErr
	}
	registrations := append([]store.WorkspaceWebhook(nil), params.Registrations...)
	for i := range registrations {
		if registrations[i].ID == uuid.Nil {
			registrations[i].ID = uuid.New()
		}
	}
	m.upserted = &params.Plan.State
	m.markApplied = true
	result := &store.ApplyWebhookConfigPlanResult{State: m.state, Registrations: registrations}
	m.webhookResult = result
	return result, m.err
}

func (m *mockConfigStore) CreateConfigPlan(ctx context.Context, params store.CreateConfigPlanParams) (*store.ConfigPlan, error) {
	m.createdPlan = &params
	if m.plan == nil {
		m.plan = &store.ConfigPlan{
			ID:                  uuid.New(),
			Revision:            1,
			ConfigKey:           params.ConfigKey,
			ConfigType:          params.ConfigType,
			OwnerSubjectID:      params.OwnerSubjectID,
			OwnerTeamID:         params.OwnerTeamID,
			SourceHash:          params.SourceHash,
			BaseGeneration:      params.BaseGeneration,
			Status:              store.ConfigPlanStatusPending,
			Actions:             params.Actions,
			DesiredState:        params.DesiredState,
			ResolvedPayload:     params.ResolvedPayload,
			RequiredPermissions: params.RequiredPermissions,
		}
	}
	if m.plan.OwnerSubjectID == nil && m.plan.OwnerTeamID == nil {
		m.plan.OwnerSubjectID = params.OwnerSubjectID
		m.plan.OwnerTeamID = params.OwnerTeamID
	}
	return m.plan, m.err
}

func (m *mockConfigStore) GetConfigPlan(ctx context.Context, planID uuid.UUID) (*store.ConfigPlan, error) {
	if m.plan != nil && m.plan.Revision == 0 {
		m.plan.Revision = 1
	}
	if m.plan != nil && m.plan.ConfigType != store.ConfigTypeWorkspace {
		if m.plan.OwnerTeamID == nil {
			owner := testAppOwnerTeamID
			m.plan.OwnerTeamID = &owner
		}
		if len(m.plan.RequiredPermissions) == 0 {
			m.plan.RequiredPermissions = json.RawMessage(`[{"permission":"app.read","resource_type":"app","resource_id":"00000000-0000-0000-0000-000000000002"}]`)
		}
	}
	return m.plan, m.err
}

func (m *mockConfigStore) ReplaceConfigPlanActions(ctx context.Context, planID uuid.UUID, actions, requiredPermissions json.RawMessage, actorID uuid.UUID) (*store.ConfigPlan, error) {
	if m.plan != nil {
		m.plan.Actions = actions
		m.plan.RequiredPermissions = requiredPermissions
		m.plan.Revision++
	}
	return m.plan, m.err
}

func (m *mockConfigStore) ReserveConfigPlanApply(_ context.Context, _ uuid.UUID, expectedRevision int) (*store.ConfigPlanApplyLease, error) {
	if m.err != nil {
		return nil, m.err
	}
	if expectedRevision <= 0 {
		return nil, store.ErrConfigPlanRevisionMismatch
	}
	if m.applyLeaseID != uuid.Nil {
		return nil, store.ErrConfigPlanApplyInProgress
	}
	m.applyLeaseID = uuid.New()
	return &store.ConfigPlanApplyLease{ID: m.applyLeaseID, ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (m *mockConfigStore) RenewConfigPlanApply(_ context.Context, _ uuid.UUID, _ int, leaseID uuid.UUID) (*store.ConfigPlanApplyLease, error) {
	if m.renewLeaseCalled != nil {
		select {
		case m.renewLeaseCalled <- struct{}{}:
		default:
		}
	}
	if m.renewLeaseErr != nil {
		return nil, m.renewLeaseErr
	}
	if m.applyLeaseID != leaseID {
		return nil, store.ErrConfigPlanRevisionMismatch
	}
	return &store.ConfigPlanApplyLease{ID: leaseID, ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (m *mockConfigStore) ReleaseConfigPlanApply(_ context.Context, _ uuid.UUID, _ int, leaseID uuid.UUID) error {
	if m.applyLeaseID == leaseID {
		m.applyLeaseID = uuid.Nil
	}
	return nil
}

func TestWorkspaceApplyLeaseContextIgnoresClientCancellationButIsBounded(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	ctx, cancel := workspaceApplyLeaseContextWithTimeout(parent, &mockConfigStore{}, uuid.New(), 1, uuid.New(), 10*time.Millisecond)
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatalf("apply context inherited client cancellation: %v", ctx.Err())
	default:
	}
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("apply context error = %v, want deadline", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("bounded apply context did not reach its deadline")
	}
}

func TestWorkspaceApplyLeaseContextCancelsWhenRenewalIsLost(t *testing.T) {
	called := make(chan struct{}, 1)
	configStore := &mockConfigStore{renewLeaseErr: store.ErrConfigPlanRevisionMismatch, renewLeaseCalled: called}
	ctx, cancel := workspaceApplyLeaseContextWithTiming(context.Background(), configStore, uuid.New(), 3, uuid.New(), time.Second, time.Millisecond, 50*time.Millisecond)
	defer cancel()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("lease renewal was not attempted")
	}
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("apply context error = %v, want cancellation after lease loss", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("apply context remained live after lease renewal failed")
	}
}

func TestWorkspaceConfigErrorMarksCancelledMutationEvidence(t *testing.T) {
	ctx, cancel := context.WithCancel(accesscontrol.ContextWithMutationAuditEvidence(context.Background()))
	cancel()
	response := httptest.NewRecorder()

	writeWorkspaceConfigError(response, context.Canceled, ctx)

	evidence, ok := accesscontrol.MutationAuditEvidenceFromContext(ctx)
	if response.Code != http.StatusInternalServerError || !ok || !evidence.Cancelled {
		t.Fatalf("status/evidence = %d/%#v/%v", response.Code, evidence, ok)
	}
}

func TestWorkspaceApplyKeepsLeaseWhenFinalOutcomeIsUnknown(t *testing.T) {
	planID := uuid.New()
	configStore := &mockConfigStore{
		plan: &store.ConfigPlan{
			ID: planID, ConfigKey: "workspace:unknown", ConfigType: store.ConfigTypeWorkspace,
			SourceHash: "source", Status: store.ConfigPlanStatusPending, Revision: 1,
			Actions: json.RawMessage(`[]`), ResolvedPayload: json.RawMessage(`{"services":{},"buckets":{}}`),
		},
		markErr: errors.New("commit outcome unavailable"),
	}
	_, err := executeWorkspaceConfigApply(context.Background(), configStore, &workspaceTestStore{}, &mockVerifier{}, workspaceApplyCall{
		accountID: uuid.New(), planID: planID, planRevision: 1, sourceHash: "source",
	})
	if err == nil {
		t.Fatal("expected finalization failure")
	}
	if configStore.applyLeaseID == uuid.Nil {
		t.Fatal("ambiguous apply failure released its fencing lease")
	}
}

func (m *mockConfigStore) CreateWorkspaceNotification(ctx context.Context, params store.CreateWorkspaceNotificationParams) (*store.WorkspaceNotification, error) {
	m.createdNotes = append(m.createdNotes, params)
	note := store.WorkspaceNotification{
		ID:        uuid.New(),
		Type:      params.Type,
		Severity:  params.Severity,
		Status:    store.WorkspaceNotificationStatusPending,
		ServiceID: params.ServiceID,
		Version:   params.Version,
		ConfigKey: params.ConfigKey,
		Message:   params.Message,
		Metadata:  params.Metadata,
		CreatedBy: params.CreatedBy,
	}
	m.notifications = append(m.notifications, note)
	return &note, m.err
}

func (m *mockConfigStore) ListWorkspaceNotifications(ctx context.Context, status store.WorkspaceNotificationStatus) ([]store.WorkspaceNotification, error) {
	return m.notifications, m.err
}

// unresolvedMockNotifications mirrors unresolvedWorkspaceNotifications'
// (workspace_config_handlers.go) own dismissed-exclusion rule, kept here as
// a small private helper so the three pagination methods below don't repeat
// the filter three times.
func (m *mockConfigStore) unresolvedMockNotifications() []store.WorkspaceNotification {
	var out []store.WorkspaceNotification
	for _, n := range m.notifications {
		if n.Status != store.WorkspaceNotificationStatusDismissed {
			out = append(out, n)
		}
	}
	return out
}

func (m *mockConfigStore) ListUnresolvedWorkspaceNotificationsPage(ctx context.Context, limit, offset int, pendingOnly bool, resolved bool) ([]store.WorkspaceNotification, error) {
	if m.err != nil {
		return nil, m.err
	}
	var candidates []store.WorkspaceNotification
	if pendingOnly {
		for _, n := range m.notifications {
			if n.Status == store.WorkspaceNotificationStatusPending {
				candidates = append(candidates, n)
			}
		}
	} else {
		candidates = m.unresolvedMockNotifications()
	}
	// Mirror the real store's "pending sorts before acknowledged" ordering
	// (config_repository.go) so tests asserting order match production.
	sort.SliceStable(candidates, func(i, j int) bool {
		iPending := candidates[i].Status == store.WorkspaceNotificationStatusPending
		jPending := candidates[j].Status == store.WorkspaceNotificationStatusPending
		return iPending && !jPending
	})
	if offset >= len(candidates) {
		return nil, nil
	}
	end := offset + limit
	if limit <= 0 || end > len(candidates) {
		end = len(candidates)
	}
	return candidates[offset:end], nil
}

func (m *mockConfigStore) CountUnresolvedWorkspaceNotifications(ctx context.Context) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	return len(m.unresolvedMockNotifications()), nil
}

func (m *mockConfigStore) CountPendingWorkspaceNotifications(ctx context.Context) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	count := 0
	for _, n := range m.notifications {
		if n.Status == store.WorkspaceNotificationStatusPending {
			count++
		}
	}
	return count, nil
}

// UpdateWorkspaceNotificationStatus mirrors postgresConfigRepository's real
// semantics closely enough for handler/resolver tests: validates the target
// status, enforces 'dismissed' as terminal, and mutates m.notifications in
// place so a subsequent ListWorkspaceNotifications/lookup sees the change.
func (m *mockConfigStore) UpdateWorkspaceNotificationStatus(ctx context.Context, id uuid.UUID, status store.WorkspaceNotificationStatus, resolvedBy uuid.UUID) (*store.WorkspaceNotification, error) {
	if status != store.WorkspaceNotificationStatusAcknowledged && status != store.WorkspaceNotificationStatusDismissed {
		return nil, store.ErrWorkspaceNotificationStatusInvalid
	}
	for i := range m.notifications {
		if m.notifications[i].ID != id {
			continue
		}
		if m.notifications[i].Status == store.WorkspaceNotificationStatusDismissed {
			return nil, store.ErrWorkspaceNotificationImmutable
		}
		m.notifications[i].Status = status
		m.notifications[i].ResolvedBy = &resolvedBy
		return &m.notifications[i], nil
	}
	return nil, store.ErrWorkspaceNotificationNotFound
}

func TestWorkspaceConfigPlanHandler(t *testing.T) {
	managedSvcID := uuid.New()
	unmanagedSvcID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		workspaceServices: []store.WorkspaceService{
			{ServiceID: managedSvcID, Version: "2026-07-01"},
			{ServiceID: unmanagedSvcID, Version: "2026-07-01"},
		},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			managedSvcID: {{ServiceID: managedSvcID, Version: "2026-07-01"}},
		},
	}
	managed, _ := json.Marshal(workspaceManagedResources{Services: []workspaceManagedService{{
		ServiceID: managedSvcID.String(),
		Versions:  []string{"2026-07-01", "2026-06-01"},
	}}})
	configStore := &mockConfigStore{state: &store.ConfigState{Generation: 7, ManagedResources: managed}}

	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/config/plan", WorkspaceConfigPlanHandler(configStore, s, &mockRegistryClient{}))

	body := []byte(`{
		"source_hash": "abc",
		"config": {
			"kind": "workspace",
			"services": {
				"managed": {
					"service_id": "` + managedSvcID.String() + `",
					"versions": [{"version": "2026-07-01"}]
				}
			}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/workspace/config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if configStore.createdPlan == nil || configStore.createdPlan.BaseGeneration != 7 {
		t.Fatalf("expected plan with base generation 7, got %#v", configStore.createdPlan)
	}

	summary := decodeWorkspacePlanSummaryResponse(t, rr.Body)
	assertWorkspacePlanPreservesUnmanagedService(t, summary, unmanagedSvcID)
	assertWorkspacePlanDisablesManagedVersion(t, summary, "2026-06-01")
	assertWorkspacePlanUsesBatchedVersionLookup(t, s)
}

func decodeWorkspacePlanSummaryResponse(t *testing.T, body *bytes.Buffer) workspacePlanSummary {
	t.Helper()
	var resp struct {
		Summary             workspacePlanSummary         `json:"summary"`
		RequiredPermissions []requiredPermissionResponse `json:"required_permissions"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp.Summary
}

func assertWorkspacePlanPreservesUnmanagedService(t *testing.T, summary workspacePlanSummary, unmanagedSvcID uuid.UUID) {
	t.Helper()
	if len(summary.UnmanagedServices) != 1 || summary.UnmanagedServices[0] != unmanagedSvcID.String() {
		t.Fatalf("expected unmanaged service %s, got %#v", unmanagedSvcID, summary.UnmanagedServices)
	}
	if hasWorkspaceAction(summary.Actions, "remove_service", unmanagedSvcID.String()) {
		t.Fatalf("unmanaged service must not be planned for removal: %#v", summary.Actions)
	}
}

func assertWorkspacePlanDisablesManagedVersion(t *testing.T, summary workspacePlanSummary, version string) {
	t.Helper()
	if !hasWorkspaceVersionAction(summary.Actions, "disable_service_version", version) {
		t.Fatalf("expected managed removed version action, got %#v", summary.Actions)
	}
}

func assertWorkspacePlanUsesBatchedVersionLookup(t *testing.T, s *workspaceTestStore) {
	t.Helper()
	// Regression guard for the N+1 fix: loadCurrentWorkspaceState must fetch
	// every activated service's allowed versions in one batched call, not one
	// ListWorkspaceServiceVersions call per service.
	if len(s.batchedVersionLookups) != 1 {
		t.Fatalf("expected exactly one batched version lookup, got %d: %#v", len(s.batchedVersionLookups), s.batchedVersionLookups)
	}
	if got, want := len(s.batchedVersionLookups[0]), 2; got != want {
		t.Fatalf("expected the batched lookup to cover both activated services, got %d ids: %#v", got, s.batchedVersionLookups[0])
	}
}

func hasWorkspaceVersionAction(actions []workspacePlanAction, actionType workspaceplan.ActionType, version string) bool {
	for _, action := range actions {
		if action.Type == actionType && action.Version == version {
			return true
		}
	}
	return false
}

func TestWorkspaceConfigPlanHandler_ResolvesServiceSlugsInOneBatch(t *testing.T) {
	oktaID := uuid.New()
	githubID := uuid.New()
	verifier := &mockVerifier{slugIDs: map[string]uuid.UUID{
		"okta":   oktaID,
		"github": githubID,
	}}
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
	}
	configStore := &mockConfigStore{}

	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/config/plan", WorkspaceConfigPlanHandler(configStore, s, verifier))

	body := []byte(`{
		"source_hash": "abc",
		"config": {
			"kind": "workspace",
			"services": {
				"okta": {
					"versions": [{"version": "2026-07-01"}]
				},
				"github": {
					"versions": [{"version": "2026-06-15"}]
				}
			}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/workspace/config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if verifier.resolveCalls != 1 {
		t.Fatalf("expected one slug batch resolution, got %d", verifier.resolveCalls)
	}
	if got := strings.Join(verifier.resolvedSlugs, ","); got != "github,okta" {
		t.Fatalf("expected sorted slug batch, got %q", got)
	}
	var resolved workspaceConfigDocument
	if err := json.Unmarshal(configStore.createdPlan.ResolvedPayload, &resolved); err != nil {
		t.Fatalf("decode resolved payload: %v", err)
	}
	if resolved.Services["okta"].ServiceID != oktaID.String() {
		t.Fatalf("expected okta service_id %s, got %q", oktaID, resolved.Services["okta"].ServiceID)
	}
	if resolved.Services["github"].ServiceID != githubID.String() {
		t.Fatalf("expected github service_id %s, got %q", githubID, resolved.Services["github"].ServiceID)
	}
}

func TestWorkspaceServiceSlugPersistsLocalIdentity(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{name: "bare", key: "github", want: "github"},
		{name: "provider qualified", key: "@acme/github", want: "github"},
		{name: "uuid identity", key: "11111111-1111-4111-8111-111111111111", want: ""},
		{name: "trimmed", key: "  github  ", want: "github"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := workspaceServiceSlug(test.key); got != test.want {
				t.Fatalf("workspaceServiceSlug(%q) = %q, want %q", test.key, got, test.want)
			}
		})
	}
}

func TestWorkspaceConfigPlanHandler_ResolvesOmittedVersionsInOneBatch(t *testing.T) {
	oktaID := uuid.New()
	githubID := uuid.New()
	oktaVersionID := uuid.New()
	githubVersionID := uuid.New()
	verifier := &mockVerifier{latestVersions: map[uuid.UUID]sandbox.ServiceVersionResolvedRef{
		oktaID:   {ServiceID: oktaID, Version: "2026-07-01", ServiceVersionID: oktaVersionID},
		githubID: {ServiceID: githubID, Version: "2026-06-15", ServiceVersionID: githubVersionID},
	}}
	s := &workspaceTestStore{accountID: uuid.New()}
	configStore := &mockConfigStore{}

	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/config/plan", WorkspaceConfigPlanHandler(configStore, s, verifier))

	body := []byte(`{
		"source_hash": "abc",
		"config": {
			"kind": "workspace",
			"services": {
				"okta": {"service_id": "` + oktaID.String() + `"},
				"github": {"service_id": "` + githubID.String() + `"}
			}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/workspace/config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if verifier.verifyCalls != 0 {
		t.Fatalf("expected no per-service VerifyServiceExists calls during plan, got %d", verifier.verifyCalls)
	}
	if len(verifier.latestBatches) != 1 {
		t.Fatalf("expected one latest-version batch, got %d: %#v", len(verifier.latestBatches), verifier.latestBatches)
	}
	gotBatch := sortedUUIDStrings(verifier.latestBatches[0])
	wantBatch := sortedUUIDStrings([]uuid.UUID{oktaID, githubID})
	if strings.Join(gotBatch, ",") != strings.Join(wantBatch, ",") {
		t.Fatalf("expected latest batch %v, got %v", wantBatch, gotBatch)
	}
	var resolved workspaceConfigDocument
	if err := json.Unmarshal(configStore.createdPlan.ResolvedPayload, &resolved); err != nil {
		t.Fatalf("decode resolved payload: %v", err)
	}
	assertResolvedWorkspaceVersion(t, resolved.Services["okta"], "2026-07-01", oktaVersionID)
	assertResolvedWorkspaceVersion(t, resolved.Services["github"], "2026-06-15", githubVersionID)
}

func TestWorkspaceConfigPlanHandler_RejectsPublicForNonOwnedService(t *testing.T) {
	svcID := uuid.New()
	versionID := uuid.New()
	verifier := &mockRegistryClient{
		visibility: map[uuid.UUID]sandbox.ServiceVisibility{
			svcID: {ServiceID: svcID, IsOwner: false, IsPublic: true},
		},
		contractRevisions: map[string]sandbox.ServiceVersionRevision{
			svcID.String() + "|2026-07-01": {ServiceID: svcID, Version: "2026-07-01", ServiceVersionID: versionID, Revision: 1},
		},
	}
	s := &workspaceTestStore{accountID: uuid.New()}
	configStore := &mockConfigStore{}

	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/config/plan", WorkspaceConfigPlanHandler(configStore, s, verifier))

	body := []byte(`{
		"source_hash": "abc",
		"config": {
			"kind": "workspace",
			"services": {
				"@producer/service": {
					"service_id": "` + svcID.String() + `",
					"public": true,
					"versions": [{"version": "2026-07-01"}]
				}
			}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/workspace/config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "public can only be set for services owned by this workspace") {
		t.Fatalf("expected ownership error, got %s", rr.Body.String())
	}
}

func TestWorkspaceConfigPlanHandler_PlansOwnedServicePublicChange(t *testing.T) {
	svcID := uuid.New()
	versionID := uuid.New()
	verifier := &mockRegistryClient{
		visibility: map[uuid.UUID]sandbox.ServiceVisibility{
			svcID: {ServiceID: svcID, IsOwner: true, IsPublic: false},
		},
		contractRevisions: map[string]sandbox.ServiceVersionRevision{
			svcID.String() + "|2026-07-01": {ServiceID: svcID, Version: "2026-07-01", ServiceVersionID: versionID, Revision: 1},
		},
	}
	s := &workspaceTestStore{accountID: uuid.New()}
	configStore := &mockConfigStore{}

	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/config/plan", WorkspaceConfigPlanHandler(configStore, s, verifier))

	body := []byte(`{
		"source_hash": "abc",
		"config": {
			"kind": "workspace",
			"services": {
				"stripe": {
					"service_id": "` + svcID.String() + `",
					"public": true,
					"versions": [{"version": "2026-07-01"}]
				}
			}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/workspace/config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Summary             workspacePlanSummary         `json:"summary"`
		RequiredPermissions []requiredPermissionResponse `json:"required_permissions"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !hasWorkspaceAction(resp.Summary.Actions, "set_service_public", svcID.String()) {
		t.Fatalf("expected set_service_public action, got %#v", resp.Summary.Actions)
	}
	if !hasRequiredPermission(resp.RequiredPermissions, "service.manage", "service", svcID) {
		t.Fatalf("expected service.manage preview, got %#v", resp.RequiredPermissions)
	}
}

func hasRequiredPermission(permissions []requiredPermissionResponse, permission, resourceType string, resourceID uuid.UUID) bool {
	for _, item := range permissions {
		if item.Permission == permission && item.ResourceType == resourceType && item.ResourceID == resourceID {
			return true
		}
	}
	return false
}

func TestWorkspaceConfigPlanHandler_RejectsVersionPublicForNonOwnedService(t *testing.T) {
	svcID := uuid.New()
	versionID := uuid.New()
	verifier := &mockRegistryClient{
		visibility: map[uuid.UUID]sandbox.ServiceVisibility{
			svcID: {ServiceID: svcID, IsOwner: false, IsPublic: true},
		},
		contractRevisions: map[string]sandbox.ServiceVersionRevision{
			svcID.String() + "|2026-07-01": {ServiceID: svcID, Version: "2026-07-01", ServiceVersionID: versionID, Revision: 1},
		},
	}
	s := &workspaceTestStore{accountID: uuid.New()}
	configStore := &mockConfigStore{}

	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/config/plan", WorkspaceConfigPlanHandler(configStore, s, verifier))

	body := []byte(`{
		"source_hash": "abc",
		"config": {
			"kind": "workspace",
			"services": {
				"@producer/service": {
					"service_id": "` + svcID.String() + `",
					"versions": [{"version": "2026-07-01", "public": true}]
				}
			}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/workspace/config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "version 2026-07-01 public can only be set for services owned by this workspace") {
		t.Fatalf("expected version ownership error, got %s", rr.Body.String())
	}
}

func TestWorkspaceConfigPlanHandler_PlansVersionPublicAndExecutionPolicyChange(t *testing.T) {
	svcID := uuid.New()
	versionID := uuid.New()
	verifier := &mockRegistryClient{
		visibility: map[uuid.UUID]sandbox.ServiceVisibility{
			svcID: {ServiceID: svcID, IsOwner: true, IsPublic: true},
		},
		contractRevisions: map[string]sandbox.ServiceVersionRevision{
			svcID.String() + "|2026-07-01": {ServiceID: svcID, Version: "2026-07-01", ServiceVersionID: versionID, Revision: 1, IsPublic: true},
		},
	}
	s := &workspaceTestStore{accountID: uuid.New()}
	configStore := &mockConfigStore{}

	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/config/plan", WorkspaceConfigPlanHandler(configStore, s, verifier))

	body := []byte(`{
		"source_hash": "abc",
		"config": {
			"kind": "workspace",
			"services": {
				"stripe": {
					"service_id": "` + svcID.String() + `",
					"versions": [{
						"version": "2026-07-01",
						"public": false,
						"execution_policy": {"public": true, "rate_limit": {"version":3,"policies":[{"name":"requests","mode":"enforce","unit":"requests","identity":{"inputs":[{"kind":"service_version"}]},"cost":{"default":1,"rules":[]},"algorithm":"fixed_window","fixed_window":{"limit":5,"duration_ms":1000}}]}}
					}]
				}
			}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/workspace/config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Summary workspacePlanSummary `json:"summary"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !hasWorkspaceAction(resp.Summary.Actions, "set_service_version_private", svcID.String()) {
		t.Fatalf("expected set_service_version_private action, got %#v", resp.Summary.Actions)
	}
	if !hasWorkspaceAction(resp.Summary.Actions, "publish_service_version_execution_policy", svcID.String()) {
		t.Fatalf("expected publish_service_version_execution_policy action, got %#v", resp.Summary.Actions)
	}
}

func hasWorkspaceAction(actions []workspacePlanAction, actionType workspaceplan.ActionType, serviceID string) bool {
	for _, action := range actions {
		if action.Type == actionType && action.ServiceID == serviceID {
			return true
		}
	}
	return false
}

func TestWorkspaceConfigPlanHandler_BlocksRemovingServiceUsedBySDK(t *testing.T) {
	svcID := uuid.New()
	appID := uuid.New()
	svcVersionID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		workspaceServices: []store.WorkspaceService{
			{ServiceID: svcID, Version: "2026-07-01"},
		},
		existingScopeHash: "hash",
		existingScope:     []byte(`[{"service_id":"` + svcID.String() + `","service_version_id":"` + svcVersionID.String() + `"}]`),
	}
	managed, _ := json.Marshal(workspaceManagedResources{Services: []workspaceManagedService{{
		ServiceID: svcID.String(),
		Versions:  []string{"2026-07-01"},
	}}})
	configStore := &mockConfigStore{
		state: &store.ConfigState{Generation: 1, ManagedResources: managed},
		states: []store.ConfigState{{
			ConfigKey:        "sdk:security",
			ConfigType:       store.ConfigTypeSDK,
			LatestResourceID: &appID,
		}},
	}

	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/config/plan", WorkspaceConfigPlanHandler(configStore, s, &mockRegistryClient{}))

	body := []byte(`{"source_hash":"abc","config":{"kind":"workspace","services":{}}}`)
	req := httptest.NewRequest(http.MethodPost, "/workspace/config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Summary workspacePlanSummary `json:"summary"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	summary := resp.Summary
	if len(summary.Blockers) != 1 {
		t.Fatalf("expected blocker, got %#v", summary)
	}
	if len(summary.Actions) != 1 || !summary.Actions[0].RequiresDecision {
		t.Fatalf("expected decision-required remove action, got %#v", summary.Actions)
	}
	if got := summary.Actions[0].ImpactedSDKConfigs; len(got) != 1 || got[0] != "sdk:security" {
		t.Fatalf("expected impacted SDK config, got %#v", got)
	}
}

func TestWorkspaceConfigPlanHandler_ReadsAppRuntimesInOneBatch(t *testing.T) {
	svcID := uuid.New()
	firstAppID := uuid.New()
	secondAppID := uuid.New()
	svcVersionID := uuid.New()
	selection := []byte(`[{"service_id":"` + svcID.String() + `","service_version_id":"` + svcVersionID.String() + `"}]`)
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		workspaceServices: []store.WorkspaceService{
			{ServiceID: svcID, Version: "2026-07-01"},
		},
		mockScopes: map[uuid.UUID]*store.AppRuntime{
			firstAppID:  {AppID: firstAppID, Selections: selection},
			secondAppID: {AppID: secondAppID, Selections: selection},
		},
	}
	managed, _ := json.Marshal(workspaceManagedResources{Services: []workspaceManagedService{{
		ServiceID: svcID.String(),
		Versions:  []string{"2026-07-01"},
	}}})
	configStore := &mockConfigStore{
		state: &store.ConfigState{Generation: 1, ManagedResources: managed},
		states: []store.ConfigState{
			{ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, LatestResourceID: &firstAppID},
			{ConfigKey: "sdk:platform", ConfigType: store.ConfigTypeSDK, LatestResourceID: &secondAppID},
		},
	}

	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/config/plan", WorkspaceConfigPlanHandler(configStore, s, &mockRegistryClient{}))
	body := []byte(`{"source_hash":"abc","config":{"kind":"workspace","services":{}}}`)
	req := httptest.NewRequest(http.MethodPost, "/workspace/config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(s.batchedScopeLookups) != 1 {
		t.Fatalf("expected one batched scope lookup, got %#v", s.batchedScopeLookups)
	}
	if len(s.batchedScopeLookups[0]) != 2 {
		t.Fatalf("expected both SDK ids in batch, got %#v", s.batchedScopeLookups[0])
	}
	if len(s.scopeLookups) != 0 {
		t.Fatalf("expected no individual scope lookups, got %#v", s.scopeLookups)
	}
}

func TestWorkspaceConfigPlanHandler_FailsClosedOnScopeBatchError(t *testing.T) {
	svcID := uuid.New()
	appID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		workspaceServices: []store.WorkspaceService{
			{ServiceID: svcID, Version: "2026-07-01"},
		},
		scopeBatchErr: errors.New("scope database unavailable"),
	}
	managed, _ := json.Marshal(workspaceManagedResources{Services: []workspaceManagedService{{
		ServiceID: svcID.String(),
		Versions:  []string{"2026-07-01"},
	}}})
	configStore := &mockConfigStore{
		state:  &store.ConfigState{Generation: 1, ManagedResources: managed},
		states: []store.ConfigState{{ConfigKey: "sdk:security", ConfigType: store.ConfigTypeSDK, LatestResourceID: &appID}},
	}

	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/config/plan", WorkspaceConfigPlanHandler(configStore, s, &mockRegistryClient{}))
	req := httptest.NewRequest(http.MethodPost, "/workspace/config/plan", bytes.NewReader([]byte(`{"source_hash":"abc","config":{"kind":"workspace","services":{}}}`)))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected fail-closed 500, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestWorkspaceConfigPlanHandler_DeprecationDirectiveKeepsImpactedService(t *testing.T) {
	svcID := uuid.New()
	appID := uuid.New()
	svcVersionID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		workspaceServices: []store.WorkspaceService{
			{ServiceID: svcID, Version: "2026-07-01"},
		},
		existingScopeHash: "hash",
		existingScope:     []byte(`[{"service_id":"` + svcID.String() + `","service_version_id":"` + svcVersionID.String() + `"}]`),
	}
	managed, _ := json.Marshal(workspaceManagedResources{Services: []workspaceManagedService{{
		ServiceID: svcID.String(),
		Versions:  []string{"2026-07-01"},
	}}})
	configStore := &mockConfigStore{
		state: &store.ConfigState{Generation: 1, ManagedResources: managed},
		states: []store.ConfigState{{
			ConfigKey:        "sdk:security",
			ConfigType:       store.ConfigTypeSDK,
			LatestResourceID: &appID,
		}},
	}

	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/config/plan", WorkspaceConfigPlanHandler(configStore, s, &mockRegistryClient{}))

	body := []byte(`{
		"source_hash":"abc",
		"config":{
			"kind":"workspace",
			"services":{},
			"deprecations":[{"service_id":"` + svcID.String() + `","effective_at":"2026-09-01","reason":"security team migration"}]
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/workspace/config/plan", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Summary workspacePlanSummary `json:"summary"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Summary.Blockers) != 0 {
		t.Fatalf("expected deprecation plan without blockers, got %#v", resp.Summary.Blockers)
	}
	if len(resp.Summary.Actions) != 1 || resp.Summary.Actions[0].Type != "deprecate_service" {
		t.Fatalf("expected deprecate_service action, got %#v", resp.Summary.Actions)
	}
	action := resp.Summary.Actions[0]
	if action.EffectiveAt != "2026-09-01" || action.SuggestedCommand == "" {
		t.Fatalf("expected dated deprecation command, got %#v", action)
	}
}

func sortedUUIDStrings(ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	sort.Strings(out)
	return out
}

func assertResolvedWorkspaceVersion(t *testing.T, svc workspaceConfigService, version string, versionID uuid.UUID) {
	t.Helper()
	if len(svc.Versions) != 1 || svc.Versions[0].Version != version {
		t.Fatalf("expected version %s, got %#v", version, svc.Versions)
	}
	if svc.Versions[0].ServiceVersionID != versionID.String() {
		t.Fatalf("expected resolved version ID %s, got %#v", versionID, svc.Versions)
	}
}

func TestWorkspaceConfigApplyHandler(t *testing.T) {
	svcID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
	}

	planID := uuid.New()
	payload := json.RawMessage(`{
		"kind": "workspace",
		"services": {
			"svc": {
				"service_id": "` + svcID.String() + `",
				"versions": [{"version":"2026-08-01","service_version_id":"` + uuid.NewString() + `"}]
			}
		}
	}`)
	managed, _ := json.Marshal(workspaceManagedResources{Services: []workspaceManagedService{{
		ServiceID: svcID.String(),
		Versions:  []string{"2026-07-01", "2026-08-01"},
	}}})
	configStore := &mockConfigStore{
		state: &store.ConfigState{Generation: 3, ManagedResources: managed},
		plan: &store.ConfigPlan{
			ID:              planID,
			Status:          store.ConfigPlanStatusPending,
			ConfigKey:       "workspace",
			ConfigType:      store.ConfigTypeWorkspace,
			SourceHash:      "abc",
			BaseGeneration:  3,
			ResolvedPayload: payload,
		},
	}

	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/config/apply", WorkspaceConfigApplyHandler(configStore, s, &mockRegistryClient{name: "test"}, testMasterKey))

	body := []byte(`{"plan_id": "` + planID.String() + `", "source_hash": "abc"}`)
	req := httptest.NewRequest(http.MethodPost, "/workspace/config/apply", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.gotVersion != "2026-08-01" {
		t.Fatalf("expected latest public version to be activated, got %q", s.gotVersion)
	}
	if len(s.removedVersions) != 1 || s.removedVersions[0] != svcID.String()+":2026-07-01" {
		t.Fatalf("expected old managed version removal, got %#v", s.removedVersions)
	}
	if configStore.upserted == nil || string(configStore.upserted.ManagedResources) == "" {
		t.Fatalf("expected applied managed resources to be stored, got %#v", configStore.upserted)
	}
}

func TestLoadWorkspacePlanForApplyRejectsRevisionDifferentFromAuthorizationSnapshot(t *testing.T) {
	plan := &store.ConfigPlan{
		ID: uuid.New(), ConfigKey: "workspace", ConfigType: store.ConfigTypeWorkspace,
		Status: store.ConfigPlanStatusPending, SourceHash: "abc", Revision: 2,
	}
	configStore := &mockConfigStore{plan: plan}
	_, _, err := loadWorkspacePlanForApply(context.Background(), configStore, workspaceApplyCall{
		planID: plan.ID, planRevision: 1, sourceHash: plan.SourceHash,
	})
	var httpErr workspaceConfigHTTPError
	if !errors.As(err, &httpErr) || httpErr.status != http.StatusConflict || httpErr.message != "plan_revision_changed" {
		t.Fatalf("revision mismatch error = %#v, want 409 plan_revision_changed", err)
	}
}

func TestWorkspaceConfigApplyHandler_UpsertsBasicAuthSecretsFromRuntimeConfig(t *testing.T) {
	svcID := uuid.New()
	bucketID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		bucketsByName: map[string]*store.Bucket{
			"prod": {ID: bucketID, Name: "prod"},
		},
	}
	planID := uuid.New()
	configStore := workspaceApplyConfigStore(planID, workspaceBasicAuthApplyPayload(svcID, "prod"))
	verifier := &mockRegistryClient{
		name: "GitHub",
		serviceMetadata: &fusedobject.ServiceMetadata{
			ID: svcID,
			AuthConfigs: fusedobject.AuthConfigs{{
				Name:              "basicAuth",
				Type:              "http",
				Scheme:            "basic",
				BasicPasswordMode: authrouting.BasicPasswordRequired,
			}},
		},
	}

	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/config/apply", WorkspaceConfigApplyHandler(configStore, s, verifier, testMasterKey))
	req := httptest.NewRequest(http.MethodPost, "/workspace/config/apply", bytes.NewReader(workspaceBasicAuthApplyRequest(planID)))
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	assertWorkspaceBasicSecrets(t, s.upsertedSecrets, bucketID, svcID)
	if bytes.Contains(configStore.upserted.DesiredState, []byte("alice")) || bytes.Contains(configStore.upserted.DesiredState, []byte("s3cr3t")) {
		t.Fatal("applied desired state must not store resolved basic auth material")
	}
}

// TestWorkspaceConfigApplyHandler_UpsertsMTLSAuthSecretsFromRuntimeConfig
// verifies apply-time mTLS material is validated, encrypted, and kept out of
// persisted desired state.
func TestWorkspaceConfigApplyHandler_UpsertsMTLSAuthSecretsFromRuntimeConfig(t *testing.T) {
	svcID := uuid.New()
	bucketID := uuid.New()
	certPEM, keyPEM := workspaceTestMTLSPair(t)
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		bucketsByName: map[string]*store.Bucket{
			"prod": {ID: bucketID, Name: "prod"},
		},
	}
	planID := uuid.New()
	configStore := workspaceApplyConfigStore(planID, workspaceMTLSAuthApplyPayload(svcID, "prod"))
	verifier := &mockRegistryClient{
		name: "GitHub",
		serviceMetadata: &fusedobject.ServiceMetadata{
			ID: svcID,
			AuthConfigs: fusedobject.AuthConfigs{{
				Name: "clientCert",
				Type: "mutualTLS",
			}},
		},
	}

	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/config/apply", WorkspaceConfigApplyHandler(configStore, s, verifier, testMasterKey))
	req := httptest.NewRequest(http.MethodPost, "/workspace/config/apply", bytes.NewReader(workspaceMTLSAuthApplyRequest(planID, certPEM, keyPEM)))
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	assertWorkspaceMTLSSecrets(t, s.upsertedSecrets, bucketID, svcID, certPEM, keyPEM)
	if bytes.Contains(configStore.upserted.DesiredState, []byte(certPEM)) || bytes.Contains(configStore.upserted.DesiredState, []byte(keyPEM)) {
		t.Fatal("applied desired state must not store resolved mTLS auth material")
	}
}

func workspaceBasicAuthApplyPayload(svcID uuid.UUID, bucket string) json.RawMessage {
	payload := map[string]any{
		"kind":     "workspace",
		"services": workspaceTestServicePayload(svcID),
		"buckets": map[string]any{bucket: map[string]any{"service_config": map[string]any{"github": map[string]any{"auth": map[string]any{
			"auth_type": "basic", "username": "$GITHUB_BASIC_USER", "password": "$GITHUB_BASIC_PASS",
		}}}}},
	}
	body, _ := json.Marshal(payload)
	return body
}

// workspaceMTLSAuthApplyPayload keeps plan state on env refs so raw cert/key
// material can only arrive through apply materials.
func workspaceMTLSAuthApplyPayload(svcID uuid.UUID, bucket string) json.RawMessage {
	payload := map[string]any{
		"kind":     "workspace",
		"services": workspaceTestServicePayload(svcID),
		"buckets": map[string]any{bucket: map[string]any{"service_config": map[string]any{"github": map[string]any{"auth": map[string]any{
			"auth_type": "mtls", "cert": "$GITHUB_CLIENT_CERT", "key": "$GITHUB_CLIENT_KEY",
		}}}}},
	}
	body, _ := json.Marshal(payload)
	return body
}

// workspaceTestServicePayload keeps bucket-shape tests focused on credential
// placement instead of repeating service version boilerplate.
func workspaceTestServicePayload(svcID uuid.UUID) map[string]any {
	return map[string]any{"github": workspaceTestServiceEntry(svcID)}
}

func workspaceTestServiceEntry(svcID uuid.UUID) map[string]any {
	return map[string]any{
		"service_id": svcID.String(),
		"versions": []map[string]string{{
			"version": "2026-08-01", "service_version_id": uuid.NewString(),
		}},
	}
}

func workspaceBasicAuthApplyRequest(planID uuid.UUID) []byte {
	return []byte(`{
		"plan_id":"` + planID.String() + `",
		"source_hash":"abc",
		"auth_materials": {
			"prod\u0000github": {
				"username": "alice",
				"password": "s3cr3t"
			}
		}
	}`)
}

// workspaceMTLSAuthApplyRequest posts resolved material out-of-band, matching
// the CLI apply contract instead of embedding secrets in config.
func workspaceMTLSAuthApplyRequest(planID uuid.UUID, certPEM, keyPEM string) []byte {
	payload := map[string]any{
		"plan_id":     planID.String(),
		"source_hash": "abc",
		"auth_materials": map[string]any{
			"prod\x00github": map[string]string{
				"cert": certPEM,
				"key":  keyPEM,
			},
		},
	}
	body, _ := json.Marshal(payload)
	return body
}

func workspaceApplyConfigStore(planID uuid.UUID, payload json.RawMessage) *mockConfigStore {
	return &mockConfigStore{
		state: &store.ConfigState{Generation: 0},
		plan: &store.ConfigPlan{
			ID:              planID,
			Status:          store.ConfigPlanStatusPending,
			ConfigKey:       "workspace",
			ConfigType:      store.ConfigTypeWorkspace,
			SourceHash:      "abc",
			BaseGeneration:  0,
			ResolvedPayload: payload,
		},
	}
}

func assertWorkspaceBasicSecrets(t *testing.T, secrets []store.WorkspaceSecret, bucketID, serviceID uuid.UUID) {
	t.Helper()
	if len(secrets) != 2 {
		t.Fatalf("expected two basic auth secrets, got %#v", secrets)
	}
	values := map[string]string{}
	for _, secret := range secrets {
		if secret.BucketID != bucketID || secret.ServiceID != serviceID || secret.CredentialType != "basic" {
			t.Fatalf("unexpected basic secret identity: %#v", secret)
		}
		values[secret.KeyName] = decryptWorkspaceSecretForTest(t, secret)
	}
	if values["basicAuth_username"] != "alice" || values["basicAuth_password"] != "s3cr3t" {
		t.Fatalf("unexpected basic secret values: %#v", values)
	}
}

// assertWorkspaceMTLSSecrets checks the pair is stored under service-scoped
// cert/key names and encrypted before persistence.
func assertWorkspaceMTLSSecrets(t *testing.T, secrets []store.WorkspaceSecret, bucketID, serviceID uuid.UUID, certPEM, keyPEM string) {
	t.Helper()
	if len(secrets) != 2 {
		t.Fatalf("expected two mTLS auth secrets, got %#v", secrets)
	}
	values := map[string]string{}
	for _, secret := range secrets {
		if secret.BucketID != bucketID || secret.ServiceID != serviceID || secret.CredentialType != "mtls" {
			t.Fatalf("unexpected mTLS secret identity: %#v", secret)
		}
		values[secret.KeyName] = decryptWorkspaceSecretForTest(t, secret)
	}
	if values["clientCert_cert"] != certPEM || values["clientCert_key"] != keyPEM {
		t.Fatalf("unexpected mTLS secret values: %#v", values)
	}
}

// workspaceTestMTLSPair creates valid PEM material for apply tests without
// checking secrets into the repository.
func workspaceTestMTLSPair(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "fused-workspace-test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return string(certPEM), string(keyPEM)
}

func decryptWorkspaceSecretForTest(t *testing.T, secret store.WorkspaceSecret) string {
	t.Helper()
	dek, err := store.UnwrapDEK(testMasterKey, secret.EncryptedDEK)
	if err != nil {
		t.Fatalf("unwrap secret DEK: %v", err)
	}
	value, err := store.DecryptWithDEK(dek, secret.EncryptedValue)
	if err != nil {
		t.Fatalf("decrypt secret value: %v", err)
	}
	return value
}

func TestWorkspaceConfigApplyHandler_BlocksImpactedServiceRemovalWithoutForce(t *testing.T) {
	svcID := uuid.New()
	rr := runWorkspaceRemovalApply(t, svcID, "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "force_remove_required") {
		t.Fatalf("expected force_remove_required, got %s", rr.Body.String())
	}
}

func TestWorkspaceConfigApplyHandler_AllowsImpactedServiceRemovalWithForce(t *testing.T) {
	svcID := uuid.New()
	rr := runWorkspaceRemovalApply(t, svcID, "force_remove")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestWorkspaceConfigApplyHandler_ForceRemovalCreatesNotification(t *testing.T) {
	svcID := uuid.New()
	rr, configStore := runWorkspaceRemovalApplyWithStore(t, svcID, "force_remove")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(configStore.createdNotes) != 1 {
		t.Fatalf("expected one workspace notification, got %#v", configStore.createdNotes)
	}
	note := configStore.createdNotes[0]
	if note.Type != store.WorkspaceNotificationTypeServiceRemoved || note.Severity != store.WorkspaceNotificationSeverityBreaking {
		t.Fatalf("unexpected notification type/severity: %#v", note)
	}
	if note.ServiceID == nil || *note.ServiceID != svcID {
		t.Fatalf("expected service id %s, got %#v", svcID, note.ServiceID)
	}
	if note.ConfigKey != "sdk:security" {
		t.Fatalf("expected impacted config key, got %q", note.ConfigKey)
	}
}

func TestWorkspaceConfigApplyHandler_DeprecationDirectiveDoesNotRemoveService(t *testing.T) {
	svcID := uuid.New()
	planID := uuid.New()
	managed, _ := json.Marshal(workspaceManagedResources{Services: []workspaceManagedService{{
		ServiceID: svcID.String(),
		Versions:  []string{"2026-07-01"},
	}}})
	payload := json.RawMessage(`{
		"kind":"workspace",
		"services":{},
		"deprecations":[{"service_id":"` + svcID.String() + `","effective_at":"2026-09-01"}]
	}`)

	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
	}
	configStore := &mockConfigStore{
		state: &store.ConfigState{Generation: 3, ManagedResources: managed},
		plan: &store.ConfigPlan{
			ID:              planID,
			Status:          store.ConfigPlanStatusPending,
			ConfigKey:       "workspace",
			ConfigType:      store.ConfigTypeWorkspace,
			SourceHash:      "abc",
			BaseGeneration:  3,
			ResolvedPayload: payload,
		},
	}
	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/config/apply", WorkspaceConfigApplyHandler(configStore, s, &mockRegistryClient{name: "test"}, testMasterKey))

	body := []byte(`{"plan_id": "` + planID.String() + `", "source_hash": "abc"}`)
	req := httptest.NewRequest(http.MethodPost, "/workspace/config/apply", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(s.removedWorkspaceServices) != 0 {
		t.Fatalf("expected deprecation to keep service active, got removals %#v", s.removedWorkspaceServices)
	}
	var retained workspaceManagedResources
	if err := json.Unmarshal(configStore.upserted.ManagedResources, &retained); err != nil {
		t.Fatalf("decode managed resources: %v", err)
	}
	if len(retained.Services) != 1 || retained.Services[0].ServiceID != svcID.String() {
		t.Fatalf("expected deprecated service to remain managed, got %#v", retained.Services)
	}
}

func TestWorkspaceNotificationsGraphQL_ReturnsEngineInboxShape(t *testing.T) {
	noteID := uuid.New()
	svcID := uuid.New()
	configStore := &mockConfigStore{
		notifications: []store.WorkspaceNotification{{
			ID:        noteID,
			Type:      store.WorkspaceNotificationTypeServiceRemoved,
			Severity:  store.WorkspaceNotificationSeverityBreaking,
			Status:    store.WorkspaceNotificationStatusPending,
			ServiceID: &svcID,
			ConfigKey: "sdk:security",
			Message:   "service removed",
		}},
	}
	s := &workspaceTestStore{accountID: uuid.New()}
	h := mountWorkspaceNotificationsGraphQLTestHandler(t, configStore, s, &mockRegistryClient{})

	resp := workspaceNotificationsGraphQLData(t, h)
	items := resp["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected one item, got %#v", items)
	}
	first := items[0].(map[string]any)
	if first["id"] != "engine:"+noteID.String() || first["source"] != "engine" {
		t.Fatalf("expected engine-prefixed notification, got %#v", first)
	}
	if first["severity"] != "breaking" || first["status"] != "pending" {
		t.Fatalf("unexpected notification state: %#v", first)
	}
	if warnings := resp["warnings"].([]any); len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %#v", warnings)
	}
}

func TestWorkspaceNotificationsGraphQL_PaginatesAndOrdersUnreadFirst(t *testing.T) {
	// Three rows: one acknowledged (older) and two pending (newer), so
	// unread-first ordering (config_repository.go's "status = 'pending'
	// DESC, created_at DESC") should surface both pending rows on page 1
	// of a limit=2 query ahead of the older acknowledged row, even though
	// the acknowledged row isn't the oldest by created_at alone here.
	svcID := uuid.New()
	older := store.WorkspaceNotification{
		ID: uuid.New(), Type: store.WorkspaceNotificationTypeServiceRemoved,
		Severity: store.WorkspaceNotificationSeverityBreaking, Status: store.WorkspaceNotificationStatusAcknowledged,
		ServiceID: &svcID, Message: "acknowledged row",
	}
	pendingA := store.WorkspaceNotification{
		ID: uuid.New(), Type: store.WorkspaceNotificationTypeServiceRemoved,
		Severity: store.WorkspaceNotificationSeverityBreaking, Status: store.WorkspaceNotificationStatusPending,
		ServiceID: &svcID, Message: "pending row a",
	}
	pendingB := store.WorkspaceNotification{
		ID: uuid.New(), Type: store.WorkspaceNotificationTypeServiceRemoved,
		Severity: store.WorkspaceNotificationSeverityBreaking, Status: store.WorkspaceNotificationStatusPending,
		ServiceID: &svcID, Message: "pending row b",
	}
	configStore := &mockConfigStore{notifications: []store.WorkspaceNotification{older, pendingA, pendingB}}
	s := &workspaceTestStore{accountID: uuid.New()}
	h := mountWorkspaceNotificationsGraphQLTestHandler(t, configStore, s, &mockRegistryClient{})

	data := doMCPGraphQLRequest(t, h, `query {
		workspaceNotifications(page: 1, limit: 2) {
			items { message status }
			total_count
			pending_count
		}
	}`)
	resp := data["workspaceNotifications"].(map[string]any)
	items := resp["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("expected 2 items on page 1, got %#v", items)
	}
	for _, raw := range items {
		item := raw.(map[string]any)
		if item["status"] != "pending" {
			t.Fatalf("expected both page-1 items pending (unread-first ordering), got %#v", item)
		}
	}
	if resp["total_count"] != float64(3) {
		t.Fatalf("expected total_count 3 (all unresolved), got %#v", resp["total_count"])
	}
	if resp["pending_count"] != float64(2) {
		t.Fatalf("expected pending_count 2, got %#v", resp["pending_count"])
	}

	// unread_only=true should exclude the acknowledged row and shrink
	// total_count to match, so the numbered-page control stays consistent
	// with what's actually being paged through.
	data = doMCPGraphQLRequest(t, h, `query {
		workspaceNotifications(page: 1, limit: 10, unread_only: true) {
			items { message status }
			total_count
			pending_count
		}
	}`)
	resp = data["workspaceNotifications"].(map[string]any)
	items = resp["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("expected 2 unread items, got %#v", items)
	}
	if resp["total_count"] != float64(2) {
		t.Fatalf("expected total_count 2 when unread_only, got %#v", resp["total_count"])
	}
}

func mountWorkspaceNotificationsGraphQLTestHandler(t *testing.T, configStore store.ConfigRepository, s store.Store, registryClient sandbox.RegistryClient) http.HandlerFunc {
	t.Helper()
	schema, err := newMCPGraphQLSchema(configStore, s, &mockVerifier{}, registryClient, testMasterKey)
	if err != nil {
		t.Fatalf("newMCPGraphQLSchema() error = %v", err)
	}
	return withGraphQLTestOwner(t, s, mcpGraphQLHandler(schema))
}

func workspaceNotificationsGraphQLData(t *testing.T, h http.HandlerFunc) map[string]any {
	t.Helper()
	data := doMCPGraphQLRequest(t, h, `query {
		workspaceNotifications {
			items {
				id
				source
				type
				severity
				status
				service_id
				version
				config_key
				message
				integration_object_id
				webhook_object_id
				detected_at
				diff { field old_value new_value severity description }
			}
			warnings
		}
	}`)
	resp, ok := data["workspaceNotifications"].(map[string]any)
	if !ok {
		t.Fatalf("expected workspaceNotifications object, got %#v", data["workspaceNotifications"])
	}
	return resp
}

func runWorkspaceRemovalApply(t *testing.T, svcID uuid.UUID, decision string) *httptest.ResponseRecorder {
	rr, _ := runWorkspaceRemovalApplyWithStore(t, svcID, decision)
	return rr
}

func runWorkspaceRemovalApplyWithStore(t *testing.T, svcID uuid.UUID, decision string) (*httptest.ResponseRecorder, *mockConfigStore) {
	t.Helper()
	planID := uuid.New()
	managed, _ := json.Marshal(workspaceManagedResources{Services: []workspaceManagedService{{
		ServiceID: svcID.String(),
		Versions:  []string{"2026-07-01"},
	}}})
	action := workspacePlanAction{
		ID:                 workspaceActionID(workspaceplan.ActionRemoveService, svcID),
		Type:               workspaceplan.ActionRemoveService,
		ServiceID:          svcID.String(),
		RequiresDecision:   true,
		Decision:           decision,
		ImpactedSDKConfigs: []string{"sdk:security"},
	}
	actions, _ := json.Marshal([]workspacePlanAction{action})
	blockers, _ := json.Marshal([]workspacePlanBlocker{{
		Code:      "service_used_by_sdk",
		ServiceID: svcID.String(),
		ActionID:  action.ID,
		Message:   "requires force",
	}})

	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
	}
	configStore := &mockConfigStore{
		state: &store.ConfigState{Generation: 3, ManagedResources: managed},
		plan: &store.ConfigPlan{
			ID:              planID,
			Status:          store.ConfigPlanStatusPending,
			ConfigKey:       "workspace",
			ConfigType:      store.ConfigTypeWorkspace,
			SourceHash:      "abc",
			BaseGeneration:  3,
			ResolvedPayload: json.RawMessage(`{"kind":"workspace","services":{"replacement":{"service_id":"` + uuid.NewString() + `","versions":[{"version":"2026-08-01","service_version_id":"` + uuid.NewString() + `"}]}}}`),
			Actions:         actions,
			Blockers:        blockers,
		},
	}
	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/config/apply", WorkspaceConfigApplyHandler(configStore, s, &mockRegistryClient{name: "test"}, testMasterKey))

	body := []byte(`{"plan_id": "` + planID.String() + `", "source_hash": "abc"}`)
	req := httptest.NewRequest(http.MethodPost, "/workspace/config/apply", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr, configStore
}

type mockRegistryClient struct {
	sandbox.RegistryClient
	name                     string
	driftSnapshots           []models.DriftSnapshot
	driftErr                 error
	driftServiceIDs          []uuid.UUID
	driftServiceIDBatches    [][]uuid.UUID
	endpointNameBatches      [][]string
	slugIDs                  map[string]uuid.UUID
	archivedServiceIDs       []uuid.UUID
	archiveErr               error
	deprecatedVersions       []deprecatedVersionCall
	deprecateVersionErr      error
	slugBatches              [][]string
	contractRevisions        map[string]sandbox.ServiceVersionRevision
	versionResolutionBatches [][]sandbox.ServiceVersionRef
	latestVersionBatches     [][]uuid.UUID
	visibility               map[uuid.UUID]sandbox.ServiceVisibility
	visibilityBatches        [][]uuid.UUID
	visibilityUpdates        []serviceVisibilityUpdate
	// configUpdates records service-level policy publishes so tests can assert
	// the execution policy was published with the correct service ID.
	configUpdates  []uuid.UUID
	configPolicies []any
	// versionVisibilityUpdates and versionConfigUpdates are the per-version
	// equivalents of visibilityUpdates/configUpdates above.
	versionVisibilityUpdates []serviceVersionVisibilityUpdate
	versionConfigUpdates     []serviceVersionConfigUpdate
	validatedSelections      [][]models.SDKSelection
	serviceMetadata          *fusedobject.ServiceMetadata
	fetchMetadataCalls       int
}

// FetchServiceMetadata satisfies the ServiceVerifier extension used by the
// webhook-registration apply path (upsertWorkspaceServiceWebhooks). An
// explicit override is required rather than relying on the embedded
// sandbox.RegistryClient: that field is a zero-value interface in every
// existing test, so an unrouted call would nil-panic instead of returning a
// usable (if empty) result.
func (m *mockRegistryClient) FetchServiceMetadata(_ context.Context, serviceID uuid.UUID, _ string) (*fusedobject.ServiceMetadata, error) {
	m.fetchMetadataCalls++
	if m.serviceMetadata != nil {
		return m.serviceMetadata, nil
	}
	return &fusedobject.ServiceMetadata{ID: serviceID}, nil
}

func (m *mockRegistryClient) FetchServiceMetadataBatch(_ context.Context, refs []sandbox.ServiceMetadataRef) (map[string]*fusedobject.ServiceMetadata, error) {
	m.fetchMetadataCalls++
	result := make(map[string]*fusedobject.ServiceMetadata, len(refs))
	for _, ref := range refs {
		metadata := m.serviceMetadata
		if metadata == nil {
			metadata = &fusedobject.ServiceMetadata{ID: ref.ServiceID}
		}
		result[sandbox.ServiceMetadataRefKey(ref)] = metadata
	}
	return result, nil
}

func (m *mockRegistryClient) FetchServiceVersionAuthConfigs(_ context.Context, refs []sandbox.ServiceVersionRef, _ string) ([]sandbox.ServiceVersionAuthConfigs, error) {
	out := make([]sandbox.ServiceVersionAuthConfigs, 0, len(refs))
	for _, ref := range refs {
		var authConfigs fusedobject.AuthConfigs
		if m.serviceMetadata != nil {
			authConfigs = m.serviceMetadata.AuthConfigs
		}
		out = append(out, sandbox.ServiceVersionAuthConfigs{ServiceID: ref.ServiceID, Version: ref.Version, AuthConfigs: authConfigs})
	}
	return out, nil
}

func (m *mockRegistryClient) FetchServiceVersionExecutionAuthContracts(_ context.Context, selections []sandbox.ServiceVersionExecutionAuthSelection, _ string) ([]sandbox.ServiceVersionExecutionAuthContract, error) {
	configs := make(map[uuid.UUID]fusedobject.AuthConfigs, len(selections))
	if m.serviceMetadata != nil {
		for _, selection := range selections {
			configs[selection.ServiceID] = m.serviceMetadata.AuthConfigs
		}
	}
	return anonymousExecutionAuthContracts(selections, configs), nil
}

type serviceVisibilityUpdate struct {
	ServiceID uuid.UUID
	Public    bool
}

func (m *mockRegistryClient) FetchServiceVersionRevision(_ context.Context, serviceID uuid.UUID, version, _ string) (sandbox.ServiceVersionRevision, error) {
	return m.contractRevisions[serviceID.String()+"|"+version], nil
}

func (m *mockRegistryClient) VerifyServiceExists(_ context.Context, serviceID uuid.UUID, _ string) (string, string, string, uuid.UUID, error) {
	if m.name != "" {
		return m.name, "test/test-service", "2026-07-01", uuid.New(), nil
	}
	return "test", "test/test-service", "2026-07-01", uuid.New(), nil
}

func (m *mockRegistryClient) FetchServiceVisibility(_ context.Context, serviceIDs []uuid.UUID, _ string) (map[uuid.UUID]sandbox.ServiceVisibility, error) {
	m.visibilityBatches = append(m.visibilityBatches, append([]uuid.UUID(nil), serviceIDs...))
	out := map[uuid.UUID]sandbox.ServiceVisibility{}
	for _, serviceID := range serviceIDs {
		if vis, ok := m.visibility[serviceID]; ok {
			out[serviceID] = vis
		}
	}
	return out, nil
}

func (m *mockRegistryClient) UpdateServicePublic(_ context.Context, serviceID uuid.UUID, isPublic bool, _ string) error {
	m.visibilityUpdates = append(m.visibilityUpdates, serviceVisibilityUpdate{ServiceID: serviceID, Public: isPublic})
	return nil
}

func (m *mockRegistryClient) PublishServiceExecutionPolicy(_ context.Context, serviceID uuid.UUID, policy any, _ string) error {
	m.configUpdates = append(m.configUpdates, serviceID)
	m.configPolicies = append(m.configPolicies, policy)
	return nil
}

type serviceVersionVisibilityUpdate struct {
	ServiceID uuid.UUID
	Version   string
	Public    bool
}

type serviceVersionConfigUpdate struct {
	ServiceID uuid.UUID
	Version   string
	Policy    any
}

func (m *mockRegistryClient) UpdateServiceVersionPublic(_ context.Context, serviceID uuid.UUID, version string, isPublic bool, _ string) error {
	m.versionVisibilityUpdates = append(m.versionVisibilityUpdates, serviceVersionVisibilityUpdate{ServiceID: serviceID, Version: version, Public: isPublic})
	return nil
}

func (m *mockRegistryClient) PublishServiceVersionExecutionPolicy(_ context.Context, serviceID uuid.UUID, version string, policy any, _ string) error {
	m.versionConfigUpdates = append(m.versionConfigUpdates, serviceVersionConfigUpdate{ServiceID: serviceID, Version: version, Policy: policy})
	return nil
}

type deprecatedVersionCall struct {
	ServiceID uuid.UUID
	Version   string
}

func (m *mockRegistryClient) ArchiveService(_ context.Context, serviceID uuid.UUID, _ string) error {
	if m.archiveErr != nil {
		return m.archiveErr
	}
	m.archivedServiceIDs = append(m.archivedServiceIDs, serviceID)
	return nil
}

func (m *mockRegistryClient) DeprecateServiceVersion(_ context.Context, serviceID uuid.UUID, version, _ string) error {
	if m.deprecateVersionErr != nil {
		return m.deprecateVersionErr
	}
	m.deprecatedVersions = append(m.deprecatedVersions, deprecatedVersionCall{ServiceID: serviceID, Version: version})
	return nil
}

func (m *mockRegistryClient) FetchServiceVersionRevisions(_ context.Context, refs []sandbox.ServiceVersionRef, _ string) ([]sandbox.ServiceVersionRevision, error) {
	m.versionResolutionBatches = append(m.versionResolutionBatches, append([]sandbox.ServiceVersionRef(nil), refs...))
	out := make([]sandbox.ServiceVersionRevision, 0, len(refs))
	for _, ref := range refs {
		if revision, ok := m.contractRevisions[ref.ServiceID.String()+"|"+ref.Version]; ok {
			out = append(out, revision)
			continue
		}
		out = append(out, sandbox.ServiceVersionRevision{
			ServiceID: ref.ServiceID, Version: ref.Version, ServiceVersionID: uuid.New(), Revision: 1,
		})
	}
	return out, nil
}

func (m *mockRegistryClient) FetchLatestServiceVersions(_ context.Context, serviceIDs []uuid.UUID, _ string) ([]sandbox.ServiceVersionResolvedRef, error) {
	m.latestVersionBatches = append(m.latestVersionBatches, append([]uuid.UUID(nil), serviceIDs...))
	out := make([]sandbox.ServiceVersionResolvedRef, 0, len(serviceIDs))
	for _, serviceID := range serviceIDs {
		out = append(out, sandbox.ServiceVersionResolvedRef{
			ServiceID:        serviceID,
			Version:          "2026-07-01",
			ServiceVersionID: uuid.New(),
		})
	}
	return out, nil
}

func (m *mockRegistryClient) ResolveServiceIDsBySlugs(ctx context.Context, slugs []string, apiKey string) (map[string]uuid.UUID, error) {
	m.slugBatches = append(m.slugBatches, append([]string(nil), slugs...))
	out := map[string]uuid.UUID{}
	for _, slug := range slugs {
		if id, ok := m.slugIDs[slug]; ok {
			out[slug] = id
		}
	}
	return out, nil
}

func (m *mockRegistryClient) FetchDriftSnapshots(ctx context.Context, serviceID uuid.UUID, apiKey string) ([]models.DriftSnapshot, error) {
	m.driftServiceIDs = append(m.driftServiceIDs, serviceID)
	return m.driftSnapshots, m.driftErr
}

func (m *mockRegistryClient) FetchDriftSnapshotsForServices(ctx context.Context, serviceIDs []uuid.UUID, apiKey string) ([]models.DriftSnapshot, error) {
	m.driftServiceIDBatches = append(m.driftServiceIDBatches, append([]uuid.UUID(nil), serviceIDs...))
	m.driftServiceIDs = append(m.driftServiceIDs, serviceIDs...)
	return m.driftSnapshots, m.driftErr
}

// FetchServiceChangelogSince is unused by these tests but must exist for
// mockRegistryClient to satisfy sandbox.RegistryClient (Phase 2 widened that
// interface -- see plans/plan-service-changelog.md's "## Phase 2").
func (m *mockRegistryClient) FetchServiceChangelogSince(ctx context.Context, serviceID uuid.UUID, since time.Time, apiKey string) ([]models.ServiceChangelogEntry, error) {
	return nil, nil
}

func (m *mockRegistryClient) FetchEndpointByName(ctx context.Context, serviceID uuid.UUID, version string, endpointName string) (*fusedobject.Endpoint, error) {
	return &fusedobject.Endpoint{Name: endpointName}, nil
}

func (m *mockRegistryClient) ValidateSDKSelections(ctx context.Context, selections []models.SDKSelection) error {
	m.validatedSelections = append(m.validatedSelections, selections)
	return nil
}

func (m *mockRegistryClient) FetchEndpointsByNames(ctx context.Context, serviceID uuid.UUID, serviceVersionID uuid.UUID, endpointNames []string) ([]fusedobject.Endpoint, error) {
	m.endpointNameBatches = append(m.endpointNameBatches, append([]string(nil), endpointNames...))
	endpoints := make([]fusedobject.Endpoint, len(endpointNames))
	for i, name := range endpointNames {
		endpoints[i] = fusedobject.Endpoint{Name: name}
	}
	return endpoints, nil
}

func TestWorkspaceConfigApplyHandler_FirstApplyDoesNotRemoveUnmanagedServices(t *testing.T) {
	workspaceID := uuid.New()
	svcID := uuid.New()
	planID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: workspaceID,
		workspaceServices: []store.WorkspaceService{{
			ServiceID:   svcID,
			ServiceName: "unmanaged_service",
			Version:     "1.0.0",
		}},
	}
	configStore := &mockConfigStore{
		plan: &store.ConfigPlan{
			ID:              planID,
			ConfigKey:       "workspace",
			ConfigType:      store.ConfigTypeWorkspace,
			SourceHash:      "abc",
			BaseGeneration:  0,
			Status:          store.ConfigPlanStatusPending,
			ResolvedPayload: []byte(`{"services":{}}`),
		},
		state: &store.ConfigState{ConfigKey: "workspace", Generation: 0},
	}

	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/config/apply", WorkspaceConfigApplyHandler(configStore, s, &mockRegistryClient{name: "test"}, testMasterKey))

	body := fmt.Appendf(nil, `{"plan_id": "%s", "source_hash": "abc"}`, planID)
	req := httptest.NewRequest(http.MethodPost, "/workspace/config/apply", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(s.removedWorkspaceServices) > 0 {
		t.Errorf("expected unmanaged service to NOT be deactivated on first apply")
	}
}

func TestWorkspaceConfigApplyHandler_EnablesVersionsInConfigOrder(t *testing.T) {
	planID := uuid.New()
	svcID := uuid.New()

	s := &workspaceTestStore{accountID: uuid.New()}
	configStore := &mockConfigStore{
		plan: &store.ConfigPlan{
			ID:              planID,
			ConfigKey:       "workspace",
			ConfigType:      store.ConfigTypeWorkspace,
			SourceHash:      "abc",
			BaseGeneration:  0,
			Status:          store.ConfigPlanStatusPending,
			ResolvedPayload: fmt.Appendf(nil, `{"services":{"okta":{"service_id":"%s","versions":[{"version":"2026-06-01","service_version_id":"%s"},{"version":"2026-08-01","service_version_id":"%s"},{"version":"2026-07-01","service_version_id":"%s"}]}}}`, svcID, uuid.NewString(), uuid.NewString(), uuid.NewString()),
		},
		state: &store.ConfigState{ConfigKey: "workspace", Generation: 0},
	}

	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/config/apply", WorkspaceConfigApplyHandler(configStore, s, &mockRegistryClient{name: "test"}, testMasterKey))

	body := fmt.Appendf(nil, `{"plan_id": "%s", "source_hash": "abc"}`, planID)
	req := httptest.NewRequest(http.MethodPost, "/workspace/config/apply", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := s.gotVersion; got != "2026-06-01" {
		t.Errorf("expected first configured version to create the service, got %q", got)
	}
}

func TestWorkspaceConfigApplyHandler_AppliesOwnedServicePublicChange(t *testing.T) {
	planID := uuid.New()
	svcID := uuid.New()
	versionID := uuid.New()

	s := &workspaceTestStore{accountID: uuid.New()}
	resolved := []byte(`{"services":{"stripe":{"service_id":"` + svcID.String() + `","public":true,"versions":[{"version":"2026-07-01","service_version_id":"` + versionID.String() + `"}]}}}`)
	actions := []byte(`[{"id":"set_service_public:` + svcID.String() + `","type":"set_service_public","service_id":"` + svcID.String() + `","public":true}]`)
	configStore := &mockConfigStore{
		plan: &store.ConfigPlan{
			ID:              planID,
			ConfigKey:       "workspace",
			ConfigType:      store.ConfigTypeWorkspace,
			SourceHash:      "abc",
			Status:          store.ConfigPlanStatusPending,
			ResolvedPayload: resolved,
			Actions:         actions,
		},
		state: &store.ConfigState{ConfigKey: "workspace", Generation: 0},
	}
	verifier := &mockRegistryClient{
		name: "Stripe",
		visibility: map[uuid.UUID]sandbox.ServiceVisibility{
			svcID: {ServiceID: svcID, IsOwner: true, IsPublic: false},
		},
	}

	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/config/apply", WorkspaceConfigApplyHandler(configStore, s, verifier, testMasterKey))

	body := fmt.Appendf(nil, `{"plan_id": "%s", "source_hash": "abc"}`, planID)
	req := httptest.NewRequest(http.MethodPost, "/workspace/config/apply", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := verifier.visibilityUpdates; len(got) != 1 || got[0].ServiceID != svcID || !got[0].Public {
		t.Fatalf("expected public update for %s, got %#v", svcID, got)
	}
}

func TestWorkspaceConfigApplyHandler_AppliesVersionPublicAndExecutionPolicyChange(t *testing.T) {
	planID := uuid.New()
	svcID := uuid.New()
	versionID := uuid.New()

	s := &workspaceTestStore{accountID: uuid.New()}
	resolved := []byte(`{"services":{"stripe":{` +
		`"service_id":"` + svcID.String() + `",` +
		`"versions":[{"version":"2026-07-01","service_version_id":"` + versionID.String() + `","public":false,"execution_policy":{"public":true,"rate_limit":{"version":3,"policies":[{"name":"requests","mode":"enforce","unit":"requests","identity":{"inputs":[{"kind":"service_version"}]},"cost":{"default":1,"rules":[]},"algorithm":"fixed_window","fixed_window":{"limit":5,"duration_ms":1000}}]}}}]` +
		`}}}`)
	actions := []byte(`[` +
		`{"id":"set_service_version_private:` + svcID.String() + `:2026-07-01","type":"set_service_version_private","service_id":"` + svcID.String() + `","version":"2026-07-01","public":false},` +
		`{"id":"publish_service_version_execution_policy:` + svcID.String() + `:2026-07-01","type":"publish_service_version_execution_policy","service_id":"` + svcID.String() + `","version":"2026-07-01","public":true}` +
		`]`)
	configStore := &mockConfigStore{
		plan: &store.ConfigPlan{
			ID:              planID,
			ConfigKey:       "workspace",
			ConfigType:      store.ConfigTypeWorkspace,
			SourceHash:      "abc",
			Status:          store.ConfigPlanStatusPending,
			ResolvedPayload: resolved,
			Actions:         actions,
		},
		state: &store.ConfigState{ConfigKey: "workspace", Generation: 0},
	}
	verifier := &mockRegistryClient{
		name: "Stripe",
		visibility: map[uuid.UUID]sandbox.ServiceVisibility{
			svcID: {ServiceID: svcID, IsOwner: true, IsPublic: true},
		},
	}

	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/config/apply", WorkspaceConfigApplyHandler(configStore, s, verifier, testMasterKey))

	body := fmt.Appendf(nil, `{"plan_id": "%s", "source_hash": "abc"}`, planID)
	req := httptest.NewRequest(http.MethodPost, "/workspace/config/apply", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := verifier.versionVisibilityUpdates; len(got) != 1 || got[0].ServiceID != svcID || got[0].Version != "2026-07-01" || got[0].Public {
		t.Fatalf("expected version-private update for %s, got %#v", svcID, got)
	}
	if got := verifier.versionConfigUpdates; len(got) != 1 || got[0].ServiceID != svcID || got[0].Version != "2026-07-01" {
		t.Fatalf("expected version execution policy publish for %s, got %#v", svcID, got)
	}
}

func TestWorkspaceConfigApplyHandler_VersionForceRemovalCreatesNotification(t *testing.T) {
	workspaceID := uuid.New()
	planID := uuid.New()
	svcID := uuid.New()

	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: workspaceID,
		workspaceServices: []store.WorkspaceService{{
			ServiceID:   svcID,
			ServiceName: "okta",
			Version:     "2026-08-01",
		}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			svcID: {{ServiceID: svcID, Version: "2026-07-01"}, {ServiceID: svcID, Version: "2026-08-01"}},
		},
	}
	configStore := &mockConfigStore{
		plan: &store.ConfigPlan{
			ID:              planID,
			ConfigKey:       "workspace",
			ConfigType:      store.ConfigTypeWorkspace,
			SourceHash:      "abc",
			BaseGeneration:  1,
			Status:          store.ConfigPlanStatusPending,
			ResolvedPayload: fmt.Appendf(nil, `{"services":{"okta":{"service_id":"%s","versions":[{"version":"2026-08-01","service_version_id":"%s"}]}}}`, svcID, uuid.NewString()),
			Actions:         fmt.Appendf(nil, `[{"id":"disable_service_version:%s:2026-07-01","type":"disable_service_version","service_id":"%s","version":"2026-07-01","decision":"force_remove","impacted_sdk_configs":["sdk:test"]}]`, svcID, svcID),
		},
		state: &store.ConfigState{
			ConfigKey:        "workspace",
			Generation:       1,
			ManagedResources: fmt.Appendf(nil, `{"%s":{"service_id":"%s","versions":["2026-07-01","2026-08-01"]}}`, svcID, svcID),
		},
	}

	r := newControlTestRouter(s.accountID)
	r.Post("/workspace/config/apply", WorkspaceConfigApplyHandler(configStore, s, &mockRegistryClient{name: "test"}, testMasterKey))

	body := fmt.Appendf(nil, `{"plan_id": "%s", "source_hash": "abc"}`, planID)
	req := httptest.NewRequest(http.MethodPost, "/workspace/config/apply", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(configStore.notifications) != 1 {
		t.Fatalf("expected 1 notification created, got %d", len(configStore.notifications))
	}
	if got := configStore.notifications[0].Type; got != store.WorkspaceNotificationTypeVersionRemoved {
		t.Errorf("expected version removed notification type, got %q", got)
	}
}

func TestWorkspaceNotificationsGraphQL_AggregatesRegistryDrift(t *testing.T) {
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{DriftMonitoringEnabled: true})
	defer entitlement.LiveEntitlement.Reset()
	workspaceID := uuid.New()
	svcID := uuid.New()
	configStore := &mockConfigStore{
		notifications: []store.WorkspaceNotification{},
	}
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: workspaceID,
		workspaceServices: []store.WorkspaceService{{
			ServiceID:   svcID,
			ServiceName: "okta",
			Version:     "1.0.0",
		}},
	}
	registryClient := &mockRegistryClient{
		driftSnapshots: []models.DriftSnapshot{{
			ID:                  uuid.New(),
			IntegrationObjectID: uuid.New(),
		}},
	}

	h := mountWorkspaceNotificationsGraphQLTestHandler(t, configStore, s, registryClient)
	resp := workspaceNotificationsGraphQLData(t, h)
	items := resp["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected one item, got %#v", items)
	}
	first := items[0].(map[string]any)
	if first["source"] != "registry" || first["type"] != "endpoint_drift" {
		t.Errorf("unexpected item: %#v", first)
	}

	// Regression guard for the N+1 fix: one activated service must still
	// result in exactly one batched Registry call, not a per-service loop
	// that happens to look the same at N=1.
	if len(registryClient.driftServiceIDBatches) != 1 {
		t.Fatalf("expected exactly one batched drift call, got %d: %#v", len(registryClient.driftServiceIDBatches), registryClient.driftServiceIDBatches)
	}
}

// TestWorkspaceNotificationsGraphQL_BatchesDriftAcrossMultipleServices is
// the real N+1 regression guard: a workspace with several activated
// services must still cost exactly one Registry call, covering every
// service, not one call per service.
func TestWorkspaceNotificationsGraphQL_BatchesDriftAcrossMultipleServices(t *testing.T) {
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{DriftMonitoringEnabled: true})
	defer entitlement.LiveEntitlement.Reset()
	workspaceID := uuid.New()
	svcA := uuid.New()
	svcB := uuid.New()
	svcC := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: workspaceID,
		workspaceServices: []store.WorkspaceService{
			{ServiceID: svcA, ServiceName: "okta", Version: "1.0.0"},
			{ServiceID: svcB, ServiceName: "github", Version: "1.0.0"},
			{ServiceID: svcC, ServiceName: "slack", Version: "1.0.0"},
		},
	}
	registryClient := &mockRegistryClient{}

	h := mountWorkspaceNotificationsGraphQLTestHandler(t, &mockConfigStore{}, s, registryClient)
	workspaceNotificationsGraphQLData(t, h)
	if len(registryClient.driftServiceIDBatches) != 1 {
		t.Fatalf("expected exactly one batched drift call for 3 services, got %d: %#v", len(registryClient.driftServiceIDBatches), registryClient.driftServiceIDBatches)
	}
	if got, want := len(registryClient.driftServiceIDBatches[0]), 3; got != want {
		t.Fatalf("expected the batched call to cover all 3 activated services, got %d ids: %#v", got, registryClient.driftServiceIDBatches[0])
	}
}

// A persisted false value from the former Dev-plan gate is normalized to true,
// so drift remains available after upgrading an existing Engine.
func TestWorkspaceNotificationsGraphQL_LegacyDriftFalseStillCallsRegistry(t *testing.T) {
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{DriftMonitoringEnabled: false})
	defer entitlement.LiveEntitlement.Reset()

	workspaceID := uuid.New()
	svcID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: workspaceID,
		workspaceServices: []store.WorkspaceService{{
			ServiceID:   svcID,
			ServiceName: "okta",
			Version:     "1.0.0",
		}},
	}
	registryClient := &mockRegistryClient{
		driftSnapshots: []models.DriftSnapshot{{ID: uuid.New()}},
	}

	h := mountWorkspaceNotificationsGraphQLTestHandler(t, &mockConfigStore{}, s, registryClient)
	resp := workspaceNotificationsGraphQLData(t, h)
	items := resp["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected drift item after legacy value normalization, got %#v", items)
	}
	if len(registryClient.driftServiceIDBatches) != 1 {
		t.Fatalf("expected registry drift call after legacy value normalization, got %#v", registryClient.driftServiceIDBatches)
	}
}

// TestWorkspaceNotificationsGraphQL_DriftEnabled_CallsRegistry verifies
// the positive path: when DriftMonitoringEnabled is true the registry
// is still called and drift items appear.
func TestWorkspaceNotificationsGraphQL_DriftEnabled_CallsRegistry(t *testing.T) {
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{DriftMonitoringEnabled: true})
	defer entitlement.LiveEntitlement.Reset()

	workspaceID := uuid.New()
	svcID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: workspaceID,
		workspaceServices: []store.WorkspaceService{{
			ServiceID:   svcID,
			ServiceName: "okta",
			Version:     "1.0.0",
		}},
	}
	registryClient := &mockRegistryClient{
		driftSnapshots: []models.DriftSnapshot{{
			ID:                  uuid.New(),
			IntegrationObjectID: uuid.New(),
		}},
	}

	h := mountWorkspaceNotificationsGraphQLTestHandler(t, &mockConfigStore{}, s, registryClient)
	resp := workspaceNotificationsGraphQLData(t, h)
	items := resp["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected one drift item when enabled, got %#v", items)
	}
	first := items[0].(map[string]any)
	if first["source"] != "registry" || first["type"] != "endpoint_drift" {
		t.Errorf("unexpected item: %#v", first)
	}
	if len(registryClient.driftServiceIDBatches) != 1 {
		t.Fatalf("expected exactly one batched drift call, got %d: %#v", len(registryClient.driftServiceIDBatches), registryClient.driftServiceIDBatches)
	}
}

// ---------------------------------------------------------------------------
// Task 3: archiveRemovedOwnedServices + planRemovedServiceIDs
// ---------------------------------------------------------------------------

func makeRemoveServicePlan(serviceIDs ...uuid.UUID) *store.ConfigPlan {
	actions := make([]workspacePlanAction, 0, len(serviceIDs))
	for _, id := range serviceIDs {
		actions = append(actions, workspacePlanAction{
			ID:        workspaceActionID(workspaceplan.ActionRemoveService, id),
			Type:      workspaceplan.ActionRemoveService,
			ServiceID: id.String(),
		})
	}
	b, _ := json.Marshal(actions)
	return &store.ConfigPlan{Actions: b}
}

func TestPlanRemovedServiceIDs_ReturnsOnlyRemoveActions(t *testing.T) {
	svc1 := uuid.New()
	svc2 := uuid.New()
	addSvc := uuid.New()

	actions := []workspacePlanAction{
		{ID: "a1", Type: workspaceplan.ActionRemoveService, ServiceID: svc1.String()},
		{ID: "a2", Type: workspaceplan.ActionAddService, ServiceID: addSvc.String()},
		{ID: "a3", Type: workspaceplan.ActionRemoveService, ServiceID: svc2.String()},
	}
	b, _ := json.Marshal(actions)
	plan := &store.ConfigPlan{Actions: b}

	ids := planRemovedServiceIDs(plan)
	if len(ids) != 2 {
		t.Fatalf("expected 2 removed IDs, got %d", len(ids))
	}
	got := map[uuid.UUID]bool{ids[0]: true, ids[1]: true}
	if !got[svc1] || !got[svc2] {
		t.Errorf("expected %s and %s in result, got %v", svc1, svc2, ids)
	}
}

func TestPlanRemovedServiceIDs_EmptyPlan(t *testing.T) {
	ids := planRemovedServiceIDs(&store.ConfigPlan{})
	if len(ids) != 0 {
		t.Errorf("expected no IDs for empty plan, got %v", ids)
	}
}

func TestArchiveRemovedOwnedServices_ArchivesOnlyOwned(t *testing.T) {
	ctx := context.Background()
	ownedID := uuid.New()
	nonOwnedID := uuid.New()

	rc := &mockRegistryClient{
		visibility: map[uuid.UUID]sandbox.ServiceVisibility{
			ownedID:    {ServiceID: ownedID, IsOwner: true},
			nonOwnedID: {ServiceID: nonOwnedID, IsOwner: false},
		},
	}
	plan := makeRemoveServicePlan(ownedID, nonOwnedID)
	call := workspaceApplyCall{apiKey: "fsk_test"}

	if err := archiveRemovedOwnedServices(ctx, rc, call, plan); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rc.archivedServiceIDs) != 1 || rc.archivedServiceIDs[0] != ownedID {
		t.Errorf("expected only owned service %s archived, got %v", ownedID, rc.archivedServiceIDs)
	}
}

func TestArchiveRemovedOwnedServices_NoActionsIsNoop(t *testing.T) {
	ctx := context.Background()
	rc := &mockRegistryClient{}
	plan := &store.ConfigPlan{} // no actions

	if err := archiveRemovedOwnedServices(ctx, rc, workspaceApplyCall{}, plan); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rc.archivedServiceIDs) != 0 {
		t.Errorf("expected no archives for empty plan, got %v", rc.archivedServiceIDs)
	}
}

// A verifier that doesn't implement ServiceArchiver should be a silent no-op
// so that Engines without the capability don't break on removal actions.
func TestArchiveRemovedOwnedServices_MissingCapabilityReturnsError(t *testing.T) {
	ctx := context.Background()
	svcID := uuid.New()

	// A bare struct that implements neither ServiceVisibilityResolver nor
	// ServiceArchiver must return an error — not silently skip. A missing
	// capability is a misconfiguration, not a supported downgrade path.
	type bareClient struct{ sandbox.RegistryClient }
	plan := makeRemoveServicePlan(svcID)
	if err := archiveRemovedOwnedServices(ctx, &bareClient{}, workspaceApplyCall{}, plan); err == nil {
		t.Fatal("expected error when archiver capability is absent, got nil")
	}
}

func TestArchiveRemovedOwnedServices_PropagatesArchiveError(t *testing.T) {
	ctx := context.Background()
	svcID := uuid.New()

	rc := &mockRegistryClient{
		visibility: map[uuid.UUID]sandbox.ServiceVisibility{
			svcID: {ServiceID: svcID, IsOwner: true},
		},
		archiveErr: errors.New("registry 500"),
	}
	plan := makeRemoveServicePlan(svcID)
	call := workspaceApplyCall{apiKey: "fsk_test"}

	err := archiveRemovedOwnedServices(ctx, rc, call, plan)
	if err == nil {
		t.Fatal("expected error from failing ArchiveService, got nil")
	}
}

// ---------------------------------------------------------------------------
// Task 4: WillArchive label on remove_service plan actions
// ---------------------------------------------------------------------------

func TestPlanWorkspaceChanges_WillArchiveOnlyForOwned(t *testing.T) {
	ownedID := uuid.New()
	nonOwnedID := uuid.New()

	previousManaged := map[uuid.UUID]workspaceManagedService{
		ownedID:    {ServiceID: ownedID.String(), Versions: []string{"2026-07-01"}},
		nonOwnedID: {ServiceID: nonOwnedID.String(), Versions: []string{"2026-07-01"}},
	}
	// Neither service is in desired — both trigger remove_service actions.
	desired := workspaceDesiredState{Services: map[uuid.UUID]workspaceDesiredService{}}
	visibility := map[uuid.UUID]sandbox.ServiceVisibility{
		ownedID:    {ServiceID: ownedID, IsOwner: true},
		nonOwnedID: {ServiceID: nonOwnedID, IsOwner: false},
	}

	summary := planWorkspaceChanges(desired, nil, previousManaged, nil, visibility)

	archiveActions := map[uuid.UUID]bool{}
	for _, action := range summary.Actions {
		if action.Type != workspaceplan.ActionRemoveService {
			continue
		}
		id, _ := uuid.Parse(action.ServiceID)
		archiveActions[id] = action.WillArchive
	}

	if !archiveActions[ownedID] {
		t.Errorf("owned service %s should have WillArchive=true", ownedID)
	}
	if archiveActions[nonOwnedID] {
		t.Errorf("non-owned service %s should have WillArchive=false, got true", nonOwnedID)
	}
}

func TestDesiredVersionVisibilityActionsUsesRegistryCurrentState(t *testing.T) {
	serviceID := uuid.New()
	private := false
	desired := workspaceDesiredState{Services: map[uuid.UUID]workspaceDesiredService{serviceID: {
		ServiceID: serviceID, VersionPolicies: []workspaceDesiredVersionPolicy{{Version: "1.0.0", Public: &private, CurrentPublic: &private}},
	}}}
	if actions := desiredVersionVisibilityActions(desired); len(actions) != 0 {
		t.Fatalf("unchanged private version produced actions: %+v", actions)
	}
	public := true
	policy := desired.Services[serviceID]
	policy.VersionPolicies[0].CurrentPublic = &public
	desired.Services[serviceID] = policy
	if actions := desiredVersionVisibilityActions(desired); len(actions) != 1 || actions[0].Type != "set_service_version_private" {
		t.Fatalf("version visibility drift was not planned: %+v", actions)
	}
}

func TestRejectConfiguredRegistryVisibility(t *testing.T) {
	public := true
	err := rejectConfiguredRegistryVisibility(map[string]workspaceConfigService{
		"jira": {Versions: []workspaceConfigServiceVersion{{Version: "1.0.0", RegistryPublic: &public}}},
	})
	if err == nil || !strings.Contains(err.Error(), "registry_public is read-only") {
		t.Fatalf("expected read-only Registry visibility error, got %v", err)
	}
}

func TestReconcileWorkspaceExecutionPolicyActionsUsesExactCurrentTier(t *testing.T) {
	serviceID := uuid.New()
	retry := canonicalWorkspaceRetryTest(t)
	policy := &workspaceExecutionPolicy{Retry: &retry}
	desired := workspaceDesiredState{Services: map[uuid.UUID]workspaceDesiredService{serviceID: {ServiceID: serviceID, ExecutionPolicy: policy}}}
	expected := workspaceExecutionPolicyOverride(serviceID, nil, policy)
	ref := store.WorkspaceExecutionPolicyRef{ServiceID: serviceID}
	s := &workspaceTestStore{exactExecutionPolicies: map[store.WorkspaceExecutionPolicyRef]*store.WorkspaceExecutionPolicyOverride{ref: &expected}}
	summary := workspacePlanSummary{Actions: desiredExecutionPolicyLocalActions(desired)}
	if err := reconcileWorkspaceExecutionPolicyPlanActions(context.Background(), s, desired, &summary); err != nil {
		t.Fatalf("reconcile exact policy: %v", err)
	}
	if len(summary.Actions) != 0 {
		t.Fatalf("unchanged exact policy produced actions: %+v", summary.Actions)
	}
	drifted := expected
	driftedRetry := canonicalWorkspaceRetryTest(t)
	driftedRetry.Rules[0].Action.MaxAttempts++
	drifted.RetryConfig = &driftedRetry
	s.exactExecutionPolicies[ref] = &drifted
	summary.Actions = desiredExecutionPolicyLocalActions(desired)
	if err := reconcileWorkspaceExecutionPolicyPlanActions(context.Background(), s, desired, &summary); err != nil || len(summary.Actions) != 1 {
		t.Fatalf("policy drift was not retained: actions=%+v err=%v", summary.Actions, err)
	}
}

// ---------------------------------------------------------------------------
// Task 5: applyDeprecationActions
// ---------------------------------------------------------------------------

func makePlanWithActions(actions []workspacePlanAction) *store.ConfigPlan {
	b, _ := json.Marshal(actions)
	return &store.ConfigPlan{Actions: b}
}

func TestApplyDeprecationActions_DeprecatesVersions(t *testing.T) {
	ctx := context.Background()
	svcID := uuid.New()

	rc := &mockRegistryClient{}
	plan := makePlanWithActions([]workspacePlanAction{
		{Type: workspaceplan.ActionDeprecateVersion, ServiceID: svcID.String(), Version: "2026-07-01"},
		{Type: workspaceplan.ActionDeprecateVersion, ServiceID: svcID.String(), Version: "2026-06-01"},
		{Type: workspaceplan.ActionAddService, ServiceID: uuid.New().String()}, // must be ignored
	})
	call := workspaceApplyCall{apiKey: "fsk_test"}

	if err := applyDeprecationActions(ctx, rc, call, plan, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rc.deprecatedVersions) != 2 {
		t.Fatalf("expected 2 deprecated versions, got %d: %v", len(rc.deprecatedVersions), rc.deprecatedVersions)
	}
	got := map[string]bool{}
	for _, d := range rc.deprecatedVersions {
		got[d.ServiceID.String()+"|"+d.Version] = true
	}
	if !got[svcID.String()+"|2026-07-01"] || !got[svcID.String()+"|2026-06-01"] {
		t.Errorf("unexpected deprecated calls: %v", rc.deprecatedVersions)
	}
}

func TestApplyDeprecationActions_NoActionsIsNoop(t *testing.T) {
	ctx := context.Background()
	rc := &mockRegistryClient{}

	if err := applyDeprecationActions(ctx, rc, workspaceApplyCall{}, &store.ConfigPlan{}, nil); err != nil {
		t.Fatalf("unexpected error for empty plan: %v", err)
	}
	if len(rc.deprecatedVersions) != 0 {
		t.Errorf("expected no deprecations, got %v", rc.deprecatedVersions)
	}
}

func TestApplyDeprecationActions_MissingCapabilityReturnsError(t *testing.T) {
	ctx := context.Background()
	svcID := uuid.New()
	plan := makePlanWithActions([]workspacePlanAction{
		{Type: workspaceplan.ActionDeprecateVersion, ServiceID: svcID.String(), Version: "2026-07-01"},
	})

	// A client that does NOT implement ServiceVersionDeprecator must return an
	// error — not silently skip. Missing capability is a misconfiguration.
	type bareClient struct{ sandbox.RegistryClient }
	if err := applyDeprecationActions(ctx, &bareClient{}, workspaceApplyCall{}, plan, nil); err == nil {
		t.Fatal("expected error when deprecation capability absent, got nil")
	}
}

func TestApplyDeprecationActions_PropagatesError(t *testing.T) {
	ctx := context.Background()
	svcID := uuid.New()

	rc := &mockRegistryClient{deprecateVersionErr: errors.New("registry 500")}
	plan := makePlanWithActions([]workspacePlanAction{
		{Type: workspaceplan.ActionDeprecateVersion, ServiceID: svcID.String(), Version: "2026-07-01"},
	})

	if err := applyDeprecationActions(ctx, rc, workspaceApplyCall{apiKey: "fsk_test"}, plan, nil); err == nil {
		t.Fatal("expected error from failing DeprecateServiceVersion, got nil")
	}
}

// ---------------------------------------------------------------------------
// Task 6: previouslyAppliedDeprecations + idempotent skip
// ---------------------------------------------------------------------------

func makeDesiredStateWithDeprecations(serviceID uuid.UUID, versions ...string) []byte {
	type dep struct {
		ServiceID   string `json:"service_id"`
		Version     string `json:"version,omitempty"`
		EffectiveAt string `json:"effective_at"`
	}
	deps := make([]dep, 0, len(versions))
	for _, v := range versions {
		deps = append(deps, dep{ServiceID: serviceID.String(), Version: v, EffectiveAt: "2026-12-31"})
	}
	b, _ := json.Marshal(map[string]any{"deprecations": deps})
	return b
}

func TestPreviouslyAppliedDeprecations_ParsesVersionKeys(t *testing.T) {
	svcID := uuid.New()
	state := &store.ConfigState{
		DesiredState: makeDesiredStateWithDeprecations(svcID, "2026-07-01", "2026-06-01"),
	}

	got := previouslyAppliedDeprecations(state)
	if !got[svcID.String()+"|2026-07-01"] {
		t.Errorf("expected 2026-07-01 in set, got %v", got)
	}
	if !got[svcID.String()+"|2026-06-01"] {
		t.Errorf("expected 2026-06-01 in set, got %v", got)
	}
}

func TestPreviouslyAppliedDeprecations_NilStateReturnsNil(t *testing.T) {
	if got := previouslyAppliedDeprecations(nil); got != nil {
		t.Errorf("expected nil for nil state, got %v", got)
	}
}

func TestApplyDeprecationActions_SkipsAlreadyApplied(t *testing.T) {
	ctx := context.Background()
	svcID := uuid.New()
	rc := &mockRegistryClient{}

	// Both versions in the plan — but 2026-07-01 was already applied last time.
	plan := makePlanWithActions([]workspacePlanAction{
		{Type: workspaceplan.ActionDeprecateVersion, ServiceID: svcID.String(), Version: "2026-07-01"},
		{Type: workspaceplan.ActionDeprecateVersion, ServiceID: svcID.String(), Version: "2026-06-01"},
	})
	currentState := &store.ConfigState{
		DesiredState: makeDesiredStateWithDeprecations(svcID, "2026-07-01"),
	}
	call := workspaceApplyCall{apiKey: "fsk_test"}

	if err := applyDeprecationActions(ctx, rc, call, plan, currentState); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only 2026-06-01 should have been sent; 2026-07-01 was already applied.
	if len(rc.deprecatedVersions) != 1 || rc.deprecatedVersions[0].Version != "2026-06-01" {
		t.Errorf("expected only 2026-06-01 to be deprecated, got %v", rc.deprecatedVersions)
	}
}

// TestConfigPlanApplyReservationErrorReturnsRetryTiming verifies the public 409 contract.
func TestConfigPlanApplyReservationErrorReturnsRetryTiming(t *testing.T) {
	expiresAt := time.Now().Add(45 * time.Second).UTC()
	httpErr := configPlanApplyReservationHTTPError(&store.ConfigPlanApplyInProgressError{ExpiresAt: expiresAt})
	recorder := httptest.NewRecorder()
	writeWorkspaceConfigError(recorder, httpErr)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", recorder.Code)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After header is missing")
	}
	var response workspaceConfigErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Code != "plan_apply_in_progress" || !response.Error.Retryable {
		t.Fatalf("error = %#v", response.Error)
	}
	if response.Error.Details["apply_lease_expires_at"] == "" || response.Error.Details["retry_after_seconds"] == nil {
		t.Fatalf("details = %#v", response.Error.Details)
	}
}
