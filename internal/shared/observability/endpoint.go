package observability

import (
	"os"
	"strings"
)

// signalEndpoint resolves the standard signal-specific endpoint before the
// shared endpoint and finally the Engine configuration fallback.
func signalEndpoint(signalEnvironment string, configured []string) (string, bool) {
	// Signal-specific OTEL configuration has the highest standard precedence.
	if endpoint := strings.TrimSpace(os.Getenv(signalEnvironment)); endpoint != "" {
		return endpoint, true
	}
	// The shared endpoint applies to every enabled OTLP signal.
	if endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")); endpoint != "" {
		return endpoint, true
	}
	// YAML remains a supported fallback for existing Engine installations.
	if len(configured) > 0 {
		return strings.TrimSpace(configured[0]), false
	}
	return "", false
}
