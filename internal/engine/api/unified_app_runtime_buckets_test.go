package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

type runtimeBucketFixture struct {
	store.Store
	buckets  []store.Bucket
	allowed  bool
	err      error
	checks   int
	selected uuid.UUID
}

// GetBucketsByNames models a bounded authoritative lookup without secret access.
func (f *runtimeBucketFixture) GetBucketsByNames(context.Context, []string) ([]store.Bucket, error) {
	return f.buckets, f.err
}

// AuthorizeUnifiedAppRuntimeBucket records each uncached admission decision.
func (f *runtimeBucketFixture) AuthorizeUnifiedAppRuntimeBucket(_ context.Context, _, _, _, bucket uuid.UUID) (bool, error) {
	f.checks++
	f.selected = bucket
	return f.allowed, f.err
}

// TestRuntimeBucketConfigAdmission rejects ambiguous opt-ins before any plan can gain authority.
func TestRuntimeBucketConfigAdmission(t *testing.T) {
	cases := []struct {
		kind  string
		names []string
		valid bool
	}{
		{"unified_app", nil, true}, {"sdk", nil, true}, {"unified_app", []string{"tenant-a", "tenant-b"}, true},
		{"sdk", []string{"tenant-a"}, false}, {"mcp", []string{"tenant-a"}, false},
		{"unified_app", []string{"*"}, false}, {"unified_app", []string{""}, false}, {"unified_app", []string{" a"}, false}, {"unified_app", []string{"a", "a"}, false},
		{"unified_app", make([]string, 33), false},
	}
	for _, tc := range cases {
		err := validateRuntimeBuckets(sdkConfigDocument{Kind: tc.kind, RuntimeBuckets: tc.names})
		// Accepted configuration must match the explicit finite-scope contract.
		if (err == nil) != tc.valid {
			t.Fatalf("kind=%s names=%v valid=%t err=%v", tc.kind, tc.names, tc.valid, err)
		}
	}
}

// TestRuntimeBucketPlanPinsIdentity proves replacement names and missing IDs never reuse reviewed permission.
func TestRuntimeBucketPlanPinsIdentity(t *testing.T) {
	bucket := store.Bucket{ID: uuid.New(), Name: "tenant"}
	fixture := &runtimeBucketFixture{buckets: []store.Bucket{bucket}}
	doc := sdkConfigDocument{Kind: "unified_app", RuntimeBuckets: []string{"tenant"}}
	refs := runtimeBucketRefs(fixture.buckets)
	// Unchanged names and IDs remain valid through delayed apply.
	if err := verifyRuntimeBucketPlan(context.Background(), fixture, doc, refs); err != nil {
		t.Fatal(err)
	}
	fixture.buckets[0].ID = uuid.New()
	// Deletion and recreation under the same name must require a fresh plan.
	if err := verifyRuntimeBucketPlan(context.Background(), fixture, doc, refs); err == nil {
		t.Fatal("replacement bucket admitted")
	}
	fixture.buckets = nil
	if _, err := resolveRuntimeBuckets(context.Background(), fixture, doc); err == nil {
		t.Fatal("partial scope admitted")
	}
}

// TestRuntimeBucketPermissionsCoverScope includes runtime targets in ordinary plan actor/owner authorization.
func TestRuntimeBucketPermissionsCoverScope(t *testing.T) {
	a, b := store.Bucket{ID: uuid.New(), Name: "default"}, store.Bucket{ID: uuid.New(), Name: "tenant"}
	buckets := appBucketSet{Default: a, Runtime: []store.Bucket{a, b}, Overrides: map[uuid.UUID]store.Bucket{uuid.New(): b}}
	requirements, _, err := configBucketRequirements(buckets.distinct())
	// Overlap is deduplicated but neither runtime nor static bucket can lose its permission check.
	if err != nil || len(requirements) != 2 {
		t.Fatalf("requirements=%v err=%v", requirements, err)
	}
}

// TestRuntimeBucketAdmissionChecksEveryRun prevents cached permission, outage fallback and nil-store bypass.
func TestRuntimeBucketAdmissionChecksEveryRun(t *testing.T) {
	fixture := &runtimeBucketFixture{allowed: true}
	server := &EngineGRPCServer{store: fixture}
	bucket := uuid.New()
	spec := capabilityRunSpec{identity: auth.RuntimeIdentity{AccountID: uuid.New(), AppID: uuid.New(), TokenID: uuid.New()}, bucketID: &bucket}
	// Every invocation must perform a fresh check rather than caching an earlier success.
	for i := 0; i < 2; i++ {
		if err := server.authorizeRuntimeBucket(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
	}
	if fixture.checks != 2 || fixture.selected != bucket {
		t.Fatal("authorization was not repeated for exact bucket")
	}
	fixture.allowed = false
	if err := server.authorizeRuntimeBucket(context.Background(), spec); err == nil || err.status != http.StatusForbidden {
		t.Fatalf("denial=%v", err)
	}
	fixture.err = errors.New("offline")
	if err := server.authorizeRuntimeBucket(context.Background(), spec); err == nil || err.status != http.StatusServiceUnavailable {
		t.Fatalf("outage=%v", err)
	}
	spec.bucketID = nil
	if err := server.authorizeRuntimeBucket(context.Background(), spec); err != nil {
		t.Fatal("static routing changed")
	}
}

type runtimeBucketRouteFixture struct {
	*capabilityRouteStore
	allowed bool
	checks  int
}

// AuthorizeUnifiedAppRuntimeBucket lets route tests observe denial before any durable acceptance.
func (f *runtimeBucketRouteFixture) AuthorizeUnifiedAppRuntimeBucket(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) {
	f.checks++
	return f.allowed, nil
}

// TestRuntimeBucketRESTDenialPrecedesAcceptance rejects explicit selection before worker or asynchronous reservation.
func TestRuntimeBucketRESTDenialPrecedesAcceptance(t *testing.T) {
	runtime := &restRuntimeTestDouble{}
	_, base, familyID := newCapabilityRouteFixture(runtime, nil)
	fixture := &runtimeBucketRouteFixture{capabilityRouteStore: base}
	base.server.store = fixture
	body := `{"operation":"execute","input":{"value":"test"},"bucket_id":"` + uuid.NewString() + `"}`
	response := performRESTExecution(t, base.server, familyID, "fsk_test", body, "")
	// Denial is observable as HTTP 403 and cannot allocate a result or connect to a provider.
	if response.Code != http.StatusForbidden || fixture.checks != 1 || len(base.records) != 0 || runtime.connects != 0 {
		t.Fatalf("status=%d checks=%d records=%d connects=%d", response.Code, fixture.checks, len(base.records), runtime.connects)
	}
	fixture.allowed = true
	selected := uuid.New()
	spec := capabilityRunSpec{identity: auth.RuntimeIdentity{AccountID: uuid.New(), AppID: base.bundle.AppID, TokenID: uuid.New()}, version: "1.0.0", bundle: &base.bundle, manifest: &unifiedAppManifest{}, input: []byte(`{}`), mode: "live", bucketID: &selected}
	admitted, requestErr := base.server.admitCapabilityRun(context.Background(), spec)
	if requestErr != nil || !admitted.created || base.record.BucketID == nil || *base.record.BucketID != selected {
		t.Fatalf("admission=%v error=%v", admitted.created, requestErr)
	}
}

// TestRuntimeBucketRawRESTRejectsSelector prevents the new envelope field from silently falling through physical routing.
func TestRuntimeBucketRawRESTRejectsSelector(t *testing.T) {
	runtime := &restRuntimeTestDouble{physicalFound: true}
	_, base, familyID := newCapabilityRouteFixture(runtime, nil)
	body := `{"operation":"issues.get","input":{},"bucket_id":"` + uuid.NewString() + `"}`
	response := performRESTExecution(t, base.server, familyID, "fsk_test", body, "")
	// A caller must never believe a selected bucket was applied when the operation does not support it.
	if response.Code != http.StatusBadRequest || runtime.connects != 0 {
		t.Fatalf("status=%d connects=%d", response.Code, runtime.connects)
	}
}

type runtimeBucketSchemaFixture struct {
	store.UnifiedAppBundleStore
	buckets []store.Bucket
}

// ReadUnifiedAppRuntimeBuckets supplies immutable public scope without touching credentials.
func (f runtimeBucketSchemaFixture) ReadUnifiedAppRuntimeBuckets(context.Context, uuid.UUID, uuid.UUID) ([]store.Bucket, error) {
	return f.buckets, nil
}

// TestRuntimeBucketOpenAPIScope keeps optional routing discoverable only for explicitly opted-in versions.
func TestRuntimeBucketOpenAPIScope(t *testing.T) {
	bucket := store.Bucket{ID: uuid.New(), Name: "tenant"}
	fixture := runtimeBucketSchemaFixture{buckets: []store.Bucket{bucket}}
	properties, err := unifiedAppRequestProperties(context.Background(), fixture, &store.App{AccountID: uuid.New(), AppID: uuid.New()}, map[string]any{"type": "object"})
	if err != nil {
		t.Fatal(err)
	}
	schema := properties["bucket_id"].(map[string]any)
	// Clients receive exact permitted IDs and non-secret display labels, never wildcard schemas.
	if schema["enum"].([]string)[0] != bucket.ID.String() || schema["x-fused-bucket-names"].(map[string]string)[bucket.ID.String()] != "tenant" {
		t.Fatal("public bucket scope changed")
	}
	fixture.buckets = nil
	properties, err = unifiedAppRequestProperties(context.Background(), fixture, &store.App{}, map[string]any{})
	if err != nil || properties["bucket_id"] != nil {
		t.Fatal("static app acquired runtime bucket field")
	}
}

// TestRuntimeBucketVersionFence rejects a promotion race before invoking different code or resolving credentials.
func TestRuntimeBucketVersionFence(t *testing.T) {
	runtime := &restRuntimeTestDouble{}
	_, base, familyID := newCapabilityRouteFixture(runtime, nil)
	body := `{"operation":"execute","input":{},"expected_app_id":"` + uuid.NewString() + `"}`
	response := performRESTExecution(t, base.server, familyID, "fsk_test", body, "")
	// A caller's reviewed version is enforced even when its bucket remains allowed by a successor.
	if response.Code != http.StatusConflict || runtime.connects != 0 || len(base.records) != 0 {
		t.Fatalf("status=%d connects=%d records=%d", response.Code, runtime.connects, len(base.records))
	}
	if err := validateUnifiedAppExpectedVersion(&base.bundle.AppID, base.bundle.AppID); err != nil {
		t.Fatal(err)
	}
}

// TestRuntimeBucketRESTInvalidIdentity prevents explicit invalid controls from reverting to static routing.
func TestRuntimeBucketRESTInvalidIdentity(t *testing.T) {
	for _, field := range []string{"bucket_id", "expected_app_id", "BUCKET_ID"} {
		for _, value := range []string{"null", `""`, `"` + uuid.Nil.String() + `"`, `"tenant-name"`} {
			runtime := &restRuntimeTestDouble{}
			_, base, familyID := newCapabilityRouteFixture(runtime, nil)
			body := `{"operation":"execute","input":{},"` + field + `":` + value + `}`
			response := performRESTExecution(t, base.server, familyID, "fsk_test", body, "")
			// Invalid caller routing cannot reserve a result or start provider-capable work.
			if response.Code != http.StatusBadRequest || runtime.connects != 0 || len(base.records) != 0 {
				t.Fatalf("field=%s value=%s status=%d connects=%d records=%d", field, value, response.Code, runtime.connects, len(base.records))
			}
		}
	}
}

// TestRuntimeBucketAsyncDenial rejects revoked authority before returning a durable asynchronous handle.
func TestRuntimeBucketAsyncDenial(t *testing.T) {
	runtime := &restRuntimeTestDouble{}
	router, base, familyID := newCapabilityRouteFixture(runtime, nil)
	fixture := &runtimeBucketRouteFixture{capabilityRouteStore: base}
	base.server.store = fixture
	body := `{"operation":"execute","input":{},"bucket_id":"` + uuid.NewString() + `"}`
	request := httptest.NewRequest(http.MethodPost, "/v1/apps/"+familyID.String()+"/executions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer fsk_test")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Prefer", "respond-async")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	// Asynchronous requests share the same uncached admission boundary as synchronous requests.
	if response.Code != http.StatusForbidden || fixture.checks != 1 || len(base.records) != 0 || runtime.connects != 0 {
		t.Fatalf("status=%d checks=%d records=%d connects=%d", response.Code, fixture.checks, len(base.records), runtime.connects)
	}
}
