package sandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/Usefused/engine/internal/engine/connectresource"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/serverrouting"
)

type RuntimeEnvironmentResolution struct {
	Environment string
	BaseURL     string
	Source      string
	Variables   []serverrouting.Variable
}

var errAbsoluteServerOverrideRequired = errors.New("service server URL requires an absolute workspace or connection-profile override")

const (
	serverVariableBindingLocation  = "server_variable"
	serverVariableSourceConnection = "connection_resource"
	serverVariableSourceApp        = "app_injection"
	serverVariableSourceWorkspace  = "workspace_policy"
)

type serverVariableInputs struct {
	values  map[string]string
	sources map[string]string
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
		name := runtimeServerName(server.Name, server.Environment)
		if server.URL != "" {
			out = append(out, runtimeServer{Name: name, URL: server.URL, IsDefault: server.IsDefault, Variables: server.Variables})
		}
	}
	return out
}

func runtimeServerName(name, legacyEnvironment string) string {
	if current := strings.TrimSpace(name); current != "" {
		return current
	}
	return strings.TrimSpace(legacyEnvironment)
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
		// Registry preserves unresolved source references, but execution must not
		// invent an origin. Workspace or trusted resource configuration owns it.
		return resolution, validateStaticRuntimeServer(resolution.BaseURL)
	}
	supplied, err := serverVariableValues(credentials, values, metadata.ServerVariables)
	if err != nil {
		return RuntimeEnvironmentResolution{}, err
	}
	resolved, usedSupplied, err := serverrouting.Resolve(resolution.BaseURL, resolution.Variables, supplied.values)
	if err != nil {
		return RuntimeEnvironmentResolution{}, runtimeServerResolutionError(resolution.BaseURL, err)
	}
	// Provider defaults do not change the resolution source; supplied inputs may
	// require an additional trust-boundary decision.
	if usedSupplied {
		if err := validateSuppliedServerRouting(metadata, resolution.BaseURL, resolved, resolution.Variables, supplied.sources); err != nil {
			return RuntimeEnvironmentResolution{}, err
		}
		// The bounded winning layer makes user/agent routing changes auditable
		// without recording variable names, values, URLs, or hostnames.
		resolution.Source = resolvedServerVariableSource(resolution.Variables, supplied.sources, resolution.Source)
	}
	resolution.BaseURL = resolved
	return resolution, nil
}

func validateStaticRuntimeServer(baseURL string) error {
	if requiresAbsoluteServerOverride(baseURL) {
		return errAbsoluteServerOverrideRequired
	}
	return serverrouting.ValidateResolvedURL(baseURL)
}

func runtimeServerResolutionError(baseURL string, err error) error {
	if requiresAbsoluteServerOverride(baseURL) {
		return errAbsoluteServerOverrideRequired
	}
	return err
}

func requiresAbsoluteServerOverride(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	return err == nil && !parsed.IsAbs()
}

func forcedRuntimeBaseURL(values []store.BucketValue) string {
	for _, value := range values {
		if value.Location == "base_url" && value.SourceKind == "connection_resource" && value.Mode == "force" {
			return value.Value
		}
	}
	return ""
}

func serverVariableValues(credentials map[string]any, values []store.BucketValue, configured map[string]string) (serverVariableInputs, error) {
	result, err := resourceMetadataValues(credentials["fused_resource_metadata"])
	if err != nil {
		return serverVariableInputs{}, err
	}
	inputs := serverVariableInputs{values: result, sources: make(map[string]string, len(result))}
	// Provider resource metadata is trusted only after its connection profile
	// has passed the existing resource and host validation path.
	for name := range result {
		inputs.sources[name] = serverVariableSourceConnection
	}
	// One pass merges the already-resolved connection and app bindings without
	// introducing another bucket read or a second precedence implementation.
	for _, value := range values {
		// A consumed connection binding wins before app mode is evaluated.
		if applyConnectionServerVariable(&inputs, value) {
			continue
		}
		applyAppServerVariable(&inputs, value)
	}
	// Workspace policy remains the final local authority, matching the existing
	// operation-server precedence while also covering service-level templates.
	for name, value := range configured {
		inputs.values[name] = value
		inputs.sources[name] = serverVariableSourceWorkspace
	}
	return inputs, nil
}

// applyConnectionServerVariable preserves the provenance already attached by
// the compiled connection-resource binding path.
func applyConnectionServerVariable(inputs *serverVariableInputs, value store.BucketValue) bool {
	// Empty targets cannot contribute to a declared provider variable.
	if value.SourceKind != "connection_resource" || value.KeyName == "" {
		return false
	}
	inputs.values[value.KeyName] = value.Value
	inputs.sources[value.KeyName] = serverVariableSourceConnection
	return true
}

// applyAppServerVariable applies mode precedence to values already resolved
// through the app bucket's existing batched ingestion path.
func applyAppServerVariable(inputs *serverVariableInputs, value store.BucketValue) {
	// Ordinary request injections remain owned by the dispatcher.
	if value.Location != serverVariableBindingLocation || value.SourceKind != "literal" || value.KeyName == "" {
		return
	}
	// Default mode preserves a value already selected by a resource profile.
	if value.Mode == "default" && inputs.values[value.KeyName] != "" {
		return
	}
	// Plans admit only these modes; omission is retained for legacy app rows
	// and has the same forced meaning as current canonical plans.
	if value.Mode != "force" && value.Mode != "" && value.Mode != "default" {
		return
	}
	inputs.values[value.KeyName] = value.Value
	inputs.sources[value.KeyName] = serverVariableSourceApp
}

// usesConnectionResourceVariable applies the stronger allowlist only when the
// selected template actually consumes connection-resource data.
func usesServerVariableSource(variables []serverrouting.Variable, sources map[string]string, source string) bool {
	// The declared variable set bounds which resolved inputs can affect the URL.
	for _, variable := range variables {
		// Provenance is tracked after precedence so an overridden lower layer does
		// not retain authority over the final routing decision.
		if sources[variable.Name] == source {
			return true
		}
	}
	return false
}

// resolvedServerVariableSource reports only the highest-precedence consumed
// layer so OTEL and durable Activity explain who selected the route safely.
func resolvedServerVariableSource(variables []serverrouting.Variable, sources map[string]string, fallback string) string {
	// Explicit priority checks make the aggregate independent of provider
	// variable ordering while preserving the runtime merge precedence.
	if usesServerVariableSource(variables, sources, serverVariableSourceWorkspace) {
		return serverVariableSourceWorkspace
	}
	if usesServerVariableSource(variables, sources, serverVariableSourceApp) {
		return serverVariableSourceApp
	}
	if usesServerVariableSource(variables, sources, serverVariableSourceConnection) {
		return serverVariableSourceConnection
	}
	return fallback
}

// validateSuppliedServerRouting keeps resource/workspace allowlists intact and
// confines app bucket values to a provider-owned registrable domain.
func validateSuppliedServerRouting(metadata *fusedobject.ServiceMetadata, template, resolved string, variables []serverrouting.Variable, sources map[string]string) error {
	usesConnection := usesServerVariableSource(variables, sources, serverVariableSourceConnection)
	usesWorkspace := usesServerVariableSource(variables, sources, serverVariableSourceWorkspace)
	// Resource and workspace routing retain the reviewed ConnectConfig allowlist
	// used before app server-variable support was introduced.
	if usesConnection || usesWorkspace {
		if err := connectresource.ValidateBaseURL(resolved, runtimeAllowedHosts(metadata.ConnectConfig)); err != nil {
			return err
		}
	}
	return serverrouting.ValidateResolvedHostAnchor(template, resolved, unboundedServerVariablesBySource(variables, sources, serverVariableSourceApp))
}

// unboundedServerVariablesBySource selects only non-enum inputs because the
// resolver already confines enum-bound values to the provider declaration.
func unboundedServerVariablesBySource(variables []serverrouting.Variable, sources map[string]string, source string) map[string]bool {
	selected := make(map[string]bool)
	// Only variables consumed by the selected template can affect its route.
	for _, variable := range variables {
		// Enum-bound host variables are an immutable provider allowlist already.
		if sources[variable.Name] == source && len(variable.Enum) == 0 {
			selected[variable.Name] = true
		}
	}
	return selected
}

func applyOperationRuntimeServer(metadata *fusedobject.ServiceMetadata, service *models.Service, operation *models.IntegrationObject, resolution RuntimeEnvironmentResolution, credentials map[string]any, values []store.BucketValue) (RuntimeEnvironmentResolution, error) {
	if len(operation.OperationServers) == 0 || service.ServerSource == "connection_resource" {
		return resolution, nil
	}
	server, ok := selectOperationServer(operation.OperationServers, resolution.Environment)
	if !ok {
		return resolution, nil
	}
	supplied, err := serverVariableValues(credentials, values, metadata.ServerVariables)
	if err != nil {
		return resolution, err
	}
	resolved, dynamic, err := resolveOperationServerURL(service.BaseURL, server, supplied.values)
	if err != nil {
		return resolution, err
	}
	// Dynamic operation servers reuse the same source-specific trust boundary as
	// service-level templates before replacing the request origin.
	if dynamic {
		if err := validateSuppliedServerRouting(metadata, server.URL, resolved, server.Variables, supplied.sources); err != nil {
			return resolution, err
		}
	}
	service.BaseURL = resolved
	service.ServerSource = "operation"
	resolution.BaseURL = resolved
	resolution.Source = resolvedServerVariableSource(server.Variables, supplied.sources, "operation")
	return resolution, nil
}

func selectOperationServer(servers models.Servers, environment string) (models.Server, bool) {
	for _, server := range servers {
		name := runtimeServerName(server.Name, server.Environment)
		if environment != "" && comparableEnvironmentName(name) == comparableEnvironmentName(environment) {
			return server, true
		}
	}
	for _, server := range servers {
		if server.IsDefault {
			return server, true
		}
	}
	if len(servers) == 0 {
		return models.Server{}, false
	}
	return servers[0], true
}

func resolveOperationServerURL(serviceBaseURL string, server models.Server, supplied map[string]string) (string, bool, error) {
	reference, dynamic, err := serverrouting.ResolveReference(server.URL, server.Variables, supplied)
	if err != nil {
		return "", false, err
	}
	base, baseErr := url.Parse(serviceBaseURL)
	relative, relativeErr := url.Parse(reference)
	if baseErr != nil || relativeErr != nil || !base.IsAbs() {
		return "", false, errors.New("operation server URL is invalid")
	}
	resolved := base.ResolveReference(relative).String()
	if err := serverrouting.ValidateResolvedURL(resolved); err != nil {
		return "", false, err
	}
	return resolved, dynamic, nil
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

// runtimeAllowedHosts selects the allowlist that owns the persisted route.
// Discovery is authoritative when both collection and discovery are present;
// the supplied site identifies a provider-discovered API route but is never
// itself used as the execution base URL.
func runtimeAllowedHosts(config *fusedobject.ServiceConnectConfig) []string {
	if config == nil {
		return nil
	}
	if config.ResourceDiscovery != nil {
		return config.ResourceDiscovery.AllowedHosts
	}
	if config.ResourceInput != nil {
		return config.ResourceInput.AllowedHosts
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
	var connectionErr *ConnectionRequiredError
	// callers can start the exact missing Engine connection without seeing
	// provider credential material or parsing resolver text.
	if errors.As(err, &connectionErr) {
		return mustJSONError(connectionErr)
	}
	var resourceErr *ResourceSelectionRequiredError
	// resource selection is a bounded user action rather than an opaque
	// provider execution failure.
	if errors.As(err, &resourceErr) {
		return mustJSONError(resourceErr)
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
