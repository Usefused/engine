package observability

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/log/global"
)

// TestLogExportOptOutKeepsStderr proves endpoint configuration cannot override a local-only logging choice.
func TestLogExportOptOutKeepsStderr(t *testing.T) {
	for _, source := range []string{"shared", "logs", "yaml"} {
		// Exercise each endpoint source independently, including normalized opt-out values.
		t.Run(source, func(t *testing.T) {
			var requests atomic.Int64
			// Count actual network traffic to catch startup probes as well as later log batches.
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "")
			t.Setenv("OTEL_LOGS_EXPORTER", "none")
			configured := ""
			// A single active source proves the opt-out also covers the YAML fallback.
			switch source {
			case "shared":
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
			case "logs":
				t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", server.URL+"/v1/logs")
			case "yaml":
				configured = server.URL
				t.Setenv("OTEL_LOGS_EXPORTER", " NONE ")
			}
			stderr, err := os.CreateTemp(t.TempDir(), "stderr")
			// Without a real local sink the console-preservation assertion would be meaningless.
			if err != nil {
				t.Fatal(err)
			}
			previousStderr, previousLogger := os.Stderr, slog.Default()
			previousProvider := global.GetLoggerProvider()
			os.Stderr = stderr
			// Restore process-wide logging even when an assertion aborts this subtest.
			t.Cleanup(func() {
				CloseLogs(context.Background())
				os.Stderr = previousStderr
				slog.SetDefault(previousLogger)
				global.SetLoggerProvider(previousProvider)
				stderr.Close()
			})
			InitLogs(context.Background(), configured)
			slog.Info("local-only log remains visible")
			// No provider means neither a startup probe nor a background export can be scheduled.
			if loggerProvider != nil {
				t.Fatal("disabled logs installed an OTLP provider")
			}
			CloseLogs(context.Background())
			// Closing a disabled exporter must remain harmless and emit no network traffic.
			if requests.Load() != 0 {
				t.Fatalf("disabled logs sent %d requests", requests.Load())
			}
			output, err := os.ReadFile(stderr.Name())
			// Stderr must retain operational messages when remote logging is disabled.
			if err != nil || !strings.Contains(string(output), "local-only log remains visible") {
				t.Fatalf("missing console log: output=%q err=%v", output, err)
			}
		})
	}
}
