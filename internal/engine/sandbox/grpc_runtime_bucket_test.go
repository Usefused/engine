package sandbox

import (
	"context"
	"testing"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TestExecuteRuntimeBucketBindsContextAndReplay proves routing cannot be forged through a caller hash or shared request context.
func TestExecuteRuntimeBucketBindsContextAndReplay(t *testing.T) {
	appID, bucketID := uuid.New(), uuid.New()
	original := EngineStreamExecuteFunc
	t.Cleanup(func() { EngineStreamExecuteFunc = original })
	var selected unifiedAppBucket
	var requestHash string
	// Capture the already-validated routing boundary without contacting a provider.
	EngineStreamExecuteFunc = func(ctx context.Context, _, _, _ string, _, _ map[string]any, _ string, _ engine.ResponseStream) error {
		selected, _ = ctx.Value(unifiedAppBucketKey{}).(unifiedAppBucket)
		requestHash = requestBodyHashFromContext(ctx)
		return nil
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-app-id", appID.String(), "x-api-key", "fixture"))
	req := &enginev1.ExecuteRequest{EndpointName: "read", BucketId: bucketID.String(), RequestBodyHash: "same-input"}
	// One request gets one opaque bucket bound to its authenticated app identity.
	if err := NewEngineGRPCServer().Execute(req, &fakeExecuteStream{ctx: ctx}); err != nil {
		t.Fatal(err)
	}
	if selected.appID != appID || selected.bucketID == nil || *selected.bucketID != bucketID || requestHash == "same-input" {
		t.Fatal("runtime bucket was not bound to request identity")
	}
	firstHash := requestHash
	req.BucketId = uuid.NewString()
	// Reusing an idempotency key across bucket selections cannot reuse the first response identity.
	if err := NewEngineGRPCServer().Execute(req, &fakeExecuteStream{ctx: ctx}); err != nil || firstHash == requestHash {
		t.Fatal("bucket selection did not change replay identity")
	}
	// The caller's base context remains unchanged for concurrent requests.
	if _, exists := ctx.Value(unifiedAppBucketKey{}).(unifiedAppBucket); exists {
		t.Fatal("request routing escaped its local context")
	}
}

// TestExecuteInvalidRuntimeBucketFailsBeforeDispatch prevents malformed selections from silently using default credentials.
func TestExecuteInvalidRuntimeBucketFailsBeforeDispatch(t *testing.T) {
	original := EngineStreamExecuteFunc
	t.Cleanup(func() { EngineStreamExecuteFunc = original })
	calls := 0
	// Any execution call would mean malformed routing crossed admission.
	EngineStreamExecuteFunc = func(context.Context, string, string, string, map[string]any, map[string]any, string, engine.ResponseStream) error {
		calls++
		return nil
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-app-id", uuid.NewString()))
	for _, bucket := range []string{"not-a-uuid", uuid.Nil.String(), " " + uuid.NewString(), "urn:uuid:" + uuid.NewString()} {
		err := NewEngineGRPCServer().Execute(&enginev1.ExecuteRequest{BucketId: bucket}, &fakeExecuteStream{ctx: ctx})
		// Malformed inputs must fail before execution and retain no fallback path.
		if status.Code(err) != codes.InvalidArgument || calls != 0 {
			t.Fatalf("invalid bucket admitted: code=%v calls=%d", status.Code(err), calls)
		}
	}
}

// TestRuntimeBucketBeforeReplayChecksLiveAuthority covers revocation and unavailable resolver behavior before response replay.
func TestRuntimeBucketBeforeReplayChecksLiveAuthority(t *testing.T) {
	original := globalSecretResolver
	t.Cleanup(func() { globalSecretResolver = original })
	app, bucket := uuid.New(), uuid.New()
	identity := auth.RuntimeIdentity{AppID: app, AccountID: uuid.New(), TokenID: uuid.New()}
	ctx := WithUnifiedAppBucket(context.Background(), app, &bucket)
	fixture := &runtimeBucketResolverFixture{allowed: true}
	globalSecretResolver = NewSecretResolver(fixture, nil)
	// Every request must perform a live check even if a prior response exists.
	if err := authorizeRuntimeBucketBeforeReplay(ctx, identity); err != nil {
		t.Fatal(err)
	}
	fixture.allowed = false
	if err := authorizeRuntimeBucketBeforeReplay(ctx, identity); err == nil || fixture.checks != 2 {
		t.Fatal("revoked bucket authority could replay a response")
	}
	globalSecretResolver = nil
	// Missing resolver support is a denial for explicit selection, never a passthrough fallback.
	if err := authorizeRuntimeBucketBeforeReplay(ctx, identity); err == nil {
		t.Fatal("missing resolver admitted explicit routing")
	}
	if err := authorizeRuntimeBucketBeforeReplay(context.Background(), identity); err != nil {
		t.Fatal("ordinary static routing changed")
	}
}
