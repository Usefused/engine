package sandbox

import (
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
)

// TestSanitiseParams verifies the single credential-stripping enforcement point.
// All OTEL/analytics recording paths call sanitiseParams before persisting —
// keeping the guarantee centralised rather than duplicated in each callsite.
func TestSanitiseParams(t *testing.T) {
	tests := []struct {
		name        string
		input       map[string]any
		extraKeys   []string
		mustAbsent  []string
		mustPresent map[string]any
	}{
		{
			name:        "strips Authorization header",
			input:       map[string]any{"query": "orders", "Authorization": "Bearer tok"},
			mustAbsent:  []string{"Authorization"},
			mustPresent: map[string]any{"query": "orders"},
		},
		{
			name:        "strips password",
			input:       map[string]any{"limit": 50, "password": "s3cr3t"},
			mustAbsent:  []string{"password"},
			mustPresent: map[string]any{"limit": 50},
		},
		{
			name:        "strips apiKey case-insensitively",
			input:       map[string]any{"page": 1, "ApiKey": "abc123"},
			mustAbsent:  []string{"ApiKey"},
			mustPresent: map[string]any{"page": 1},
		},
		{
			name:        "strips common aliases",
			input:       map[string]any{"api_key": "k", "access_token": "t", "client_secret": "s", "refresh_token": "r"},
			mustAbsent:  []string{"api_key", "access_token", "client_secret", "refresh_token"},
			mustPresent: map[string]any{},
		},
		{
			name:        "strips certificate aliases",
			input:       map[string]any{"cert": "pem", "client_key": "key", "operation": "list"},
			mustAbsent:  []string{"cert", "client_key"},
			mustPresent: map[string]any{"operation": "list"},
		},
		{
			name:        "strips service-specific extra keys",
			input:       map[string]any{"query": "ok", "X-Custom-Key": "secret"},
			extraKeys:   []string{"X-Custom-Key"},
			mustAbsent:  []string{"X-Custom-Key"},
			mustPresent: map[string]any{"query": "ok"},
		},
		{
			name:        "passes through clean params unchanged",
			input:       map[string]any{"status": "active", "page": 2},
			mustAbsent:  nil,
			mustPresent: map[string]any{"status": "active", "page": 2},
		},
		{
			name:        "nil input returns empty map",
			input:       nil,
			mustPresent: map[string]any{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := sanitiseParams(tt.input, tt.extraKeys...)
			for _, k := range tt.mustAbsent {
				if _, ok := out[k]; ok {
					t.Errorf("credential key %q survived sanitiseParams", k)
				}
			}
			for k, want := range tt.mustPresent {
				if got, ok := out[k]; !ok {
					t.Errorf("safe key %q was dropped", k)
				} else if got != want {
					t.Errorf("key %q: got %v, want %v", k, got, want)
				}
			}
		})
	}
}

func TestCredentialKeysFromAuthConfigs(t *testing.T) {
	auths := models.AuthConfigs{
		{Type: "apiKey", KeyName: "X-Api-Key"},
		{Type: "http", Scheme: "bearer"},
		{Type: "mutualTLS", Name: "clientCert"},
	}
	got := credentialKeysFromAuthConfigs(auths)
	want := []string{"X-Api-Key", "clientCert_cert", "clientCert_key"}
	if len(got) != len(want) {
		t.Errorf("unexpected keys: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("unexpected keys: %v", got)
		}
	}
}
