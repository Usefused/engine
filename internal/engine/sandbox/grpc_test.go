package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"google.golang.org/grpc/metadata"
)

// fakeExecuteStream is a minimal EngineService_ExecuteServer that captures
// sent frames without a live gRPC connection, keeping the test hermetic.
type fakeExecuteStream struct {
	enginev1.EngineService_ExecuteServer // embed to satisfy interface
	sent                                 []*enginev1.ExecuteResponse
	ctx                                  context.Context
}

func (f *fakeExecuteStream) Send(r *enginev1.ExecuteResponse) error {
	f.sent = append(f.sent, r)
	return nil
}

func (f *fakeExecuteStream) Context() context.Context {
	return f.ctx
}

func (f *fakeExecuteStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeExecuteStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeExecuteStream) SetTrailer(metadata.MD)       {}
func (f *fakeExecuteStream) RecvMsg(any) error            { return nil }
func (f *fakeExecuteStream) SendMsg(any) error            { return nil }

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
		_ = engine.SendResponseStatus(stream, 202)
		return nil
	}

	params, _ := json.Marshal(map[string]any{"page": 1})
	req := &enginev1.ExecuteRequest{
		ArtifactId:   "test-sdk-id",
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
		ArtifactId:   "sdk",
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
		ArtifactId:   "sdk",
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
		ArtifactId:      "sdk",
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
		ArtifactId:   "sdk",
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

	req := &enginev1.ExecuteRequest{ArtifactId: "sdk", EndpointName: "ep", Environment: "production"}
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
	if err := NewEngineGRPCServer().Execute(&enginev1.ExecuteRequest{ArtifactId: "sdk", EndpointName: "ep"}, stream); err != nil {
		t.Fatalf("unexpected Execute transport error: %v", err)
	}
	if stream.sent[0].Error != "ordinary failure" {
		t.Fatalf("ordinary error changed shape: %q", stream.sent[0].Error)
	}
}

// TestConnect_PropagatesToken confirms Connect wires the token into the context
// passed to ConnectSDK so downstream registry calls authenticate as the user.
func TestConnect_NilCacheFails(t *testing.T) {
	// When InitSandbox hasn't been called the global cache is nil; Connect must
	// surface a clear error rather than panicking.
	orig := globalObjectCache
	t.Cleanup(func() { globalObjectCache = orig })
	globalObjectCache = nil

	srv := NewEngineGRPCServer()
	_, err := srv.Connect(context.Background(), &enginev1.ConnectRequest{
		ArtifactId: "test",
		Token:      "tok",
	})
	if err == nil {
		t.Fatal("expected error when cache is nil, got nil")
	}
}
