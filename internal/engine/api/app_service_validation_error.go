package api

import (
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

// appServiceValidationError keeps identity separate from the explanation so
// public plan errors can use names without parsing or replacing UUIDs in prose.
type appServiceValidationError struct {
	serviceID uuid.UUID
	reason    string
	code      string
	detail    string
	remedy    string
}

// Error preserves the ID-based fallback for callers without resolved display metadata.
func (err appServiceValidationError) Error() string {
	return fmt.Sprintf("service %s %s", err.serviceID, err.reason)
}

// httpError keeps human diagnostics on the existing response contract, separate
// from the fixed auth-decision attributes recorded on the planning span.
func (err appServiceValidationError) httpError(service sdkResolvedService) workspaceConfigHTTPError {
	details := map[string]any{"service_id": err.serviceID.String()}
	// Optional diagnostic text uses the field already supported by CLI and JSON consumers.
	if err.detail != "" {
		details["server_detail"] = err.detail
	}
	return workspaceConfigHTTPError{
		status: http.StatusBadRequest, code: err.code,
		message: appValidationServiceLabel(service, err.serviceID) + " " + err.reason,
		details: details, remediation: err.remedy,
	}
}

// appValidationServiceLabel reuses the authorized plan's service name and exact
// config key; opaque identity remains authoritative and is a display fallback.
func appValidationServiceLabel(service sdkResolvedService, serviceID uuid.UUID) string {
	// Never attach another selection's label when parallel metadata is inconsistent.
	if service.ServiceID != serviceID {
		return "service " + serviceID.String()
	}
	name, key := safeAppValidationLabel(service.ServiceName), safeAppValidationLabel(service.PublicTarget)
	// A display name plus config key lets users locate the precise YAML entry.
	if name != "" && key != "" && name != key {
		return fmt.Sprintf("service %q (config key %q)", name, key)
	}
	// Prefer the already-known human name, without inventing a Registry lookup.
	if name != "" {
		return fmt.Sprintf("service %q", name)
	}
	// Slug-only resolution is still useful when no display name is available.
	if key != "" {
		return fmt.Sprintf("service %q", key)
	}
	return "service " + serviceID.String()
}

// safeAppValidationLabel bounds caller/provider display metadata and omits
// credential-shaped or terminal-control text from the public error envelope.
func safeAppValidationLabel(value string) string {
	// Reject overlong values before scanning so metadata cannot inflate error output.
	if len(value) > 128 {
		return ""
	}
	// Controls and bidi formatting must not spoof the service named by an error.
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return ""
		}
	}
	lower := strings.ToLower(value)
	// These shapes belong to credentials or transport, never a safe service label.
	for _, marker := range []string{"fsk_", "-----begin ", "://", "authorization:", "authorization=", "bearer ", "access_token=", "refresh_token=", "client_secret=", "password=", "api_key=", "apikey=", "secret="} {
		if strings.Contains(lower, marker) {
			return ""
		}
	}
	return strings.TrimSpace(value)
}
