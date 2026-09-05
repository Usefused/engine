package observability

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
)

var (
	meterProvider *sdkmetric.MeterProvider
	EngineMeter   metric.Meter

	// RED metrics
	RequestsTotal    metric.Int64Counter
	RequestsDuration metric.Float64Histogram
	RetriesTotal     metric.Int64Counter
	PaginationTotal  metric.Int64Counter
	ScopeRejections  metric.Int64Counter
	WebhookVerify    metric.Int64Counter
)

// InitMetrics installs the process meter provider using standard endpoint precedence and the Engine fallback.
func InitMetrics(ctx context.Context, configuredEndpoint ...string) {
	endpoint, endpointFromEnvironment := signalEndpoint("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", configuredEndpoint)
	// No endpoint means metrics remain registered against a no-op provider.
	if endpoint == "" {
		initNoopMetrics()
		return
	}

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "fused"
	}

	exporterOptions := []otlpmetrichttp.Option{}
	// Preserve standard per-signal environment behavior rather than overriding it programmatically.
	if endpointFromEnvironment {
		exporterOptions = nil
	} else if isOTLPEndpointURL(endpoint) {
		// A configured URL carries its HTTP security and optional path.
		exporterOptions = append(exporterOptions, otlpmetrichttp.WithEndpointURL(endpoint))
	} else {
		// Legacy host:port targets use plaintext OTLP/HTTP.
		exporterOptions = append(exporterOptions,
			otlpmetrichttp.WithEndpoint(endpoint),
			otlpmetrichttp.WithInsecure(),
		)
	}
	exporter, err := otlpmetrichttp.New(ctx, exporterOptions...)
	if err != nil {
		slog.Error("Failed to create OTEL metric exporter", slog.Any("error", err))
		initNoopMetrics()
		return
	}

	res, _ := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName), semconv.DeploymentEnvironment(EngineEnvironment())),
	)

	meterProvider = sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
		sdkmetric.WithResource(res),
	)

	otel.SetMeterProvider(meterProvider)
	EngineMeter = meterProvider.Meter("fused.engine")

	registerMetrics()
	slog.Info("OTEL metrics initialised", slog.String("endpoint", endpoint))
}

func initNoopMetrics() {
	meterProvider = sdkmetric.NewMeterProvider() // Default is no-op
	otel.SetMeterProvider(meterProvider)
	EngineMeter = meterProvider.Meter("fused.engine")
	registerMetrics()
}

func registerMetrics() {
	var err error
	RequestsTotal, err = EngineMeter.Int64Counter("engine.requests.total", metric.WithDescription("Total vendor requests executed"))
	if err != nil {
		slog.Error("Failed to register metric", slog.Any("error", err))
	}

	RequestsDuration, err = EngineMeter.Float64Histogram("engine.requests.duration", metric.WithDescription("Duration of vendor requests in ms"))
	if err != nil {
		slog.Error("Failed to register metric", slog.Any("error", err))
	}

	RetriesTotal, err = EngineMeter.Int64Counter("engine.retries.total", metric.WithDescription("Total automatic retries"))
	if err != nil {
		slog.Error("Failed to register metric", slog.Any("error", err))
	}

	PaginationTotal, err = EngineMeter.Int64Counter("engine.pagination.pages.total", metric.WithDescription("Total automated pagination pages fetched"))
	if err != nil {
		slog.Error("Failed to register metric", slog.Any("error", err))
	}

	ScopeRejections, err = EngineMeter.Int64Counter("engine.scope.rejections.total", metric.WithDescription("Total scope rejections (L2 governance)"))
	if err != nil {
		slog.Error("Failed to register metric", slog.Any("error", err))
	}

	WebhookVerify, err = EngineMeter.Int64Counter("engine.webhook.verify.total", metric.WithDescription("Total webhook signature verifications"))
	if err != nil {
		slog.Error("Failed to register metric", slog.Any("error", err))
	}
}

func CloseMetrics(ctx context.Context) {
	if meterProvider != nil {
		if err := meterProvider.Shutdown(ctx); err != nil {
			slog.Warn("Failed to shutdown meter provider", slog.Any("error", err))
		}
	}
}

func init() {
	// Initialize default no-op metrics for tests
	initNoopMetrics()
}
