package observability

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
)

var loggerProvider *sdklog.LoggerProvider

func InitLogs(ctx context.Context) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		// Use default slog text handler if OTEL is not configured
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
		return
	}

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "fused"
	}

	exporterOptions := []otlploghttp.Option{}
	if isOTLPEndpointURL(endpoint) {
		exporterOptions = append(exporterOptions, otlploghttp.WithEndpointURL(endpoint))
	} else {
		exporterOptions = append(exporterOptions,
			otlploghttp.WithEndpoint(endpoint),
			otlploghttp.WithInsecure(),
		)
	}
	exporter, err := otlploghttp.New(ctx, exporterOptions...)
	if err != nil {
		slog.Error("Failed to create OTEL log exporter", slog.Any("error", err))
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
		return
	}

	res, _ := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceNameKey.String(serviceName), semconv.DeploymentEnvironmentKey.String(EngineEnvironment())),
	)

	processor := sdklog.NewBatchProcessor(exporter)
	loggerProvider = sdklog.NewLoggerProvider(
		sdklog.WithProcessor(processor),
		sdklog.WithResource(res),
	)

	global.SetLoggerProvider(loggerProvider)

	// Create otelslog handler
	otelHandler := otelslog.NewHandler(serviceName)

	// We want to dual-write to stderr (console) and OTEL (for observability).
	// But otelslog doesn't have a multi-handler built in.
	// We can create a simple tee handler.
	tee := &teeHandler{
		h1: slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}),
		h2: otelHandler,
	}

	slog.SetDefault(slog.New(tee))
	slog.Info("OTEL logs initialised", slog.String("endpoint", endpoint))
}

type teeHandler struct {
	h1 slog.Handler
	h2 slog.Handler
}

func (t *teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return t.h1.Enabled(ctx, l) || t.h2.Enabled(ctx, l)
}

func (t *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	if t.h1.Enabled(ctx, r.Level) {
		if err := t.h1.Handle(ctx, r.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	if t.h2.Enabled(ctx, r.Level) {
		if err := t.h2.Handle(ctx, r.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

func (t *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &teeHandler{
		h1: t.h1.WithAttrs(attrs),
		h2: t.h2.WithAttrs(attrs),
	}
}

func (t *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{
		h1: t.h1.WithGroup(name),
		h2: t.h2.WithGroup(name),
	}
}
