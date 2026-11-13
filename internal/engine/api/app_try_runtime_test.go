package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

type tryRuntimeStore struct {
	store.Store
	allowed bool
	calls   int
	bucket  uuid.UUID
}

// AuthorizeUnifiedAppRuntimeBucket models live scope revocation without caching the decision.
func (s *tryRuntimeStore) AuthorizeUnifiedAppRuntimeBucket(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ uuid.UUID, bucket uuid.UUID) (bool, error) {
	s.calls++
	s.bucket = bucket
	return s.allowed, nil
}

// TestAppTryRESTBucketDispatch verifies HTTP overrides authorize before provider calls and recheck every request.
func TestAppTryRESTBucketDispatch(t *testing.T) {
	runtime := &restRuntimeTestDouble{physicalFound: true, physicalResult: sandbox.PhysicalExecutionResult{StatusCode: 200, Body: []byte(`{"ok":true}`)}}
	server, appID := newRESTPhysicalServer(runtime)
	repository := &tryRuntimeStore{Store: server.store, allowed: true}
	server.store = repository
	bucket := uuid.New()
	body := fmt.Sprintf(`{"operation":"issues.get","input":{},"bucket_id":%q}`, bucket.String())
	response := performRESTExecution(t, server, appID, "fsk_test", body, "")
	// Exact runtime authority is checked before the first cache connection or provider call.
	if response.Code != 200 || repository.calls != 1 || repository.bucket != bucket || len(runtime.physicalCalls) != 1 {
		t.Fatalf("response=%d %s checks=%d calls=%d", response.Code, response.Body.String(), repository.calls, len(runtime.physicalCalls))
	}
	repository.allowed = false
	denied := performRESTExecution(t, server, appID, "fsk_test", body, "")
	// Revocation must stop another call even when app identity remains unchanged.
	if denied.Code != http.StatusForbidden || repository.calls != 2 || len(runtime.physicalCalls) != 1 {
		t.Fatalf("denied=%d checks=%d calls=%d", denied.Code, repository.calls, len(runtime.physicalCalls))
	}
}

// TestAppTryRESTBucketSchema exposes finite scope only on raw API operation contracts.
func TestAppTryRESTBucketSchema(t *testing.T) {
	bucket := store.Bucket{ID: uuid.New(), Name: "Sandbox"}
	fixture := runtimeBucketSchemaFixture{buckets: []store.Bucket{bucket}}
	export := &appOpenAPIExport{operations: []appOpenAPIOperation{{name: "createIssue", requestSchema: map[string]any{"properties": map[string]any{}}}, {name: "unified_app:child", requestSchema: map[string]any{"properties": map[string]any{}}}}}
	app := &store.App{AppID: uuid.New(), AccountID: uuid.New()}
	err := addAPIRuntimeBucketSchema(context.Background(), fixture, app, &store.AppFamily{Kind: store.AppKindSDK, DeliveryMode: store.AppDeliveryModeAPI}, export)
	// The UI must receive exactly the deployed allowlist, with children retaining independent routing.
	if err != nil || export.operations[0].requestSchema["properties"].(map[string]any)["bucket_id"] == nil || export.operations[1].requestSchema["properties"].(map[string]any)["bucket_id"] != nil {
		t.Fatalf("export=%#v err=%v", export, err)
	}
}
