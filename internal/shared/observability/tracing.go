// Package observability provides a unified tracing abstraction for Fused.
//
// The Thread/Step interfaces are used by both the Engine and Registry to record
// user/agent-triggered executions. The underlying transport is standard OpenTelemetry
// (OTLP over HTTP) so traces integrate with any OTel-compatible backend (Grafana Tempo,
// Honeycomb, Datadog, Jaeger, etc.).
//
// # Design principles
//   - Thread ≈ an OTel trace (one root span per user execution)
//   - Step   ≈ a child span within that trace
//   - Credentials and secrets MUST NEVER appear in span attributes or refs.
//     Callers are responsible for this; the package provides no scrubbing.
//   - When OTEL_EXPORTER_OTLP_ENDPOINT is not set the package falls back to
//     a no-op implementation so the binary always compiles and runs.
package observability

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	threadify "github.com/ThreadifyDev/go-sdk"
)

// Thread represents a user/agent-triggered execution trace.
// Each Thread maps to one root OTel span; Steps are child spans.
type Thread interface {
	// AddRefs attaches safe, non-secret metadata to the root span
	// (e.g. artifact_id, service_id, tool_name). Never add credentials here.
	AddRefs(ctx context.Context, refs map[string]string) Thread
	// Step creates a named child span within this thread.
	Step(name string) Step
	// Complete marks the thread as successfully finished.
	Complete(ctx context.Context, message string)
	// Close marks the thread as cancelled/closed (non-successful termination).
	Close(ctx context.Context, message string)
}

// Step represents a single unit of work within a Thread.
type Step interface {
	// AddContext attaches observable metadata to the step span.
	// Use AddPrivateContext for anything that should not be exported.
	AddContext(data map[string]any) Step
	// AddPrivateContext attaches metadata that stays local (not exported to backend).
	AddPrivateContext(data map[string]any) Step
	// SubStep records an inline sub-operation within this step.
	SubStep(name string, data map[string]any, status ...string) Step
	// Success ends the step span with status OK.
	Success(ctx context.Context)
	// Failed ends the step span with status Error and a message.
	Failed(ctx context.Context, message string)
	// Error ends the step span with status Error from an error value.
	Error(ctx context.Context, err error)
}

// Connection is the factory for Thread instances.
// The real implementation wraps an OTel TracerProvider.
type Connection interface {
	Start(ctx context.Context, name, parentID string, tags ...string) (Thread, error)
	Close()
}

// ─── No-op implementation (used when OTEL_EXPORTER_OTLP_ENDPOINT is unset) ──

type noopStep struct{}

func (n *noopStep) AddContext(_ map[string]any) Step                     { return n }
func (n *noopStep) AddPrivateContext(_ map[string]any) Step              { return n }
func (n *noopStep) SubStep(_ string, _ map[string]any, _ ...string) Step { return n }
func (n *noopStep) Success(_ context.Context)                            {}
func (n *noopStep) Failed(_ context.Context, _ string)                   {}
func (n *noopStep) Error(_ context.Context, _ error)                     {}

type noopThread struct{}

func (n *noopThread) AddRefs(_ context.Context, _ map[string]string) Thread { return n }
func (n *noopThread) Step(_ string) Step                                    { return &noopStep{} }
func (n *noopThread) Complete(_ context.Context, _ string)                  {}
func (n *noopThread) Close(_ context.Context, _ string)                     {}

type noopConnection struct{}

func (n *noopConnection) Start(_ context.Context, _, _ string, _ ...string) (Thread, error) {
	return &noopThread{}, nil
}
func (n *noopConnection) Close() {}

// ─── OTel real implementation ─────────────────────────────────────────────────

// otelConnection wraps an OTel TracerProvider + Tracer as a Connection.
type otelConnection struct {
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
	mu       sync.Mutex
}

// Start opens a new root span as a Thread.
// parentID is an OTel W3C trace-context trace-ID string; if empty the span has
// no parent (i.e. it is a new root trace).
func (c *otelConnection) Start(ctx context.Context, name, parentID string, tags ...string) (Thread, error) {
	attrs := []attribute.KeyValue{
		attribute.StringSlice("fused.tags", tags),
	}
	ctx, span := c.tracer.Start(ctx, name, trace.WithAttributes(attrs...))
	return &otelThread{ctx: ctx, span: span}, nil
}

func (c *otelConnection) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Give the exporter 5 s to flush any pending spans before shutdown.
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.provider.Shutdown(shutCtx); err != nil {
		slog.Warn("OTEL TracerProvider shutdown error", slog.Any(

			// otelThread wraps a root span as a Thread.
			"error", err))
	}
}

type otelThread struct {
	ctx  context.Context
	span trace.Span
}

// AddRefs attaches safe metadata (artifact_id, service_id …) as span attributes.
// Callers MUST NOT pass credentials here — see package doc.
func (t *otelThread) AddRefs(ctx context.Context, refs map[string]string) Thread {
	attrs := make([]attribute.KeyValue, 0, len(refs))
	for k, v := range refs {
		attrs = append(attrs, attribute.String("fused.ref."+k, v))
	}
	t.span.SetAttributes(attrs...)
	return t
}

// Step creates a child span within the root thread span.
func (t *otelThread) Step(name string) Step {
	_, span := otel.Tracer("fused").Start(t.ctx, name)
	return &otelStep{span: span}
}

// Complete ends the root span with status OK.
func (t *otelThread) Complete(_ context.Context, message string) {
	t.span.SetStatus(codes.Ok, message)
	t.span.End()
}

// Close ends the root span with status Error (cancelled / non-successful).
func (t *otelThread) Close(_ context.Context, message string) {
	t.span.SetStatus(codes.Error, message)
	t.span.End()
}

// otelStep wraps a child span as a Step.
type otelStep struct {
	span trace.Span
}

// AddContext attaches observable metadata to the step span.
func (s *otelStep) AddContext(data map[string]any) Step {
	s.setAttrs("fused.ctx.", data)
	return s
}

// AddPrivateContext attaches metadata that is NOT exported to the OTEL backend.
// Under OTEL this is a no-op by design — private context is only meaningful
// in a local-backend model (e.g. an observability dashboard). Callers can still
// call it safely; it simply won't appear in traces.
func (s *otelStep) AddPrivateContext(_ map[string]any) Step {
	// Intentional no-op: OTEL has no concept of private attributes.
	// Data passed here is neither stored nor transmitted.
	return s
}

// SubStep records a named event on the current span (OTEL doesn't have nested
// steps, so we model sub-steps as span events for observability).
func (s *otelStep) SubStep(name string, data map[string]any, status ...string) Step {
	attrs := make([]attribute.KeyValue, 0, len(data)+1)
	if len(status) > 0 {
		attrs = append(attrs, attribute.String("status", status[0]))
	}
	for k, v := range data {
		attrs = append(attrs, attribute.String(k, fmt.Sprintf("%v", v)))
	}
	s.span.AddEvent(name, trace.WithAttributes(attrs...))
	return s
}

// Success ends the step span with status OK.
func (s *otelStep) Success(_ context.Context) {
	s.span.SetStatus(codes.Ok, "")
	s.span.End()
}

// Failed ends the step span with status Error.
func (s *otelStep) Failed(_ context.Context, message string) {
	s.span.SetStatus(codes.Error, message)
	s.span.End()
}

// Error ends the step span with an error.
func (s *otelStep) Error(_ context.Context, err error) {
	s.span.RecordError(err)
	s.span.SetStatus(codes.Error, err.Error())
	s.span.End()
}

func (s *otelStep) setAttrs(prefix string, data map[string]any) {
	attrs := make([]attribute.KeyValue, 0, len(data))
	for k, v := range data {
		attrs = append(attrs, attribute.String(prefix+k, fmt.Sprintf("%v", v)))
	}
	s.span.SetAttributes(attrs...)
}

// ─── Global connection + public API ──────────────────────────────────────────

var globalConn Connection = &noopConnection{}

// Init initialises the OTel TracerProvider. Call once at startup.
//
// Required env var: OTEL_EXPORTER_OTLP_ENDPOINT (e.g. "http://localhost:4318")
// Optional env var: OTEL_SERVICE_NAME (defaults to "fused")
//
// If OTEL_EXPORTER_OTLP_ENDPOINT is not set, the package operates as a no-op
// and no traces are emitted. This keeps local development zero-config.
func Init(ctx context.Context) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	threadifyKey := os.Getenv("THREADIFY_API_KEY")

	if endpoint == "" && threadifyKey == "" {
		slog.Warn("OTEL exporters not configured — observability disabled (no-op)")
		return
	}

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "fused"
	}

	batchers := buildTraceExporters(ctx, traceExporterConfig{
		endpoint:     endpoint,
		threadifyKey: threadifyKey,
		serviceName:  serviceName,
	})

	if len(batchers) == 0 {
		slog.Warn("No valid observability exporters created — operating as no-op")
		return
	}

	res, _ := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName), semconv.DeploymentEnvironment(EngineEnvironment())),
	)

	opts := append(batchers, sdktrace.WithResource(res))
	provider := sdktrace.NewTracerProvider(opts...)

	otel.SetTracerProvider(provider)
	tracer := provider.Tracer("fused")

	globalConn = &otelConnection{
		provider: provider,
		tracer:   tracer,
	}
	slog.Info("OTEL tracing initialised",
		slog.String("endpoint", endpoint),
		slog.Bool("threadify_enabled", threadifyKey != ""),
		slog.String("service", serviceName),
	)
}

type traceExporterConfig struct {
	endpoint     string
	threadifyKey string
	serviceName  string
}

func buildTraceExporters(ctx context.Context, cfg traceExporterConfig) []sdktrace.TracerProviderOption {
	var batchers []sdktrace.TracerProviderOption
	if cfg.endpoint != "" {
		if batcher := newOTLPTraceBatcher(ctx, cfg.endpoint); batcher != nil {
			batchers = append(batchers, batcher)
		}
	}
	if cfg.threadifyKey != "" {
		if batcher := newThreadifyTraceBatcher(ctx, cfg.threadifyKey, cfg.serviceName); batcher != nil {
			batchers = append(batchers, batcher)
		}
	}
	return batchers
}

func newOTLPTraceBatcher(ctx context.Context, endpoint string) sdktrace.TracerProviderOption {
	exporterOptions := []otlptracehttp.Option{}
	if isOTLPEndpointURL(endpoint) {
		exporterOptions = append(exporterOptions, otlptracehttp.WithEndpointURL(endpoint))
	} else {
		exporterOptions = append(exporterOptions,
			otlptracehttp.WithEndpoint(endpoint),
			otlptracehttp.WithInsecure(), // TLS termination happens at the collector
		)
	}
	exporter, err := otlptracehttp.New(ctx, exporterOptions...)
	if err != nil {
		slog.Error("Failed to create HTTP OTEL exporter", slog.Any("error", err))
		return nil
	}
	return sdktrace.WithBatcher(exporter)
}

func newThreadifyTraceBatcher(ctx context.Context, threadifyKey string, serviceName string) sdktrace.TracerProviderOption {
	conn, err := threadify.Connect(ctx, threadifyKey, threadify.WithServiceName(serviceName))
	if err != nil {
		slog.Error("Failed to connect Threadify SDK", slog.Any("error", err))
		return nil
	}
	exporter := NewThreadifyExporter(conn, SpanExporterOptions{})
	return sdktrace.WithBatcher(exporter)
}

func Start(ctx context.Context, name, parentID string, tags ...string) (Thread, error) {
	return globalConn.Start(ctx, name, parentID, tags...)
}

// Close shuts down the global OTel provider gracefully.
// Call on process shutdown to flush pending spans.
func Close() {
	globalConn.Close()
}

// ─── Context helpers (unchanged API) ─────────────────────────────────────────

type contextKey string

const (
	threadKey    contextKey = "fused_trace_thread"
	KeepAliveKey contextKey = "fused_trace_keepalive"
)

// NewContext returns a new context with the given Thread attached.
func NewContext(ctx context.Context, t Thread) context.Context {
	return context.WithValue(ctx, threadKey, t)
}

// ThreadFromContext retrieves the Thread from the context, or a noopThread if absent.
func ThreadFromContext(ctx context.Context) Thread {
	if t, ok := ctx.Value(threadKey).(Thread); ok {
		return t
	}
	return &noopThread{}
}

// DetachThread sets the keep-alive flag in the context so HTTP middleware does
// not close the thread when the request finishes (e.g. async workers).
func DetachThread(ctx context.Context) {
	if keepAlive, ok := ctx.Value(KeepAliveKey).(*bool); ok {
		*keepAlive = true
	}
}
