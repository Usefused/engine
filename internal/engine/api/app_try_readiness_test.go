package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type tryReadinessFixture struct {
	*appOpenAPITestStore
	bucket        store.Bucket
	runtime       []store.Bucket
	selection     models.SDKSelection
	materialCalls int
	ready         bool
	bindings      map[uuid.UUID]store.AppFamilyBucket
	bound         []store.Bucket
}

// GetAppRuntime supplies the configured bucket without changing any durable bindings.
func (f *tryReadinessFixture) GetAppRuntime(context.Context, uuid.UUID) (*store.AppRuntime, error) {
	return &store.AppRuntime{BucketID: f.bucket.ID}, nil
}

// ListAppFamilyServiceBuckets exposes explicit overrides without expanding runtime scope.
func (f *tryReadinessFixture) ListAppFamilyServiceBuckets(context.Context, uuid.UUID) (map[uuid.UUID]store.AppFamilyBucket, error) {
	return f.bindings, nil
}

// ListBucketsForAppFamily supplies only the app's bound buckets.
func (f *tryReadinessFixture) ListBucketsForAppFamily(context.Context, uuid.UUID) ([]store.Bucket, error) {
	return append([]store.Bucket{f.bucket}, f.bound...), nil
}

// ReadUnifiedAppRuntimeBuckets exposes an exact finite scope for alternate routing tests.
func (f *tryReadinessFixture) ReadUnifiedAppRuntimeBuckets(context.Context, uuid.UUID, uuid.UUID) ([]store.Bucket, error) {
	return f.runtime, nil
}

// ListServiceContractMetadata pins authentication to the deployed service version.
func (f *tryReadinessFixture) ListServiceContractMetadata(context.Context, []store.ServiceContractMetadataRef) (map[store.ServiceContractMetadataRef]*fusedobject.ServiceMetadata, error) {
	ref := store.ServiceContractMetadataRef{ServiceID: f.selection.ServiceID, ServiceVersionID: f.selection.ServiceVersionID}
	return map[store.ServiceContractMetadataRef]*fusedobject.ServiceMetadata{ref: {ID: ref.ServiceID, ServiceVersionID: ref.ServiceVersionID, Name: "Issues", AuthConfigs: fusedobject.AuthConfigs{{Name: "apiKey", Type: "apiKey"}}}}, nil
}

// GetAppBucketCredentialPresence records metadata-only access and returns exact material presence.
func (f *tryReadinessFixture) GetAppBucketCredentialPresence(_ context.Context, _ uuid.UUID, requirements []store.AppCredentialRequirement) ([]store.AppCredentialPresence, error) {
	f.materialCalls++
	// Missing credentials must be distinct from a storage failure or unreadable metadata.
	if !f.ready {
		return nil, nil
	}
	result := []store.AppCredentialPresence{}
	for _, requirement := range requirements {
		result = append(result, store.AppCredentialPresence{ServiceID: requirement.ServiceID, AuthName: requirement.AuthName, AuthType: requirement.AuthType, SecretKeys: requirement.SecretKeys})
	}
	return result, nil
}

// newTryReadinessFixture creates a secured immutable operation and one permitted alternate bucket.
func newTryReadinessFixture(t *testing.T) *tryReadinessFixture {
	t.Helper()
	base, _ := newAppOpenAPIFixture(t)
	selections, err := decodeAppOpenAPISelections(base.app.Selections)
	// Fixture corruption must not make authorization tests accidentally anonymous.
	if err != nil {
		t.Fatal(err)
	}
	base.family.DeliveryMode = store.AppDeliveryModeAPI
	base.matches[0].Endpoint.SecurityRequirements = securedOperation("createIssue", "apiKey").SecurityRequirements
	return &tryReadinessFixture{appOpenAPITestStore: base, selection: selections[0], bucket: store.Bucket{ID: uuid.New(), Name: "Default"}, runtime: []store.Bucket{{ID: uuid.New(), Name: "Sandbox"}}}
}

// TestAppTryReadinessScope proves metadata stays within the selected version, operation and permitted bucket.
func TestAppTryReadinessScope(t *testing.T) {
	f := newTryReadinessFixture(t)
	ctx := controlTestContext(context.Background(), f.app.AccountID)
	actor, _ := accesscontrol.ActorFromContext(ctx)
	result, err := inspectAppTry(ctx, f, f, actor, f.app, "createIssue", "", "")
	// Configured routing must expose the exact operation’s missing credential requirement.
	if err != nil || len(result.Requirements) != 1 || result.Requirements[0].Credentials != "missing" || result.Requirements[0].BucketID != f.bucket.ID {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	f.ready = true
	result, err = inspectAppTry(ctx, f, f, actor, f.app, "createIssue", f.runtime[0].ID.String(), "")
	// An allowed override must inspect that bucket’s credentials.
	if err != nil || result.Requirements[0].Credentials != "ready" || result.Requirements[0].BucketID != f.runtime[0].ID {
		t.Fatalf("alternate=%#v err=%v", result, err)
	}
}

// TestAppTryReadinessRejectsScope prevents unselected buckets and operations probing credential metadata.
func TestAppTryReadinessRejectsScope(t *testing.T) {
	f := newTryReadinessFixture(t)
	ctx := controlTestContext(context.Background(), f.app.AccountID)
	actor, _ := accesscontrol.ActorFromContext(ctx)
	before := f.materialCalls
	_, err := inspectAppTry(ctx, f, f, actor, f.app, "createIssue", uuid.NewString(), "")
	// Authorization failure must precede credential inspection.
	if err == nil || f.materialCalls != before {
		t.Fatal("unapproved bucket reached credential inspection")
	}
	_, err = inspectAppTry(ctx, f, f, actor, f.app, "unknown", "", "")
	// Authorization failure must precede credential inspection.
	if err == nil || f.materialCalls != before {
		t.Fatal("unselected operation reached credential inspection")
	}
}

// TestAppTryReadinessPermissions proves app read does not imply credential metadata authority.
func TestAppTryReadinessPermissions(t *testing.T) {
	f := newTryReadinessFixture(t)
	actor := controlTestOwnerActor(f.app.AccountID)
	snapshot, err := appPermissionTestSnapshot(1, accesscontrol.Grant{Permission: accesscontrol.PermissionAppAPIRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: f.app.AppFamilyID}})
	// Fixture permissions must be valid before testing metadata isolation.
	if err != nil {
		t.Fatal(err)
	}
	actor.Authorization = snapshot
	ctx := accesscontrol.ContextWithActor(context.Background(), actor)
	result, err := inspectAppTry(ctx, f, f, actor, f.app, "createIssue", "", "")
	// Lack of metadata permission must remain unknown without a store read.
	if err != nil || result.Requirements[0].Credentials != "unknown" || f.materialCalls != 0 {
		t.Fatalf("permission result=%#v err=%v calls=%d", result, err, f.materialCalls)
	}
}

// TestAppTryReadinessHTTP keeps the metadata response no-store and rejects anonymous callers before storage.
func TestAppTryReadinessHTTP(t *testing.T) {
	f := newTryReadinessFixture(t)
	router := newControlTestRouter(f.app.AccountID)
	router.Get("/apps/{app_id}/try-readiness", AppTryReadinessHandler(f, f))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/apps/"+f.app.AppID.String()+"/try-readiness?operation=createIssue", nil))
	var result appTryReadiness
	err := json.Unmarshal(response.Body.Bytes(), &result)
	// Successful reads need the complete metadata projection and no-store headers.
	if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" || err != nil || len(result.Requirements) != 1 {
		t.Fatalf("response=%d %s err=%v", response.Code, response.Body.String(), err)
	}
	denied := httptest.NewRecorder()
	AppTryReadinessHandler(f, f).ServeHTTP(denied, httptest.NewRequest(http.MethodGet, "/", nil))
	// Anonymous direct mounts must fail before resolving any app metadata.
	if denied.Code != 401 {
		t.Fatalf("anonymous status=%d", denied.Code)
	}
}

// TestAppTryReadinessConfiguredOverrides keeps per-service bindings distinct from the family default.
func TestAppTryReadinessConfiguredOverrides(t *testing.T) {
	f := newTryReadinessFixture(t)
	override := store.Bucket{ID: uuid.New(), Name: "Service account"}
	f.bindings = map[uuid.UUID]store.AppFamilyBucket{f.selection.ServiceID: {BucketID: override.ID}}
	f.bound = []store.Bucket{override}
	ctx := controlTestContext(context.Background(), f.app.AccountID)
	actor, _ := accesscontrol.ActorFromContext(ctx)
	result, err := inspectAppTry(ctx, f, f, actor, f.app, "createIssue", "", "")
	// Default routing must report the service override that the Engine will actually use.
	if err != nil || len(result.Requirements) != 1 || result.Requirements[0].BucketID != override.ID {
		t.Fatalf("override=%#v error=%v", result, err)
	}
	before := f.materialCalls
	f.bound = nil
	_, err = inspectAppTry(ctx, f, f, actor, f.app, "createIssue", "", "")
	// A dangling override cannot silently inspect the default or zero bucket instead.
	// Authorization failure must precede credential inspection.
	if err == nil || f.materialCalls != before {
		t.Fatal("missing override reached credential inspection")
	}
}
