package sandbox

import (
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/serverrouting"
)

// TestOperationServerTemplateAcceptsChargebeeShapes covers each official routing
// authority without importing the provider's large operation catalogue into CI.
func TestOperationServerTemplateAcceptsChargebeeShapes(t *testing.T) {
	for _, host := range []string{"{site}.{environment}", "{site}.ingest.{environment}", "{site}.file-ingest.{environment}", "{site}.grow.{environment}", "{site}-test.ingest.{environment}"} {
		t.Run(host, func(t *testing.T) {
			server := fusedobject.Server{
				URL: "{protocol}://" + host + ":{port}/api/v2",
				Variables: []serverrouting.Variable{
					operationServerVariable("protocol", "https", "http", "https"),
					operationServerVariable("site", "demo"),
					operationServerVariable("environment", "chargebee.com", "chargebee.com"),
					operationServerVariable("port", "443", "443", "8080"),
				},
			}
			endpoint := fusedobject.Endpoint{Method: "GET", ProviderProtocol: models.ProviderProtocolREST, OperationServers: fusedobject.Servers{server}}
			// Full transport admission must reach the shared template-aware validator.
			if err := validateEndpointTransport(endpoint); err != nil {
				t.Fatalf("official routing shape rejected: %v", err)
			}
		})
	}
}

// TestOperationServerTemplatePreservesDeferredValues proves snapshot admission
// does not invent or persist a tenant value needed only by execution.
func TestOperationServerTemplatePreservesDeferredValues(t *testing.T) {
	server := fusedobject.Server{URL: "{protocol}://{tenant}.example.com:{port}/{version}", Variables: []serverrouting.Variable{
		{Name: "protocol", Required: true}, {Name: "tenant", Required: true}, {Name: "port", Required: true}, {Name: "version", Required: true},
	}}
	// Structurally valid templates remain importable before bucket setup.
	if err := validateOperationServer(server); err != nil {
		t.Fatalf("deferred variables rejected: %v", err)
	}
	for _, variable := range server.Variables {
		// Syntax-only validation must leave every authored default absent.
		if variable.Default != nil {
			t.Fatal("admission invented a persisted server default")
		}
	}
	// Actual dispatch still requires concrete tenant values from its normal sources.
	if _, _, err := serverrouting.ResolveReference(server.URL, server.Variables, nil); err == nil {
		t.Fatal("unresolved execution accepted after template admission")
	}
}

// TestOperationServerTemplateExecutesWithBucketTenant verifies the full operation
// routing boundary retains enum-bound provider domains while anchoring app input.
func TestOperationServerTemplateExecutesWithBucketTenant(t *testing.T) {
	for _, environment := range []string{"chargebee.com", "chargebee.eu"} {
		t.Run(environment, func(t *testing.T) {
			operation := &models.IntegrationObject{OperationServers: models.Servers{{
				URL: "{protocol}://{site}.ingest.{environment}:{port}/api/v2",
				Variables: []serverrouting.Variable{
					operationServerVariable("protocol", "https", "http", "https"),
					{Name: "site", Required: true},
					operationServerVariable("environment", "chargebee.com", "chargebee.com", "chargebee.eu"),
					operationServerVariable("port", "443", "443", "8080"),
				},
			}}}
			values := []store.BucketValue{
				{Location: serverVariableBindingLocation, SourceKind: "literal", KeyName: "site", Value: "acme", Mode: "force"},
				{Location: serverVariableBindingLocation, SourceKind: "literal", KeyName: "environment", Value: environment, Mode: "force"},
			}
			service := &models.Service{BaseURL: "https://demo.chargebee.com", ServerSource: "service"}
			resolution, err := applyOperationRuntimeServer(&fusedobject.ServiceMetadata{}, service, operation, RuntimeEnvironmentResolution{}, nil, values)
			// Defaults and the real bucket tenant must reach the same trusted route.
			if err != nil {
				t.Fatalf("operation routing failed: %v", err)
			}
			want := "https://acme.ingest." + environment + ":443/api/v2"
			// The real selected enum value, not its default, establishes the anchor.
			if service.BaseURL != want || resolution.Source != serverVariableSourceApp {
				t.Fatalf("unexpected routing: baseURL=%q source=%q", service.BaseURL, resolution.Source)
			}
		})
	}
}

// TestOperationServerTemplateRejectsWholeHostWithPort closes the case where a
// nonnumeric placeholder marker previously made URL parsing skip host anchoring.
func TestOperationServerTemplateRejectsWholeHostWithPort(t *testing.T) {
	operation := &models.IntegrationObject{OperationServers: models.Servers{{
		URL: "{protocol}://{host}:{port}/api/v2",
		Variables: []serverrouting.Variable{
			operationServerVariable("protocol", "https"),
			operationServerVariable("host", "api.example.com"),
			operationServerVariable("port", "443"),
		},
	}}}
	values := []store.BucketValue{{Location: serverVariableBindingLocation, SourceKind: "literal", KeyName: "host", Value: "evil.example", Mode: "force"}}
	service := &models.Service{BaseURL: "https://api.example.com", ServerSource: "service"}
	_, err := applyOperationRuntimeServer(&fusedobject.ServiceMetadata{}, service, operation, RuntimeEnvironmentResolution{}, nil, values)
	// A syntactically valid destination must still belong to a fixed provider domain.
	if err == nil {
		t.Fatal("unanchored dynamic hostname accepted")
	}
	// Failed trust checks must not replace the previously selected service origin.
	if service.BaseURL != "https://api.example.com" {
		t.Fatal("rejected operation changed the execution origin")
	}
}

// TestOperationServerTemplateRejectsDeclarationDrift keeps template substitution
// behind the existing one-to-one declaration and provider enum boundaries.
func TestOperationServerTemplateRejectsDeclarationDrift(t *testing.T) {
	tests := []struct {
		name   string
		server fusedobject.Server
	}{
		{name: "undeclared", server: fusedobject.Server{URL: "https://{tenant}.example.com"}},
		{name: "unused", server: fusedobject.Server{URL: "https://api.example.com", Variables: []serverrouting.Variable{{Name: "tenant"}}}},
		{name: "duplicate", server: fusedobject.Server{URL: "https://{tenant}.example.com", Variables: []serverrouting.Variable{{Name: "tenant"}, {Name: "tenant"}}}},
		{name: "default outside enum", server: fusedobject.Server{URL: "{protocol}://api.example.com", Variables: []serverrouting.Variable{operationServerVariable("protocol", "https", "http")}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Invalid declarations cannot be hidden by a validation representative.
			if err := validateOperationServer(test.server); err == nil {
				t.Fatal("invalid server declaration accepted")
			}
		})
	}
}

// operationServerVariable models an explicit provider default independently of
// whether the variable may later be supplied by bucket configuration.
func operationServerVariable(name, value string, enum ...string) serverrouting.Variable {
	return serverrouting.Variable{Name: name, Default: &value, Enum: enum, Required: true}
}
