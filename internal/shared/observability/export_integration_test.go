package observability

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"
)

// TestOTLPTraceExportUsesStandardSignalPaths proves a completed Engine thread reaches the configured HTTP receiver.
func TestOTLPTraceExportUsesStandardSignalPaths(t *testing.T) {
	requests := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		// Capture only the first request so exporter retries cannot block the receiver.
		select {
		case requests <- request.URL.Path:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("THREADIFY_API_KEY", "")
	previousProvider := otel.GetTracerProvider()
	Init(context.Background())
	thread, err := Start(context.Background(), "test trace export", "")
	// Starting a configured trace must use the real provider.
	if err != nil {
		t.Fatal(err)
	}
	thread.Complete(context.Background(), "complete")
	Close()
	otel.SetTracerProvider(previousProvider)
	globalConn = &noopConnection{}
	// The generic endpoint must receive the standard trace signal path.
	select {
	case path := <-requests:
		if path != "/v1/traces" {
			t.Fatalf("trace export path = %q", path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("trace exporter did not reach receiver")
	}
}

// TestOTLPLogExportUsesStandardSignalPaths proves slog records are drained before shutdown returns.
func TestOTLPLogExportUsesStandardSignalPaths(t *testing.T) {
	requests := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		// Capture one batch while allowing any retry to complete successfully.
		select {
		case requests <- request.URL.Path:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "")
	previousLogger := slog.Default()
	previousProvider := global.GetLoggerProvider()
	InitLogs(context.Background())
	slog.Info("test log export", slog.String("status", "ok"), slog.String("credential", "must-not-export"))
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	CloseLogs(shutdownCtx)
	slog.SetDefault(previousLogger)
	global.SetLoggerProvider(previousProvider)
	// The generic endpoint must receive the standard log signal path.
	select {
	case path := <-requests:
		if path != "/v1/logs" {
			t.Fatalf("log export path = %q", path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("log exporter did not reach receiver")
	}
}

// TestSignalEndpointPrecedence locks the standard per-signal, shared, and YAML fallback order.
func TestSignalEndpointPrecedence(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://shared.example")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "https://traces.example/v1/traces")
	endpoint, fromEnvironment := signalEndpoint("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", []string{"http://yaml.example:4318"})
	// A signal endpoint must override both lower-precedence sources.
	if endpoint != "https://traces.example/v1/traces" || !fromEnvironment {
		t.Fatalf("signal endpoint = %q, environment=%v", endpoint, fromEnvironment)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	endpoint, fromEnvironment = signalEndpoint("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", []string{"http://yaml.example:4318"})
	// The shared standard endpoint must override YAML.
	if endpoint != "https://shared.example" || !fromEnvironment {
		t.Fatalf("shared endpoint = %q, environment=%v", endpoint, fromEnvironment)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	endpoint, fromEnvironment = signalEndpoint("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", []string{"http://yaml.example:4318"})
	// YAML is a programmatic fallback rather than an environment-owned endpoint.
	if endpoint != "http://yaml.example:4318" || fromEnvironment {
		t.Fatalf("YAML endpoint = %q, environment=%v", endpoint, fromEnvironment)
	}
}

// TestSafeOTLPLogHandlerDropsUnreviewedFields enforces the Engine log export disclosure contract.
func TestSafeOTLPLogHandlerDropsUnreviewedFields(t *testing.T) {
	capture := &capturingSlogHandler{}
	handler := &safeOTLPLogHandler{next: capture}
	record := slog.NewRecord(time.Now(), slog.LevelError, "provider call failed", 0)
	record.AddAttrs(slog.String("error_code", "provider_failed"), slog.String("credential", "secret"), slog.Any("error", context.Canceled))
	// A capture failure would invalidate the disclosure assertion.
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	attrs := capture.attributes()
	// Only explicitly reviewed fields may cross the OTLP boundary.
	if attrs["error_code"] != "provider_failed" || attrs["credential"] != "" || attrs["error"] != "" {
		t.Fatalf("exported attributes = %#v", attrs)
	}
}

type capturingSlogHandler struct {
	mu     sync.Mutex
	record slog.Record
}

// Enabled accepts every test record so filtering is isolated from log levels.
func (h *capturingSlogHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle retains a cloned record for deterministic inspection.
func (h *capturingSlogHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.record = record.Clone()
	return nil
}

// WithAttrs is unused by the focused test and keeps the same capture sink.
func (h *capturingSlogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

// WithGroup is unused by the focused test and keeps the same capture sink.
func (h *capturingSlogHandler) WithGroup(string) slog.Handler { return h }

// attributes projects the captured record into string values for exact assertions.
func (h *capturingSlogHandler) attributes() map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := map[string]string{}
	h.record.Attrs(func(attr slog.Attr) bool {
		result[attr.Key] = attr.Value.String()
		return true
	})
	return result
}
