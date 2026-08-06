package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Usefused/engine/internal/shared/fusedobject"
)

type RuntimeEnvironmentResolution struct {
	Environment string
	BaseURL     string
	Source      string
}

type EnvironmentNotSupportedError struct {
	Code      string   `json:"code"`
	Requested string   `json:"requested"`
	Available []string `json:"available"`
}

func (e *EnvironmentNotSupportedError) Error() string {
	return fmt.Sprintf("%s: %q not supported; available=%s", e.Code, e.Requested, strings.Join(e.Available, ","))
}

type DefaultEnvironmentNotConfiguredError struct {
	Code      string   `json:"code"`
	Available []string `json:"available"`
}

func (e *DefaultEnvironmentNotConfiguredError) Error() string {
	return fmt.Sprintf("%s: available=%s", e.Code, strings.Join(e.Available, ","))
}

func resolveRuntimeEnvironment(metadata *fusedobject.ServiceMetadata, requested string) (RuntimeEnvironmentResolution, error) {
	if metadata == nil {
		return RuntimeEnvironmentResolution{}, fmt.Errorf("service metadata missing")
	}
	servers := runtimeServers(metadata)
	selected := strings.TrimSpace(requested)
	if selected == "" {
		return defaultRuntimeEnvironment(metadata, servers)
	}
	return namedRuntimeEnvironment(metadata, servers, selected)
}

func namedRuntimeEnvironment(metadata *fusedobject.ServiceMetadata, servers []runtimeServer, requested string) (RuntimeEnvironmentResolution, error) {
	key := comparableEnvironmentName(requested)
	for _, server := range servers {
		if server.Name != "" && comparableEnvironmentName(server.Name) == key {
			return RuntimeEnvironmentResolution{Environment: server.Name, BaseURL: server.URL, Source: "provider"}, nil
		}
	}
	return RuntimeEnvironmentResolution{}, unsupportedEnvironment(requested, servers)
}

func defaultRuntimeEnvironment(metadata *fusedobject.ServiceMetadata, servers []runtimeServer) (RuntimeEnvironmentResolution, error) {
	if server, ok := findDefaultServer(servers, metadata.BaseURL); ok {
		return RuntimeEnvironmentResolution{Environment: server.Name, BaseURL: server.URL, Source: "default"}, nil
	}
	if metadata.BaseURL != "" && len(servers) == 0 {
		return RuntimeEnvironmentResolution{Environment: "", BaseURL: metadata.BaseURL, Source: "default"}, nil
	}
	return RuntimeEnvironmentResolution{}, &DefaultEnvironmentNotConfiguredError{
		Code:      "default_environment_not_configured",
		Available: availableRuntimeEnvironments(servers),
	}
}

type runtimeServer struct {
	Name      string
	URL       string
	IsDefault bool
}

func runtimeServers(metadata *fusedobject.ServiceMetadata) []runtimeServer {
	out := make([]runtimeServer, 0, len(metadata.Servers))
	for _, server := range metadata.Servers {
		name := strings.TrimSpace(server.Environment)
		if server.URL != "" {
			out = append(out, runtimeServer{Name: name, URL: server.URL, IsDefault: server.IsDefault})
		}
	}
	return out
}

func findDefaultServer(servers []runtimeServer, baseURL string) (runtimeServer, bool) {
	var defaultServer runtimeServer
	defaultCount := 0
	for _, server := range servers {
		if server.IsDefault {
			defaultServer = server
			defaultCount++
		}
	}
	if defaultCount == 1 {
		return defaultServer, true
	}
	if defaultCount > 1 {
		return runtimeServer{}, false
	}
	var baseURLServer runtimeServer
	baseURLCount := 0
	for _, server := range servers {
		if baseURL != "" && server.URL == baseURL {
			baseURLServer = server
			baseURLCount++
		}
	}
	if baseURLCount == 1 {
		return baseURLServer, true
	}
	if len(servers) == 1 {
		return servers[0], true
	}
	return runtimeServer{}, false
}

func unsupportedEnvironment(requested string, servers []runtimeServer) error {
	available := availableRuntimeEnvironments(servers)
	return &EnvironmentNotSupportedError{Code: "environment_not_supported", Requested: requested, Available: available}
}

func availableRuntimeEnvironments(servers []runtimeServer) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		key := comparableEnvironmentName(name)
		if key != "" && !seen[key] {
			out = append(out, name)
			seen[key] = true
		}
	}
	for _, server := range servers {
		add(server.Name)
	}
	return out
}

func comparableEnvironmentName(name string) string {
	normalized := strings.ToLower(strings.TrimSpace(name))
	return strings.Join(strings.Fields(normalized), " ")
}

// encodeRuntimeError preserves only documented structured decisions while
// keeping ordinary execution failures backward-compatible as plain strings.
func encodeRuntimeError(err error) string {
	var timeoutErr *executionTimeoutError
	if errors.As(err, &timeoutErr) {
		return mustJSONError(timeoutErr)
	}
	var reconnectErr *ReconnectRequiredError
	// Wrapped reconnect errors retain their typed SDK contract across the
	// resolver and dispatcher layers instead of degrading to a plain string.
	if errors.As(err, &reconnectErr) {
		return mustJSONError(reconnectErr)
	}
	if _, ok := err.(*EnvironmentNotSupportedError); ok {
		return mustJSONError(err)
	}
	if _, ok := err.(*DefaultEnvironmentNotConfiguredError); ok {
		return mustJSONError(err)
	}
	return err.Error()
}

// mustJSONError fails back to the normal error text because error reporting
// must never replace the original failure with a serialization failure.
func mustJSONError(err error) string {
	payload, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		return err.Error()
	}
	return string(payload)
}
