package observability

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

const maxExportedLogValueBytes = 256

var (
	loggerProvider        *sdklog.LoggerProvider
	exportedLogAttributes = map[string]struct{}{
		"action": {}, "attempted": {}, "batch_limit": {}, "cache_status": {}, "claimed": {},
		"count": {}, "error_code": {}, "error_count": {}, "failure_code": {}, "lease_contended": {},
		"method_family": {}, "mode": {}, "outcome": {}, "reconnect_required": {}, "refreshed": {},
		"segment": {}, "selection_count": {}, "status": {}, "tls": {}, "transient_failure": {},
		"trigger": {}, "usage_reporting": {}, "was_idle": {},
	}
)

// InitLogs installs a process-wide stderr/OTLP tee whose exported attributes
// follow a fixed, bounded contract and exclude raw errors and identifiers.
func InitLogs(ctx context.Context, configuredEndpoint ...string) {
	endpoint, endpointFromEnvironment := signalEndpoint("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", configuredEndpoint)
	console := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	// Local stderr remains the only destination when log export is unconfigured.
	if endpoint == "" {
		slog.SetDefault(slog.New(console))
		return
	}
	exporterOptions := []otlploghttp.Option{}
	// Standard environment configuration owns signal paths, headers, certificates, and TLS flags.
	if endpointFromEnvironment {
		exporterOptions = nil
	} else if isOTLPEndpointURL(endpoint) {
		// A configured URL supplies scheme and optional explicit path.
		exporterOptions = append(exporterOptions, otlploghttp.WithEndpointURL(endpoint))
	} else {
		// Legacy host:port targets use plaintext OTLP/HTTP.
		exporterOptions = append(exporterOptions, otlploghttp.WithEndpoint(endpoint), otlploghttp.WithInsecure())
	}
	exporter, err := otlploghttp.New(ctx, exporterOptions...)
	// Exporter construction failures must leave operational stderr logging intact.
	if err != nil {
		slog.SetDefault(slog.New(console))
		slog.Error("Failed to create OTEL log exporter", slog.String("error_code", "otel_log_exporter_init_failed"))
		return
	}
	serviceName := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME"))
	// Keep resource identity aligned across traces, metrics, and logs.
	if serviceName == "" {
		serviceName = "fused"
	}
	res, _ := resource.New(ctx, resource.WithAttributes(
		semconv.ServiceName(serviceName), semconv.DeploymentEnvironment(EngineEnvironment()),
	))
	loggerProvider = sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)), sdklog.WithResource(res),
	)
	global.SetLoggerProvider(loggerProvider)
	otelHandler := &safeOTLPLogHandler{next: otelslog.NewHandler(serviceName)}
	slog.SetDefault(slog.New(&teeHandler{console: console, export: otelHandler}))
	slog.Info("OTEL logs initialised")
	// Force the startup record across the network so configuration failures are locally visible immediately.
	if err := verifyLogExporter(loggerProvider); err != nil {
		slog.Error("OTEL log exporter startup delivery failed", slog.String("error_code", "otel_log_startup_delivery_failed"))
	}
}

// verifyLogExporter waits for the startup batch using a fixed process-owned deadline.
func verifyLogExporter(provider *sdklog.LoggerProvider) error {
	probeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return provider.ForceFlush(probeCtx)
}

// CloseLogs flushes buffered records while the process still has a live shutdown context.
func CloseLogs(ctx context.Context) {
	// A nil provider means log export was intentionally disabled or failed before installation.
	if loggerProvider == nil {
		return
	}
	// Shutdown performs the final batch flush and reports only a stable local failure code.
	if err := loggerProvider.Shutdown(ctx); err != nil {
		slog.Warn("Failed to shutdown logger provider", slog.String("error_code", "otel_log_shutdown_failed"))
	}
	loggerProvider = nil
}

type teeHandler struct {
	console slog.Handler
	export  slog.Handler
}

// Enabled admits a record needed by either destination.
func (t *teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return t.console.Enabled(ctx, level) || t.export.Enabled(ctx, level)
}

// Handle writes independently to stderr and OTLP so exporter trouble cannot suppress local diagnostics.
func (t *teeHandler) Handle(ctx context.Context, record slog.Record) error {
	// Preserve the first failure while still attempting both independent sinks.
	if err := t.console.Handle(ctx, record.Clone()); err != nil {
		_ = t.export.Handle(ctx, record.Clone())
		return err
	}
	return t.export.Handle(ctx, record.Clone())
}

// WithAttrs preserves structured context for both destinations; the OTLP side filters it at emission.
func (t *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &teeHandler{console: t.console.WithAttrs(attrs), export: t.export.WithAttrs(attrs)}
}

// WithGroup preserves slog grouping semantics for both destinations.
func (t *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{console: t.console.WithGroup(name), export: t.export.WithGroup(name)}
}

type safeOTLPLogHandler struct {
	next slog.Handler
}

// Enabled delegates level selection to the OpenTelemetry bridge.
func (h *safeOTLPLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle exports the static message plus allowlisted bounded operational attributes.
func (h *safeOTLPLogHandler) Handle(ctx context.Context, record slog.Record) error {
	safe := slog.NewRecord(record.Time, record.Level, boundedLogValue(record.Message), record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		// Raw errors, identifiers, URLs, provider fields, and unknown future keys stay on stderr only.
		if _, allowed := exportedLogAttributes[attr.Key]; allowed {
			safe.AddAttrs(slog.String(attr.Key, boundedLogValue(attr.Value.String())))
		}
		return true
	})
	return h.next.Handle(ctx, safe)
}

// WithAttrs prefilters persistent attributes through the same explicit export contract.
func (h *safeOTLPLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	filtered := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		// Unknown persistent context must not bypass per-record filtering.
		if _, allowed := exportedLogAttributes[attr.Key]; allowed {
			filtered = append(filtered, slog.String(attr.Key, boundedLogValue(attr.Value.String())))
		}
	}
	return &safeOTLPLogHandler{next: h.next.WithAttrs(filtered)}
}

// WithGroup retains grouping for allowlisted attributes without exporting the group name as data.
func (h *safeOTLPLogHandler) WithGroup(name string) slog.Handler {
	return &safeOTLPLogHandler{next: h.next.WithGroup(name)}
}

// boundedLogValue enforces valid UTF-8 and a small byte ceiling for exported messages and enum values.
func boundedLogValue(value string) string {
	value = strings.ToValidUTF8(value, "�")
	// Short values preserve their exact operational meaning.
	if len(value) <= maxExportedLogValueBytes {
		return value
	}
	value = value[:maxExportedLogValueBytes]
	// Byte truncation backs up to a complete rune for valid OTLP strings.
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
