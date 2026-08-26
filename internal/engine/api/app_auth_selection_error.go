package api

import (
	"fmt"
	"strings"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

const appAuthExampleLimit = 5

// incompatibleAppAuthError explains the existing planner decision using its
// already-fetched contract, without querying credentials or changing auth policy.
func incompatibleAppAuthError(selection models.SDKSelection, auths fusedobject.AuthConfigs, operations []sandbox.OperationSecuritySummary) appServiceValidationError {
	preference := appAuthPreferenceLabel(selection)
	err := appServiceValidationError{
		serviceID: selection.ServiceID, code: "auth_selection_incompatible",
		reason: "has no authentication scheme compatible with every secured selected operation for " + preference + ". Operations within the same service can require different authentication methods.",
		remedy: "Select operations compatible with one exact auth.name (replace select_all: true with an explicit operations list), or omit auth to use the declared per-operation requirements and provide all required credentials. Review connect.scopes if changing auth.",
	}
	// Reuse the policy matcher with no operations to distinguish an unknown
	// selector from a real scheme that only supports part of the selection.
	candidates := compatibleAppAuths(auths, nil, selection.AuthType, selection.AuthName)
	if len(candidates) == 0 {
		err.code = "auth_selection_not_found"
		err.reason = "has no declared authentication scheme matching " + preference + ". Auth names are service-specific and case-sensitive."
		err.detail = "Declared auth: " + appAuthCandidateLabels(auths)
		err.remedy = "Set this service's auth.type and auth.name to a declared scheme, then plan again."
		return err
	}
	// A type-only selector can match several schemes with disjoint operation
	// coverage; attributing failures to an arbitrary candidate would mislead users.
	if len(candidates) != 1 {
		err.detail = "No single matching scheme covers the selection. Matching auth: " + appAuthCandidateLabels(candidates)
		return err
	}
	count, examples := incompatibleAppAuthExamples(operations, candidates[0].Name, auths)
	err.reason = fmt.Sprintf("selects %s, but %d selected operation(s) do not support it. Operations within the same service can require different authentication methods.", preference, count)
	err.detail = boundedAppAuthDetail("Incompatible operations (up to 5 shown): " + strings.Join(examples, "; "))
	return err
}

// appAuthPreferenceLabel identifies authored selectors, never resolved secrets
// or OAuth scopes, so users can locate the incorrect service-level preference.
func appAuthPreferenceLabel(selection models.SDKSelection) string {
	labels := []string{}
	// Absent selectors must not look like an explicitly authored empty value.
	if selection.AuthType != "" {
		labels = append(labels, "auth.type="+appAuthDiagnosticLabel(selection.AuthType))
	}
	// Exact names distinguish schemes sharing the same canonical auth type.
	if selection.AuthName != "" {
		labels = append(labels, "auth.name="+appAuthDiagnosticLabel(selection.AuthName))
	}
	// Scope-only preferences still ask the planner to find one common OAuth scheme.
	if len(labels) == 0 {
		return "the requested connect.scopes"
	}
	return strings.Join(labels, ", ")
}

// incompatibleAppAuthExamples counts every rejected operation but retains only
// a bounded sample, using the same compatibility predicate as auth resolution.
func incompatibleAppAuthExamples(operations []sandbox.OperationSecuritySummary, name string, auths fusedobject.AuthConfigs) (int, []string) {
	types := make(map[string]string, len(auths))
	// Auth definitions are already part of the batch; no per-operation lookup is needed.
	for _, auth := range auths {
		types[auth.Name] = sandbox.CanonicalFusedAuthType(auth)
	}
	count, examples := 0, []string{}
	for _, operation := range operations {
		// Anonymous and compatible alternatives must not be described as requiring a different auth.
		if operationPermitsAnonymous(operation) || operationSupportsAuth(operation, name) {
			continue
		}
		count++
		// Count remains exact even when the response sample has reached its budget.
		if len(examples) < appAuthExampleLimit {
			examples = append(examples, appAuthDiagnosticLabel(operation.Name)+" requires "+appAuthRequirementLabel(operation.SecurityRequirements, types))
		}
	}
	return count, examples
}

// appAuthRequirementLabel preserves OR-of-AND requirements instead of implying
// that any one scheme in an AND branch would be sufficient for execution.
func appAuthRequirementLabel(requirements authrouting.Requirements, types map[string]string) string {
	alternatives := []string{}
	for _, alternative := range requirements[:min(len(requirements), 3)] {
		names := []string{}
		for _, requirement := range alternative.Schemes[:min(len(alternative.Schemes), 3)] {
			names = append(names, appAuthSchemeLabel(requirement.Scheme, types[requirement.Scheme]))
		}
		// Truncation must not silently remove a mandatory member of an AND branch.
		if len(alternative.Schemes) > 3 {
			names = append(names, "[additional required schemes omitted]")
		}
		alternatives = append(alternatives, "("+strings.Join(names, " AND ")+")")
	}
	// Retain an explicit marker when more valid alternatives exist in the contract.
	if len(requirements) > 3 {
		alternatives = append(alternatives, "[additional alternatives omitted]")
	}
	return strings.Join(alternatives, " OR ")
}

// appAuthCandidateLabels exposes only bounded names/types, not the rest of the
// auth config, which can contain provider URLs or credential-bearing metadata.
func appAuthCandidateLabels(auths fusedobject.AuthConfigs) string {
	labels := []string{}
	for _, auth := range auths[:min(len(auths), appAuthExampleLimit)] {
		labels = append(labels, appAuthSchemeLabel(auth.Name, sandbox.CanonicalFusedAuthType(auth)))
	}
	// The catalogue remains authoritative when the response cannot list every scheme.
	if len(auths) > appAuthExampleLimit {
		labels = append(labels, "[additional schemes omitted]")
	}
	return boundedAppAuthDetail(strings.Join(labels, ", "))
}

// appAuthSchemeLabel uses the canonical config type alongside the exact scheme
// name, making OpenAPI types such as http/basic useful to config authors.
func appAuthSchemeLabel(name, authType string) string {
	label := safeAppValidationLabel(authType)
	// Unknown or unsafe types cannot be suggested as valid authored config values.
	if label == "" {
		label = "type unavailable"
	}
	return appAuthDiagnosticLabel(name) + " (" + label + ")"
}

// appAuthDiagnosticLabel reuses service-label admission to prevent provider
// metadata from becoming a credential echo or terminal-control channel.
func appAuthDiagnosticLabel(value string) string {
	label := safeAppValidationLabel(value)
	// Omit unsafe or unavailable labels rather than fabricate an exact config value.
	if label == "" {
		return "[label omitted]"
	}
	return fmt.Sprintf("%q", label)
}

// boundedAppAuthDetail fits the existing CLI diagnostic budget and marks partial
// text explicitly; operation counts in the primary message remain exact.
func boundedAppAuthDetail(value string) string {
	runes := []rune(value)
	// Leave headroom below the shared 1,024-rune CLI server-detail ceiling.
	if len(runes) > 900 {
		return string(runes[:900]) + "… [additional detail omitted]"
	}
	return value
}
