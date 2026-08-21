package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeExecuteStream is a minimal EngineService_ExecuteServer that captures
// sent frames without a live gRPC connection, keeping the test hermetic.
type fakeExecuteStream struct {
	enginev1.EngineService_ExecuteServer // embed to satisfy interface
	sent                                 []*enginev1.ExecuteResponse
	ctx                                  context.Context
	header                               metadata.MD
}

func (f *fakeExecuteStream) Send(r *enginev1.ExecuteResponse) error {
	f.sent = append(f.sent, r)
	return nil
}

func (f *fakeExecuteStream) Context() context.Context {
	return f.ctx
}

func (f *fakeExecuteStream) SetHeader(metadata.MD) error { return nil }
func (f *fakeExecuteStream) SendHeader(header metadata.MD) error {
	f.header = header.Copy()
	return nil
}
func (f *fakeExecuteStream) SetTrailer(metadata.MD) {}
func (f *fakeExecuteStream) RecvMsg(any) error      { return nil }
func (f *fakeExecuteStream) SendMsg(any) error      { return nil }

// --- Task 2.3 ---

// TestExecute_EndpointNamePropagated asserts that the gRPC Execute handler
// extracts req.EndpointName and forwards it verbatim to EngineStreamExecuteFunc.
// This is the critical contract: the wire field "endpoint_name" (not "tool_name")
// must survive the proto → handler → dispatch boundary intact.
func TestExecute_EndpointNamePropagated(t *testing.T) {
	const wantEndpoint = "list_customers"

	// Capture the endpointName that reaches the execution entrypoint.
	var gotEndpoint string
	orig := EngineStreamExecuteFunc
	t.Cleanup(func() { EngineStreamExecuteFunc = orig })
	EngineStreamExecuteFunc = func(
		_ context.Context, _, _, endpointName string,
		_ map[string]any, _ map[string]any, _ string, stream engine.ResponseStream,
	) error {
		gotEndpoint = endpointName
		_ = engine.SendResponseContract(stream, 202, "tenant/private+json")
		_ = engine.SendResponseStatus(stream, 202)
		return nil
	}

	params, _ := json.Marshal(map[string]any{"page": 1})
	req := &enginev1.ExecuteRequest{
		AppId:        "test-sdk-id",
		Token:        "tok",
		EndpointName: wantEndpoint, // must use EndpointName, NOT a ToolName field
		Params:       params,
	}

	srv := NewEngineGRPCServer()
	stream := &fakeExecuteStream{ctx: context.Background()}

	if err := srv.Execute(req, stream); err != nil {
		t.Fatalf("Execute returned unexpected error: %v", err)
	}

	if gotEndpoint != wantEndpoint {
		t.Errorf("endpointName propagation: got %q, want %q", gotEndpoint, wantEndpoint)
	}
	if len(stream.sent) != 1 || stream.sent[0].StatusCode != 202 {
		t.Fatalf("provider status frame was not propagated: %#v", stream.sent)
	}
	if got := stream.header.Get("fused-response-status"); len(got) != 1 || got[0] != "202" {
		t.Fatalf("response status metadata = %#v", stream.header)
	}
	if got := stream.header.Get("fused-response-media-family"); len(got) != 1 || got[0] != "unknown" {
		t.Fatalf("response media metadata was not bounded: %#v", stream.header)
	}
}

// TestExecute_InvalidPaginationIntentStopsBeforeExecution proves malformed caller controls cannot reach resolution or provider dispatch.
func TestExecute_InvalidPaginationIntentStopsBeforeExecution(t *testing.T) {
	calls := 0
	orig := EngineStreamExecuteFunc
	t.Cleanup(func() { EngineStreamExecuteFunc = orig })
	EngineStreamExecuteFunc = func(
		_ context.Context, _, _, _ string,
		_ map[string]any, _ map[string]any, _ string, _ engine.ResponseStream,
	) error {
		calls++
		return nil
	}

	req := &enginev1.ExecuteRequest{
		AppId:        "sdk",
		EndpointName: "items.list",
		Pagination:   &enginev1.PaginationIntent{MaxPages: 0},
	}
	err := NewEngineGRPCServer().Execute(req, &fakeExecuteStream{ctx: context.Background()})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Execute() code = %s, want %s: %v", status.Code(err), codes.InvalidArgument, err)
	}
	if calls != 0 {
		t.Fatalf("execution entrypoint calls = %d, want 0", calls)
	}
}

// TestExecute_PaginationIntentBindsContextAndReplayHash proves direct gRPC requests carry one bounded intent into the runtime identity.
func TestExecute_PaginationIntentBindsContextAndReplayHash(t *testing.T) {
	var capturedIntent engine.PaginationIntent
	var capturedIntentFound bool
	var capturedHash string
	orig := EngineStreamExecuteFunc
	t.Cleanup(func() { EngineStreamExecuteFunc = orig })
	EngineStreamExecuteFunc = func(
		ctx context.Context, _, _, _ string,
		_ map[string]any, _ map[string]any, _ string, _ engine.ResponseStream,
	) error {
		capturedIntent, capturedIntentFound = engine.PaginationIntentFromContext(ctx)
		capturedHash = requestBodyHashFromContext(ctx)
		return nil
	}

	req := &enginev1.ExecuteRequest{
		AppId:           "sdk",
		EndpointName:    "items.list",
		RequestBodyHash: "base-hash",
		Pagination:      &enginev1.PaginationIntent{MaxPages: 1},
	}
	err := NewEngineGRPCServer().Execute(req, &fakeExecuteStream{ctx: context.Background()})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !capturedIntentFound || capturedIntent.MaxPages != 1 {
		t.Fatalf("pagination intent = %+v, found %t", capturedIntent, capturedIntentFound)
	}
	wantHash := engine.BindPaginationIntentRequestHash("base-hash", &engine.PaginationIntent{MaxPages: 1})
	if capturedHash != wantHash {
		t.Fatalf("request hash = %q, want %q", capturedHash, wantHash)
	}
}

// TestExecute_NoToolNameField verifies that the ExecuteRequest struct has no
// ToolName field — ensuring the proto rename is complete at the Go type level.
// If this test compiles, the rename is correct; if someone re-adds ToolName, it
// won't compile and the test must be updated.
func TestExecute_ProtoFieldIsEndpointName(t *testing.T) {
	req := &enginev1.ExecuteRequest{
		EndpointName: "my_endpoint",
	}
	// Reflectively confirm the JSON tag uses endpoint_name, not tool_name.
	// We marshal and confirm the key is "endpoint_name".
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if _, ok := m["endpoint_name"]; !ok {
		t.Errorf("JSON key 'endpoint_name' missing — got keys: %v", m)
	}
	if _, ok := m["tool_name"]; ok {
		t.Errorf("stale JSON key 'tool_name' still present; rename is incomplete")
	}
}

// TestExecute_CredentialPassthrough ensures credentials map from the proto
// request reach the execution function unchanged.
func TestExecute_CredentialPassthrough(t *testing.T) {
	wantCreds := map[string]string{"Authorization": "Bearer secret"}

	var gotCreds map[string]any
	orig := EngineStreamExecuteFunc
	t.Cleanup(func() { EngineStreamExecuteFunc = orig })
	EngineStreamExecuteFunc = func(
		_ context.Context, _, _, _ string,
		_ map[string]any, creds map[string]any, _ string, _ engine.ResponseStream,
	) error {
		gotCreds = creds
		return nil
	}

	req := &enginev1.ExecuteRequest{
		AppId:        "sdk",
		EndpointName: "ep",
		Credentials:  wantCreds,
	}

	srv := NewEngineGRPCServer()
	stream := &fakeExecuteStream{ctx: context.Background()}

	if err := srv.Execute(req, stream); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for k, v := range wantCreds {
		if gotCreds[k] != v {
			t.Errorf("credential %q: got %v, want %v", k, gotCreds[k], v)
		}
	}
}

func TestExecute_RuntimeEnvironmentPropagated(t *testing.T) {
	const wantEnvironment = "sandbox"

	var gotEnvironment string
	orig := EngineStreamExecuteFunc
	t.Cleanup(func() { EngineStreamExecuteFunc = orig })
	EngineStreamExecuteFunc = func(
		_ context.Context, _, _, _ string,
		_ map[string]any, _ map[string]any, environment string, _ engine.ResponseStream,
	) error {
		gotEnvironment = environment
		return nil
	}

	req := &enginev1.ExecuteRequest{
		AppId:        "sdk",
		EndpointName: "ep",
		Environment:  wantEnvironment,
	}

	srv := NewEngineGRPCServer()
	stream := &fakeExecuteStream{ctx: context.Background()}

	if err := srv.Execute(req, stream); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotEnvironment != wantEnvironment {
		t.Fatalf("environment propagation: got %q, want %q", gotEnvironment, wantEnvironment)
	}
}

func TestExecute_IdempotencyKeyPropagatedInContext(t *testing.T) {
	const wantKey = "idem-test-123"
	const wantHash = "abc123"

	var gotKey string
	var gotHash string
	orig := EngineStreamExecuteFunc
	t.Cleanup(func() { EngineStreamExecuteFunc = orig })
	EngineStreamExecuteFunc = func(
		ctx context.Context, _, _, _ string,
		_ map[string]any, _ map[string]any, _ string, _ engine.ResponseStream,
	) error {
		gotKey = idempotencyKeyFromContext(ctx)
		gotHash = requestBodyHashFromContext(ctx)
		return nil
	}

	req := &enginev1.ExecuteRequest{
		AppId:           "sdk",
		EndpointName:    "ep",
		IdempotencyKey:  wantKey,
		RequestBodyHash: wantHash,
	}

	stream := &fakeExecuteStream{ctx: context.Background()}
	if err := NewEngineGRPCServer().Execute(req, stream); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotKey != wantKey {
		t.Fatalf("idempotency key propagation: got %q, want %q", gotKey, wantKey)
	}
	if gotHash != wantHash {
		t.Fatalf("request body hash propagation: got %q, want %q", gotHash, wantHash)
	}
}

func TestExecute_ProvidesRuntimeTimingContext(t *testing.T) {
	var sawTimings bool
	orig := EngineStreamExecuteFunc
	t.Cleanup(func() { EngineStreamExecuteFunc = orig })
	EngineStreamExecuteFunc = func(
		ctx context.Context, _, _, _ string,
		_ map[string]any, _ map[string]any, _ string, _ engine.ResponseStream,
	) error {
		_, sawTimings = engine.ExecutionTimingsFromContext(ctx)
		engine.RecordExecutionTiming(ctx, "provider_total", 12*time.Millisecond)
		return nil
	}

	req := &enginev1.ExecuteRequest{
		AppId:        "sdk",
		EndpointName: "ep",
		Params:       []byte(`{"ok":true}`),
	}
	stream := &fakeExecuteStream{ctx: context.Background()}

	if err := NewEngineGRPCServer().Execute(req, stream); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sawTimings {
		t.Fatal("expected Execute to provide runtime timing context")
	}
}

func TestParamsWithExecutionHeadersAddsRequestIdentity(t *testing.T) {
	params := paramsWithExecutionHeaders(map[string]any{
		"_headers": map[string]string{"idempotency-key": "caller-key"},
	}, "generated-key", "hash-123")

	headers := params["_headers"].(map[string]any)
	if headers["idempotency-key"] != "caller-key" {
		t.Fatalf("existing idempotency header should win, got %v", headers)
	}
	if headers[requestBodyHashHeaderName] != "hash-123" {
		t.Fatalf("request body hash header missing: %v", headers)
	}
}

func TestExecute_RuntimeEnvironmentErrorsAreStructured(t *testing.T) {
	orig := EngineStreamExecuteFunc
	t.Cleanup(func() { EngineStreamExecuteFunc = orig })
	EngineStreamExecuteFunc = func(
		_ context.Context, _, _, _ string,
		_ map[string]any, _ map[string]any, _ string, _ engine.ResponseStream,
	) error {
		return &EnvironmentNotSupportedError{
			Code:      "environment_not_supported",
			Requested: "production",
			Available: []string{"prod", "sandbox"},
		}
	}

	req := &enginev1.ExecuteRequest{AppId: "sdk", EndpointName: "ep", Environment: "production"}
	stream := &fakeExecuteStream{ctx: context.Background()}

	if err := NewEngineGRPCServer().Execute(req, stream); err != nil {
		t.Fatalf("unexpected Execute transport error: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected one error frame, got %d", len(stream.sent))
	}
	var payload struct {
		Code      string   `json:"code"`
		Requested string   `json:"requested"`
		Available []string `json:"available"`
	}
	if err := json.Unmarshal([]byte(stream.sent[0].Error), &payload); err != nil {
		t.Fatalf("error frame was not JSON: %q", stream.sent[0].Error)
	}
	if payload.Code != "environment_not_supported" || payload.Requested != "production" || len(payload.Available) != 2 || payload.Available[0] != "prod" {
		t.Fatalf("unexpected structured error: %+v", payload)
	}
}

func TestExecute_OrdinaryErrorsRemainStrings(t *testing.T) {
	orig := EngineStreamExecuteFunc
	t.Cleanup(func() { EngineStreamExecuteFunc = orig })
	EngineStreamExecuteFunc = func(
		_ context.Context, _, _, _ string,
		_ map[string]any, _ map[string]any, _ string, _ engine.ResponseStream,
	) error {
		return fmt.Errorf("ordinary failure")
	}

	stream := &fakeExecuteStream{ctx: context.Background()}
	if err := NewEngineGRPCServer().Execute(&enginev1.ExecuteRequest{AppId: "sdk", EndpointName: "ep"}, stream); err != nil {
		t.Fatalf("unexpected Execute transport error: %v", err)
	}
	if stream.sent[0].Error != "ordinary failure" {
		t.Fatalf("ordinary error changed shape: %q", stream.sent[0].Error)
	}
}

func TestConnect_NilCacheFails(t *testing.T) {
	// When InitSandbox hasn't been called the global cache is nil; Connect must
	// surface a clear error rather than panicking.
	orig := globalObjectCache
	t.Cleanup(func() { globalObjectCache = orig })
	globalObjectCache = nil

	srv := NewEngineGRPCServer()
	_, err := srv.Connect(context.Background(), &enginev1.ConnectRequest{
		AppId: "test",
		Token: "tok",
	})
	if err == nil {
		t.Fatal("expected error when cache is nil, got nil")
	}
}

func TestConnect_InvalidOrInactiveSDKReturnsFriendlyUnauthenticatedStatus(t *testing.T) {
	originalCache, originalValidator := globalObjectCache, globalTokenValidator
	t.Cleanup(func() {
		globalObjectCache, globalTokenValidator = originalCache, originalValidator
	})
	globalObjectCache = &richMockCache{}
	globalTokenValidator = &mockTokenValidator{validToken: "active-token", accountID: uuid.New()}

	appID := uuid.NewString()
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-app-id", appID,
		"x-api-key", "revoked-or-inactive-token",
	))
	_, err := NewEngineGRPCServer().Connect(ctx, &enginev1.ConnectRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Connect error = %v, want Unauthenticated", err)
	}
	if got := status.Convert(err).Message(); got != "SDK authentication failed; check the token and confirm this SDK version is active" {
		t.Fatalf("Connect message = %q", got)
	}
}

func TestConnectDoesNotForwardExecutionTokenToCache(t *testing.T) {
	originalCache, originalValidator := globalObjectCache, globalTokenValidator
	t.Cleanup(func() {
		globalObjectCache, globalTokenValidator = originalCache, originalValidator
	})
	cache := &recordingCache{}
	globalObjectCache = cache
	globalTokenValidator = &mockTokenValidator{validToken: "family-token", accountID: uuid.New()}

	appID := uuid.NewString()
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-app-id", appID,
		"x-api-key", "family-token",
	))
	if _, err := NewEngineGRPCServer().Connect(ctx, &enginev1.ConnectRequest{}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if cache.connectedID != appID {
		t.Fatalf("connected app = %q, want %q", cache.connectedID, appID)
	}
	if cache.connectedContext != ctx {
		t.Fatal("Connect derived a cache context instead of forwarding the credential-free request context")
	}
}
