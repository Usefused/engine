package sandbox

import (
	"sort"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/connectionprofile"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/serverrouting"
)

// AppScaffoldRequiredServerVariables projects only names that would remain
// unresolved when the selected operations later enter the shared runtime.
func AppScaffoldRequiredServerVariables(metadata *fusedobject.ServiceMetadata, endpoints []fusedobject.Endpoint, override *store.WorkspaceExecutionPolicyOverride) ([]string, error) {
	// Missing metadata is an incomplete immutable snapshot rather than an empty
	// requirement set, so callers must fail the whole scaffold read.
	if metadata == nil {
		return nil, store.ErrServiceContractSnapshotNotFound
	}
	// Reusing the dispatch merge keeps workspace base-URL, server clearing, and
	// server-variable inheritance semantics owned by the runtime policy layer.
	effective := mergeExecutionPolicyOverride(metadata, override)
	// A provider-reviewed resource base URL bypasses both service and operation
	// server templates at runtime, making every template variable irrelevant.
	if appScaffoldForcesResourceBaseURL(effective.ConnectConfig) {
		return []string{}, nil
	}
	satisfied := appScaffoldSatisfiedServerVariables(effective)
	required := make(map[string]struct{})
	resolution, err := resolveRuntimeEnvironment(effective, "")
	// The scaffold must mirror runtime failure when a provider declared no
	// unambiguous default service environment after workspace policy is merged.
	if err != nil {
		return nil, err
	}
	collectAppScaffoldServerVariables(required, satisfied, resolution.Variables)
	// Endpoint rows were already intersected with the requested app selection
	// in SQL, so this loop never filters a broader operation catalogue in Go.
	for _, endpoint := range endpoints {
		variables := appScaffoldOperationServerVariables(endpoint.OperationServers, resolution.Environment)
		collectAppScaffoldServerVariables(required, satisfied, variables)
	}
	names := make([]string, 0, len(required))
	// Sorting the set makes GraphQL output independent of snapshot declaration
	// order and stable across SDK and MCP initialization.
	for name := range required {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// appScaffoldForcesResourceBaseURL recognizes the same reviewed binding shape
// whose resolved BucketValue makes runtime skip operation-server routing.
func appScaffoldForcesResourceBaseURL(config *fusedobject.ServiceConnectConfig) bool {
	// Services without provider-native Connect behavior cannot bypass templates.
	if config == nil {
		return false
	}
	// Only the exact forced resource.base_url expression has the runtime trust
	// level needed to replace the complete provider routing surface.
	for _, binding := range config.Bindings {
		if binding.Location != "base_url" || binding.Mode != "force" {
			continue
		}
		expression, err := connectionprofile.ParseExpression(binding.Value)
		// Invalid expressions are rejected at snapshot admission; treating one as
		// non-forcing here keeps this read fail-closed if legacy data exists.
		if err == nil && expression.Kind == connectionprofile.SourceConnectionResource && expression.SourcePath == "base_url" {
			return true
		}
	}
	return false
}

// appScaffoldSatisfiedServerVariables gathers only declaration names, never
// connection values, so the initialization query remains credential-free.
func appScaffoldSatisfiedServerVariables(metadata *fusedobject.ServiceMetadata) map[string]struct{} {
	satisfied := make(map[string]struct{})
	// Effective metadata already contains the workspace-over-provider variable
	// precedence applied by mergeExecutionPolicyOverride.
	for name := range metadata.ServerVariables {
		satisfied[name] = struct{}{}
	}
	config := metadata.ConnectConfig
	// Without Connect metadata there are no provider-native resource values to
	// reserve during a later connection flow.
	if config == nil {
		return satisfied
	}
	// Discovery metadata keys become fused_resource_metadata at execution.
	for name := range config.Metadata {
		satisfied[name] = struct{}{}
	}
	// Resource input fields are persisted into the same metadata object after
	// the provider-owned validation and host-allowlist boundary succeeds.
	if config.ResourceInput != nil {
		for _, field := range config.ResourceInput.Fields {
			satisfied[field.Name] = struct{}{}
		}
	}
	return satisfied
}

// collectAppScaffoldServerVariables applies the resolver's actual fallback:
// a provider default resolves locally, while any no-default variable needs an
// external value regardless of a stale Required flag on older snapshots.
func collectAppScaffoldServerVariables(required, satisfied map[string]struct{}, variables []serverrouting.Variable) {
	// The selected server bounds this scan; unused environments are never added.
	for _, variable := range variables {
		// A default is already executable and a known external source is supplied
		// by workspace or provider-native connection metadata.
		if variable.Default != nil || appScaffoldVariableSatisfied(satisfied, variable.Name) {
			continue
		}
		required[variable.Name] = struct{}{}
	}
}

// appScaffoldVariableSatisfied keeps map membership readable at the decision
// site without leaking names into diagnostics or telemetry.
func appScaffoldVariableSatisfied(satisfied map[string]struct{}, name string) bool {
	_, ok := satisfied[name]
	return ok
}

// appScaffoldOperationServerVariables adapts the snapshot DTO to the existing
// runtime selector so name/default/environment precedence has one owner.
func appScaffoldOperationServerVariables(servers fusedobject.Servers, environment string) []serverrouting.Variable {
	// Operations without an override inherit only the already-selected service server.
	if len(servers) == 0 {
		return nil
	}
	runtimeServers := make(models.Servers, len(servers))
	// This is a bounded DTO conversion, not filtering; selection remains owned
	// by selectOperationServer exactly as it is during dispatch.
	for index, server := range servers {
		runtimeServers[index] = models.Server{
			URL: server.URL, Name: server.Name, Environment: server.Environment,
			IsDefault: server.IsDefault, Variables: server.Variables,
		}
	}
	selected, ok := selectOperationServer(runtimeServers, environment)
	// A non-empty server list always falls back to its first declaration, but
	// retain the guard so future selector changes cannot panic this read path.
	if !ok {
		return nil
	}
	return selected.Variables
}
