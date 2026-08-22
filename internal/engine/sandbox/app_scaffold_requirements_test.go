package sandbox

import (
	"reflect"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/connectionprofile"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/serverrouting"
)

// TestAppScaffoldRequiredServerVariablesMirrorsRuntimePrecedence proves the
// scaffold asks only for values that the eventual runtime cannot source.
func TestAppScaffoldRequiredServerVariablesMirrorsRuntimePrecedence(t *testing.T) {
	defaultRegion := "us"
	metadata := &fusedobject.ServiceMetadata{
		Servers: fusedobject.Servers{{
			URL: "https://api-{{app_id}}.sendbird.com", IsDefault: true,
			Variables: []serverrouting.Variable{{Name: "app_id", Required: true}},
		}},
		ConnectConfig: &fusedobject.ServiceConnectConfig{
			Metadata: map[string]string{"tenant": "metadata.tenant"},
			ResourceInput: &connectionprofile.ResourceInputConfig{Fields: []connectionprofile.ResourceInputField{
				{Name: "cluster"},
			}},
		},
	}
	endpoints := []fusedobject.Endpoint{{OperationServers: fusedobject.Servers{{
		URL: "https://{{region}}.example.com/{{tenant}}/{{cluster}}/{{workspace}}", IsDefault: true,
		Variables: []serverrouting.Variable{
			{Name: "region", Default: &defaultRegion}, {Name: "tenant"}, {Name: "cluster"}, {Name: "workspace"},
		},
	}}}}
	override := &store.WorkspaceExecutionPolicyOverride{ServerVariables: map[string]string{"workspace": "configured"}}

	got, err := AppScaffoldRequiredServerVariables(metadata, endpoints, override)
	// The only unsatisfied no-default variable is the Sendbird host anchor.
	if err != nil || !reflect.DeepEqual(got, []string{"app_id"}) {
		t.Fatalf("requirements = %#v, err %v", got, err)
	}
}

// TestAppScaffoldRequiredServerVariablesUsesEffectiveOperationServer proves
// the projector shares runtime environment/default selection semantics.
func TestAppScaffoldRequiredServerVariablesUsesEffectiveOperationServer(t *testing.T) {
	metadata := &fusedobject.ServiceMetadata{Servers: fusedobject.Servers{{
		URL: "https://api.example.com", Name: "production", IsDefault: true,
	}}}
	endpoints := []fusedobject.Endpoint{{OperationServers: fusedobject.Servers{
		{URL: "https://dev-{{dev_token}}.example.com", Name: "development", Variables: []serverrouting.Variable{{Name: "dev_token"}}},
		{URL: "https://prod-{{prod_token}}.example.com", Name: "production", Variables: []serverrouting.Variable{{Name: "prod_token"}}},
	}}}

	got, err := AppScaffoldRequiredServerVariables(metadata, endpoints, nil)
	// Only the operation server selected for the service's production environment participates.
	if err != nil || !reflect.DeepEqual(got, []string{"prod_token"}) {
		t.Fatalf("requirements = %#v, err %v", got, err)
	}
}

// TestAppScaffoldRequiredServerVariablesHonorsRoutingOverrides covers the two
// fixed-base-URL paths whose runtime effects intentionally differ.
func TestAppScaffoldRequiredServerVariablesHonorsRoutingOverrides(t *testing.T) {
	serviceVariable := []serverrouting.Variable{{Name: "app_id"}}
	operationVariable := []serverrouting.Variable{{Name: "region"}}
	endpoint := fusedobject.Endpoint{OperationServers: fusedobject.Servers{{
		URL: "https://{{region}}.example.com", IsDefault: true, Variables: operationVariable,
	}}}

	workspaceURL := "https://workspace.example.com"
	workspaceMetadata := &fusedobject.ServiceMetadata{Servers: fusedobject.Servers{{
		URL: "https://api-{{app_id}}.example.com", IsDefault: true, Variables: serviceVariable,
	}}}
	got, err := AppScaffoldRequiredServerVariables(workspaceMetadata, []fusedobject.Endpoint{endpoint}, &store.WorkspaceExecutionPolicyOverride{BaseURL: &workspaceURL})
	// A workspace base URL replaces the service template, but operation routing still runs.
	if err != nil || !reflect.DeepEqual(got, []string{"region"}) {
		t.Fatalf("workspace requirements = %#v, err %v", got, err)
	}

	resourceMetadata := &fusedobject.ServiceMetadata{
		Servers: workspaceMetadata.Servers,
		ConnectConfig: &fusedobject.ServiceConnectConfig{Bindings: []connectionprofile.Binding{{
			Location: "base_url", Mode: "force", Value: "${resource.base_url}",
		}}},
	}
	got, err = AppScaffoldRequiredServerVariables(resourceMetadata, []fusedobject.Endpoint{endpoint}, nil)
	// A reviewed connection-resource base URL bypasses service and operation templates.
	if err != nil || len(got) != 0 {
		t.Fatalf("resource requirements = %#v, err %v", got, err)
	}
}
