package sandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Usefused/engine/internal/engine/connectresource"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/serverrouting"
)

type RuntimeEnvironmentResolution struct {
	Environment string
	BaseURL     string
	Source      string
	Variables   []serverrouting.Variable
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
			return RuntimeEnvironmentResolution{Environment: server.Name, BaseURL: server.URL, Source: "provider", Variables: server.Variables}, nil
		}
	}
	return RuntimeEnvironmentResolution{}, unsupportedEnvironment(requested, servers)
}

func defaultRuntimeEnvironment(metadata *fusedobject.ServiceMetadata, servers []runtimeServer) (RuntimeEnvironmentResolution, error) {
	if server, ok := findDefaultServer(servers, metadata.BaseURL); ok {
		return RuntimeEnvironmentResolution{Environment: server.Name, BaseURL: server.URL, Source: "default", Variables: server.Variables}, nil
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
	Variables []serverrouting.Variable
}

func runtimeServers(metadata *fusedobject.ServiceMetadata) []runtimeServer {
	out := make([]runtimeServer, 0, len(metadata.Servers))
	for _, server := range metadata.Servers {
		name := strings.TrimSpace(server.Environment)
		if server.URL != "" {
			out = append(out, runtimeServer{Name: name, URL: server.URL, IsDefault: server.IsDefault, Variables: server.Variables})
		}
	}
	return out
}

func resolveRuntimeServerTemplate(metadata *fusedobject.ServiceMetadata, resolution RuntimeEnvironmentResolution, credentials map[string]any, values []store.BucketValue) (RuntimeEnvironmentResolution, error) {
	if baseURL := forcedRuntimeBaseURL(values); baseURL != "" {
		if err := serverrouting.ValidateResolvedURL(baseURL); err != nil {
			return RuntimeEnvironmentResolution{}, err
		}
		if err := connectresource.ValidateBaseURL(baseURL, runtimeAllowedHosts(metadata.ConnectConfig)); err != nil {
			return RuntimeEnvironmentResolution{}, err
		}
		resolution.BaseURL = baseURL
		resolution.Source = "connection_resource"
		resolution.Variables = nil
		return resolution, nil
	}
	if len(resolution.Variables) == 0 {
		// Registry may preserve a provider's protocol-relative source URL, but
		// execution must never infer a scheme. A workspace override or trusted
		// connection-resource binding has to resolve it to an absolute URL.
		return resolution, serverrouting.ValidateResolvedURL(resolution.BaseURL)
	}
	supplied, err := serverVariableValues(credentials, values)
	if err != nil {
		return RuntimeEnvironmentResolution{}, err
	}
	resolved, usedSupplied, err := serverrouting.Resolve(resolution.BaseURL, resolution.Variables, supplied)
	if err != nil {
		return RuntimeEnvironmentResolution{}, err
	}
	if usedSupplied {
		if err := connectresource.ValidateBaseURL(resolved, runtimeAllowedHosts(metadata.ConnectConfig)); err != nil {
			return RuntimeEnvironmentResolution{}, err
		}
		resolution.Source = "connection_resource"
	}
	resolution.BaseURL = resolved
	return resolution, nil
}

func forcedRuntimeBaseURL(values []store.BucketValue) string {
	for _, value := range values {
		if value.Location == "base_url" && value.SourceKind == "connection_resource" && value.Mode == "force" {
			return value.Value
		}
	}
	return ""
}

func serverVariableValues(credentials map[string]any, values []store.BucketValue) (map[string]string, error) {
	result, err := resourceMetadataValues(credentials["fused_resource_metadata"])
	if err != nil {
		return nil, err
	}
	for _, value := range values {
		if value.SourceKind == "connection_resource" && value.KeyName != "" {
			result[value.KeyName] = value.Value
		}
	}
	return result, nil
}

func resourceMetadataValues(raw any) (map[string]string, error) {
	result := make(map[string]string)
	data, ok := resourceMetadataBytes(raw)
	if !ok || len(data) == 0 {
		return result, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var metadata map[string]any
	if err := decoder.Decode(&metadata); err != nil {
		return nil, errors.New("connection resource metadata is invalid")
	}
	for key, value := range metadata {
		switch scalar := value.(type) {
		case string:
			result[key] = scalar
		case json.Number:
			result[key] = scalar.String()
		case bool:
			result[key] = fmt.Sprint(scalar)
		}
	}
	return result, nil
}

func resourceMetadataBytes(raw any) ([]byte, bool) {
	switch value := raw.(type) {
	case []byte:
		return value, true
	case string:
		return []byte(value), true
	default:
		return nil, false
	}
}

func runtimeAllowedHosts(config *fusedobject.ServiceConnectConfig) []string {
	if config == nil {
		return nil
	}
	if config.ResourceInput != nil {
		return config.ResourceInput.AllowedHosts
	}
	if config.ResourceDiscovery != nil {
		return config.ResourceDiscovery.AllowedHosts
	}
	return nil
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
