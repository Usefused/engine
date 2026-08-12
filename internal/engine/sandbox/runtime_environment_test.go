package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/serverrouting"
	"github.com/google/uuid"
)

func TestResolveRuntimeEnvironmentUsesExplicitProviderLabelNotDescription(t *testing.T) {
	metadata := &fusedobject.ServiceMetadata{
		BaseURL: "https://api.example.test",
		Servers: fusedobject.Servers{
			{URL: "https://api.example.test", Environment: "prod", Description: "Primary live endpoint", IsDefault: true},
			{URL: "https://sandbox.example.test", Environment: "sandbox", Description: "Developer testing area"},
		},
	}

	resolved, err := resolveRuntimeEnvironment(metadata, "sandbox")
	if err != nil {
		t.Fatalf("resolveRuntimeEnvironment: %v", err)
	}
	if resolved.BaseURL != "https://sandbox.example.test" || resolved.Environment != "sandbox" || resolved.Source != "provider" {
		t.Fatalf("unexpected environment resolution: %+v", resolved)
	}
}

func TestResolveRuntimeEnvironmentPrefersOpenAPI32ServerName(t *testing.T) {
	metadata := &fusedobject.ServiceMetadata{Servers: fusedobject.Servers{{
		URL: "https://api.example.test", Name: "Production", Environment: "legacy-prod", IsDefault: true,
	}}}
	resolved, err := resolveRuntimeEnvironment(metadata, "production")
	if err != nil {
		t.Fatalf("resolveRuntimeEnvironment: %v", err)
	}
	if resolved.BaseURL != "https://api.example.test" || resolved.Environment != "Production" {
		t.Fatalf("named server resolution = %+v", resolved)
	}
}

func TestResolveRuntimeServerTemplateUsesProviderDefaultWithoutAllowlist(t *testing.T) {
	defaultTenant := "api"
	metadata := &fusedobject.ServiceMetadata{Servers: fusedobject.Servers{{
		URL: "https://{tenant}.example.com", IsDefault: true,
		Variables: []serverrouting.Variable{{Name: "tenant", Default: &defaultTenant, Required: true}},
	}}}
	resolution, err := resolveRuntimeEnvironment(metadata, "")
	if err == nil {
		resolution, err = resolveRuntimeServerTemplate(metadata, resolution, nil, nil)
	}
	if err != nil || resolution.BaseURL != "https://api.example.com" || resolution.Source != "default" {
		t.Fatalf("resolution=%+v err=%v", resolution, err)
	}
}

func TestResolveRuntimeServerTemplateAllowsDynamicConfluenceTenant(t *testing.T) {
	defaultTenant := "example"
	metadata := &fusedobject.ServiceMetadata{
		Servers: fusedobject.Servers{{
			URL: "https://{your-domain}.atlassian.net", IsDefault: true,
			Variables: []serverrouting.Variable{{Name: "your-domain", Default: &defaultTenant, Required: true}},
		}},
		ConnectConfig: &fusedobject.ServiceConnectConfig{ResourceInput: &fusedobject.ResourceInputConfig{AllowedHosts: []string{"*.atlassian.net"}}},
	}
	resolution, err := resolveRuntimeEnvironment(metadata, "")
	credentials := map[string]any{"fused_resource_metadata": []byte(`{"your-domain":"acme"}`)}
	if err == nil {
		resolution, err = resolveRuntimeServerTemplate(metadata, resolution, credentials, nil)
	}
	if err != nil || resolution.BaseURL != "https://acme.atlassian.net" || resolution.Source != "connection_resource" {
		t.Fatalf("resolution=%+v err=%v", resolution, err)
	}
}

func TestResolveRuntimeServerTemplateRejectsDynamicHostOutsideAllowlist(t *testing.T) {
	defaultHost := "api.example.com"
	metadata := &fusedobject.ServiceMetadata{
		Servers: fusedobject.Servers{{
			URL: "https://{host}", IsDefault: true,
			Variables: []serverrouting.Variable{{Name: "host", Default: &defaultHost, Required: true}},
		}},
		ConnectConfig: &fusedobject.ServiceConnectConfig{ResourceInput: &fusedobject.ResourceInputConfig{AllowedHosts: []string{"*.example.com"}}},
	}
	resolution, err := resolveRuntimeEnvironment(metadata, "")
	if err == nil {
		resolution, err = resolveRuntimeServerTemplate(metadata, resolution, map[string]any{"fused_resource_metadata": []byte(`{"host":"evil.example.net"}`)}, nil)
	}
	if err == nil {
		t.Fatalf("unexpected resolution outside allowlist: %+v", resolution)
	}
}

func TestResolveRuntimeServerTemplateAppliesForcedBaseURLBeforeVariables(t *testing.T) {
	metadata := &fusedobject.ServiceMetadata{
		Servers:       fusedobject.Servers{{URL: "https://{tenant}.example.com", IsDefault: true, Variables: []serverrouting.Variable{{Name: "tenant", Required: true}}}},
		ConnectConfig: &fusedobject.ServiceConnectConfig{ResourceInput: &fusedobject.ResourceInputConfig{AllowedHosts: []string{"*.example.com"}}},
	}
	resolution, err := resolveRuntimeEnvironment(metadata, "")
	bindings := []store.BucketValue{{Location: "base_url", SourceKind: "connection_resource", Mode: "force", Value: "https://acme.example.com"}}
	if err == nil {
		resolution, err = resolveRuntimeServerTemplate(metadata, resolution, nil, bindings)
	}
	if err != nil || resolution.BaseURL != "https://acme.example.com" || resolution.Source != "connection_resource" {
		t.Fatalf("resolution=%+v err=%v", resolution, err)
	}
}

func TestResolveRuntimeServerTemplateRequiresAbsoluteProtocolRelativeOverride(t *testing.T) {
	metadata := &fusedobject.ServiceMetadata{
		Servers: fusedobject.Servers{{URL: "//your-domain.atlassian.net", IsDefault: true}},
		ConnectConfig: &fusedobject.ServiceConnectConfig{ResourceDiscovery: &fusedobject.ResourceDiscoveryConfig{
			AllowedHosts: []string{"api.atlassian.com"},
		}},
	}
	resolution, err := resolveRuntimeEnvironment(metadata, "")
	if err == nil {
		resolution, err = resolveRuntimeServerTemplate(metadata, resolution, nil, nil)
	}
	if !errors.Is(err, errAbsoluteServerOverrideRequired) {
		t.Fatalf("resolution=%+v err=%v, want absolute override requirement", resolution, err)
	}

	binding := []store.BucketValue{{
		Location: "base_url", SourceKind: "connection_resource", Mode: "force",
		Value: "https://api.atlassian.com/ex/confluence/cloud-a",
	}}
	resolution, err = resolveRuntimeEnvironment(metadata, "")
	if err == nil {
		resolution, err = resolveRuntimeServerTemplate(metadata, resolution, nil, binding)
	}
	assertRuntimeServerResolution(t, resolution, err, binding[0].Value, "connection_resource")

	override := &fusedobject.ServiceMetadata{BaseURL: "https://acme.atlassian.net"}
	resolution, err = resolveRuntimeEnvironment(override, "")
	if err == nil {
		resolution, err = resolveRuntimeServerTemplate(override, resolution, nil, nil)
	}
	assertRuntimeServerResolution(t, resolution, err, override.BaseURL, "default")
}

func assertRuntimeServerResolution(t *testing.T, resolution RuntimeEnvironmentResolution, err error, wantURL, wantSource string) {
	t.Helper()
	if err != nil || resolution.BaseURL != wantURL || resolution.Source != wantSource {
		t.Fatalf("resolution=%+v err=%v, want URL=%q source=%q", resolution, err, wantURL, wantSource)
	}
}

func TestResolveRuntimeServerTemplateRequiresOverrideForPathRelativeSource(t *testing.T) {
	defaultTenant := "acme"
	metadata := &fusedobject.ServiceMetadata{Servers: fusedobject.Servers{{
		URL: "/wiki/{tenant}", IsDefault: true,
		Variables: []serverrouting.Variable{{Name: "tenant", Default: &defaultTenant, Required: true}},
	}}}
	resolution, err := resolveRuntimeEnvironment(metadata, "")
	if err == nil {
		resolution, err = resolveRuntimeServerTemplate(metadata, resolution, nil, nil)
	}
	if !errors.Is(err, errAbsoluteServerOverrideRequired) {
		t.Fatalf("resolution=%+v err=%v, want absolute override requirement", resolution, err)
	}
}

// TestEncodeRuntimeErrorPreservesReconnectContract verifies the gRPC stream
// carries only the safe identifiers needed to start a replacement session.
func TestEncodeRuntimeErrorPreservesReconnectContract(t *testing.T) {
	want := &ReconnectRequiredError{
		Code: reconnectRequiredCode, BucketID: uuid.NewString(), ServiceID: uuid.NewString(),
		EndUserRef: "customer-42", ConnectionID: uuid.NewString(), Reason: "refresh_token_rejected",
	}
	encoded := encodeRuntimeError(fmt.Errorf("resolve connected auth: %w", want))
	var got ReconnectRequiredError
	if err := json.Unmarshal([]byte(encoded), &got); err != nil {
		t.Fatalf("decode reconnect error %q: %v", encoded, err)
	}
	if got != *want {
		t.Fatalf("encoded reconnect contract = %#v, want %#v", got, *want)
	}
}

func TestResolveRuntimeEnvironmentDoesNotAliasProviderLabels(t *testing.T) {
	metadata := &fusedobject.ServiceMetadata{
		BaseURL: "https://api.example.test",
		Servers: fusedobject.Servers{
			{URL: "https://api.example.test", Environment: "prod", IsDefault: true},
		},
	}

	_, err := resolveRuntimeEnvironment(metadata, "production")
	var unsupported *EnvironmentNotSupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("expected EnvironmentNotSupportedError, got %T %v", err, err)
	}
	if len(unsupported.Available) != 1 || unsupported.Available[0] != "prod" {
		t.Fatalf("provider labels must be preserved, got %+v", unsupported.Available)
	}
}

func TestResolveRuntimeEnvironmentDefaultsDeterministically(t *testing.T) {
	tests := []struct {
		name     string
		metadata *fusedobject.ServiceMetadata
		wantURL  string
		wantEnv  string
	}{
		{
			name: "explicit provider default",
			metadata: &fusedobject.ServiceMetadata{
				BaseURL: "https://api.example.test",
				Servers: fusedobject.Servers{
					{URL: "https://sandbox.example.test", Environment: "sandbox"},
					{URL: "https://api.example.test", Environment: "live", IsDefault: true},
				},
			},
			wantURL: "https://api.example.test",
			wantEnv: "live",
		},
		{
			name: "unique base URL match",
			metadata: &fusedobject.ServiceMetadata{
				BaseURL: "https://api.example.test",
				Servers: fusedobject.Servers{
					{URL: "https://sandbox.example.test", Environment: "sandbox"},
					{URL: "https://api.example.test", Environment: "live"},
				},
			},
			wantURL: "https://api.example.test",
			wantEnv: "live",
		},
		{
			name: "single legacy server without environment",
			metadata: &fusedobject.ServiceMetadata{
				BaseURL: "https://api.example.test",
				Servers: fusedobject.Servers{{URL: "https://api.example.test", Description: "Legacy prose"}},
			},
			wantURL: "https://api.example.test",
			wantEnv: "",
		},
		{
			name: "legacy base URL only",
			metadata: &fusedobject.ServiceMetadata{
				BaseURL: "https://api.example.test",
			},
			wantURL: "https://api.example.test",
			wantEnv: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolved, err := resolveRuntimeEnvironment(test.metadata, "")
			if err != nil {
				t.Fatalf("resolveRuntimeEnvironment: %v", err)
			}
			if resolved.BaseURL != test.wantURL || resolved.Environment != test.wantEnv || resolved.Source != "default" {
				t.Fatalf("unexpected default resolution: %+v", resolved)
			}
		})
	}
}

func TestResolveRuntimeEnvironmentRejectsAmbiguousDefault(t *testing.T) {
	metadata := &fusedobject.ServiceMetadata{
		BaseURL: "",
		Servers: fusedobject.Servers{
			{URL: "https://a.example.test", Environment: "a"},
			{URL: "https://b.example.test", Environment: "b"},
		},
	}

	_, err := resolveRuntimeEnvironment(metadata, "")
	var missing *DefaultEnvironmentNotConfiguredError
	if !errors.As(err, &missing) {
		t.Fatalf("expected DefaultEnvironmentNotConfiguredError, got %T %v", err, err)
	}
	if missing.Code != "default_environment_not_configured" || len(missing.Available) != 2 {
		t.Fatalf("unexpected structured default error: %+v", missing)
	}
}

func TestResolveRuntimeEnvironmentRejectsLabeledPlusUnlabeledDefaultAmbiguity(t *testing.T) {
	metadata := &fusedobject.ServiceMetadata{
		BaseURL: "",
		Servers: fusedobject.Servers{
			{URL: "https://api.example.test", Environment: "prod"},
			{URL: "https://regional.example.test", Description: "Regional prose only"},
		},
	}

	_, err := resolveRuntimeEnvironment(metadata, "")
	var missing *DefaultEnvironmentNotConfiguredError
	if !errors.As(err, &missing) {
		t.Fatalf("expected DefaultEnvironmentNotConfiguredError, got %T %v", err, err)
	}
	if len(missing.Available) != 1 || missing.Available[0] != "prod" {
		t.Fatalf("available values should include explicit labels only, got %+v", missing.Available)
	}
}

func TestResolveRuntimeEnvironmentRejectsDuplicateDefaults(t *testing.T) {
	metadata := &fusedobject.ServiceMetadata{
		BaseURL: "https://api.example.test",
		Servers: fusedobject.Servers{
			{URL: "https://api.example.test", Environment: "prod", IsDefault: true},
			{URL: "https://api2.example.test", Environment: "prod2", IsDefault: true},
		},
	}

	_, err := resolveRuntimeEnvironment(metadata, "")
	var missing *DefaultEnvironmentNotConfiguredError
	if !errors.As(err, &missing) {
		t.Fatalf("expected DefaultEnvironmentNotConfiguredError, got %T %v", err, err)
	}
}

func TestResolveRuntimeEnvironmentRejectsDescriptionOnlyNamedEnvironment(t *testing.T) {
	metadata := &fusedobject.ServiceMetadata{
		BaseURL: "https://api.example.test",
		Servers: fusedobject.Servers{
			{URL: "https://api.example.test", Description: "sandbox"},
		},
	}

	_, err := resolveRuntimeEnvironment(metadata, "sandbox")
	var unsupported *EnvironmentNotSupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("description-only servers are not selectable, got %T %v", err, err)
	}
	if len(unsupported.Available) != 0 {
		t.Fatalf("description-only labels must not appear as available, got %+v", unsupported.Available)
	}
}

// TestSelectedConnectedResourceDoesNotMutateBaseURL verifies resource
// selection is observable but routing changes only through a bucket binding.
func TestSelectedConnectedResourceDoesNotMutateBaseURL(t *testing.T) {
	service := &models.Service{BaseURL: "https://api.example.test"}
	source := selectedConnectedResourceSource(map[string]any{
		"fused_resource_id":       uuid.NewString(),
		"fused_resource_base_url": "https://tenant.example.test",
	})
	if source != "connection_resource" || service.BaseURL != "https://api.example.test" {
		t.Fatalf("resource selection bypassed binding engine: source=%q service=%#v", source, service)
	}
}

func TestApplyOperationRuntimeServerUsesWorkspaceVariables(t *testing.T) {
	defaultRegion := "us"
	metadata := &fusedobject.ServiceMetadata{
		ServerVariables: map[string]string{"region": "eu"},
		ConnectConfig: &fusedobject.ServiceConnectConfig{ResourceInput: &fusedobject.ResourceInputConfig{
			AllowedHosts: []string{"api.example.test"},
		}},
	}
	service := &models.Service{BaseURL: "https://api.example.test", ServerSource: "service"}
	operation := &models.IntegrationObject{OperationServers: models.Servers{{
		URL: "/{region}/v2", IsDefault: true,
		Variables: []serverrouting.Variable{{Name: "region", Default: &defaultRegion}},
	}}}
	if err := applyOperationRuntimeServer(metadata, service, operation, RuntimeEnvironmentResolution{}, nil, nil); err != nil {
		t.Fatalf("applyOperationRuntimeServer: %v", err)
	}
	if service.BaseURL != "https://api.example.test/eu/v2" || service.ServerSource != "operation" {
		t.Fatalf("service = %#v", service)
	}
}

// TestConnectedResourceRequirementRejectsCallerRoutingFields proves only the
// opaque resource ID survives from SDK input into the resolver boundary.
func TestConnectedResourceRequirementRejectsCallerRoutingFields(t *testing.T) {
	credentials := withConnectedResourceRequirement(map[string]any{
		"fused_resource_id":       "resource-id",
		"fused_resource_base_url": "https://attacker.example.test",
		"fused_resource_type":     "forged",
		"fused_connection_id":     "connection-id",
	}, &fusedobject.ServiceConnectConfig{})
	if credentials["fused_resource_id"] != "resource-id" || credentials["fused_resource_required"] != "true" {
		t.Fatalf("public selectors were not preserved: %#v", credentials)
	}
	if _, exists := credentials["fused_resource_base_url"]; exists {
		t.Fatalf("caller resource URL reached resolver boundary: %#v", credentials)
	}
	if _, exists := credentials["fused_resource_type"]; exists {
		t.Fatalf("caller resource type reached resolver boundary: %#v", credentials)
	}
	if _, exists := credentials["fused_connection_id"]; exists {
		t.Fatalf("caller connection ID reached resolver boundary: %#v", credentials)
	}
}
