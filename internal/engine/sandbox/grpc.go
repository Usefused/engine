package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type EngineGRPCServer struct {
	enginev1.UnimplementedEngineServiceServer
}

func NewEngineGRPCServer() *EngineGRPCServer {
	return &EngineGRPCServer{}
}

func authFromIncomingContext(ctx context.Context) (appID, token string) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", ""
	}
	if vals := md.Get("x-app-id"); len(vals) > 0 {
		appID = vals[0]
	}
	if vals := md.Get("x-api-key"); len(vals) > 0 {
		token = vals[0]
	}
	return appID, token
}

func (s *EngineGRPCServer) Connect(ctx context.Context, req *enginev1.ConnectRequest) (*enginev1.ConnectResponse, error) {
	// Guard: the cache is wired by InitSandbox; a nil here means the Engine booted wrong.
	if globalObjectCache == nil {
		return nil, status.Error(codes.Internal, "Engine runtime is unavailable")
	}
	appID, token := authFromIncomingContext(ctx)

	uid, err := uuid.Parse(appID)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "SDK app ID must be a valid UUID")
	}
	if globalTokenValidator != nil {
		if _, err := globalTokenValidator.Validate(ctx, uid, token); err != nil {
			return nil, status.Error(codes.Unauthenticated, "SDK authentication failed; check the token and confirm this SDK version is active")
		}
	}

	// Inject the user's token into the context so RegistryClient can authenticate as the user
	ctx = context.WithValue(ctx, "sdk-token", token)
	if err := globalObjectCache.ConnectSDK(ctx, appID); err != nil {
		return nil, status.Error(codes.FailedPrecondition, "SDK version is not available on this Engine; reapply its configuration")
	}
	return &enginev1.ConnectResponse{}, nil
}

// Disconnect releases this SDK's cache references so refcounts decrement and an
// object is evicted once no live connection references it (AD-7). The SDK calls
// this when it closes its channel.
func (s *EngineGRPCServer) Disconnect(ctx context.Context, req *enginev1.DisconnectRequest) (*enginev1.DisconnectResponse, error) {
	// No-op if the cache isn't wired — disconnect must never fail the caller.
	if globalObjectCache != nil {
		appID, token := authFromIncomingContext(ctx)
		if uid, err := uuid.Parse(appID); err == nil {
			if globalTokenValidator == nil {
				globalObjectCache.DisconnectSDK(appID)
			} else if _, err := globalTokenValidator.Validate(ctx, uid, token); err == nil {
				globalObjectCache.DisconnectSDK(appID)
			}
		}
	}
	return &enginev1.DisconnectResponse{}, nil
}

// retryOverrideFromRequest converts the wire-level RetryOverride (proto
// int32 pointers) to the dispatcher's engine.RetryOverride (int pointers). A
// nil or empty proto message yields a nil override, which
// engine.ContextWithRetryOverride treats as a no-op.
func retryOverrideFromRequest(o *enginev1.RetryOverride) *engine.RetryOverride {
	if o == nil {
		return nil
	}
	out := &engine.RetryOverride{}
	if o.MaxRetries != nil {
		v := int(*o.MaxRetries)
		out.MaxRetries = &v
	}
	if o.BackoffMs != nil {
		v := int(*o.BackoffMs)
		out.BackoffMs = &v
	}
	if out.MaxRetries == nil && out.BackoffMs == nil {
		return nil
	}
	return out
}

// grpcResponseStream adapts the gRPC server stream to the dispatcher's
// engine.ResponseStream interface, so vendor response chunks (including SSE and
// pagination pages) relay straight back on the open stream.
type grpcResponseStream struct {
	stream enginev1.EngineService_ExecuteServer
}

func (g *grpcResponseStream) Send(chunk []byte) error {
	return g.stream.Send(&enginev1.ExecuteResponse{Result: chunk})
}

func (g *grpcResponseStream) SendStatus(status int) error {
	return g.stream.Send(&enginev1.ExecuteResponse{StatusCode: int32(status)})
}

func (s *EngineGRPCServer) Execute(req *enginev1.ExecuteRequest, stream enginev1.EngineService_ExecuteServer) error {
	requestStarted := time.Now()
	timings := engine.NewExecutionTimings()
	ctx := engine.ContextWithExecutionTimings(stream.Context(), timings)
	ctx = contextWithExecutionIdentity(ctx, req.IdempotencyKey, req.RequestBodyHash)
	ctx = contextWithExecutionTransport(ctx, models.EngineExecutionTransportSDK)
	ctx = engine.ContextWithRetryOverride(ctx, retryOverrideFromRequest(req.RetryOverride))
	ctx = engine.ContextWithIdempotencyKeyPresent(ctx, strings.TrimSpace(req.IdempotencyKey) != "")

	decodeStarted := time.Now()
	appID, token := authFromIncomingContext(ctx)

	var params map[string]any
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			recordExecuteTimings(ctx, timings, requestStarted, appID, req.EndpointName)
			return fmt.Errorf("invalid json params: %w", err)
		}
	} else {
		params = make(map[string]any)
	}

	// Passthrough credentials arrive per-request on the wire; convert the typed
	// proto map to the dispatcher's map[string]any and use in-flight only.
	credentials := make(map[string]any, len(req.Credentials))
	for k, v := range req.Credentials {
		credentials[k] = v
	}
	engine.MeasureExecutionTiming(ctx, "request_decode", decodeStarted)

	// Real streaming dispatch: scope-check + vendor call, chunks relayed as they
	// arrive. Errors surface as a single ExecuteResponse.Error frame.
	adapter := &grpcResponseStream{stream: stream}
	if err := EngineStreamExecuteFunc(ctx, appID, token, req.EndpointName, params, credentials, req.Environment, adapter); err != nil {
		recordExecuteTimings(ctx, timings, requestStarted, appID, req.EndpointName)
		return stream.Send(&enginev1.ExecuteResponse{Error: encodeRuntimeError(err)})
	}
	recordExecuteTimings(ctx, timings, requestStarted, appID, req.EndpointName)
	return nil
}

func recordExecuteTimings(ctx context.Context, timings *engine.ExecutionTimings, requestStarted time.Time, appID, endpointName string) {
	timings.Record("engine_total", time.Since(requestStarted))
	snapshot := timings.SnapshotMilliseconds()

	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(timings.Attributes()...)
	}

	slog.InfoContext(ctx, "Engine runtime timings",
		slog.String("app.id", appID),
		slog.String("endpoint_name", endpointName),
		slog.Any("timings_ms", snapshot),
	)
}
