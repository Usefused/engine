package connectionprofile

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

const (
	maxMetadataKeys = 32
	maxBindings     = 128
)

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)
	jsonPathPattern   = regexp.MustCompile(`^\$(?:\[\*\])?(?:\.[A-Za-z_][A-Za-z0-9_-]*)+$`)
	headerNamePattern = regexp.MustCompile(`^[!#$%&'*+\-.^_` + "`" + `|~0-9A-Za-z]+$`)
)

var protectedHeaders = map[string]struct{}{
	"authorization": {}, "proxy-authorization": {}, "host": {}, "cookie": {},
	"set-cookie": {}, "content-length": {}, "connection": {}, "keep-alive": {},
	"proxy-authenticate": {}, "te": {}, "trailer": {}, "transfer-encoding": {}, "upgrade": {},
}

// ValidationError makes structured issues usable through existing error-only
// importer interfaces while preserving the result for GraphQL and CLI callers.
type ValidationError struct {
	Issues []Issue
}

// Error returns the first structural issue so legacy error-only callers stay
// concise while richer GraphQL callers can inspect the complete issue list.
func (e *ValidationError) Error() string {
	if len(e.Issues) == 0 {
		return "connection profile validation failed"
	}
	return fmt.Sprintf("connection profile validation failed at %s: %s", e.Issues[0].Field, e.Issues[0].Message)
}

// Err exposes only blocking issues because warnings should not make valid
// provider extensions fail publication or workspace planning.
func (r Result) Err() error {
	if !r.HasErrors() {
		return nil
	}
	errorsOnly := make([]Issue, 0, len(r.Issues))
	for _, issue := range r.Issues {
		if issue.Severity == SeverityError {
			errorsOnly = append(errorsOnly, issue)
		}
	}
	return &ValidationError{Issues: errorsOnly}
}

// Validate applies the same profile checks for imports, workspace config,
// GraphQL, CLI, and UI writes. An empty Contract still performs all checks
// that do not depend on a pinned service version.
func Validate(profile *Profile, contract Contract) Result {
	Normalize(profile)
	validator := profileValidator{profile: profile, contract: contract}
	validator.validateTopLevel()
	validator.validateDiscovery()
	validator.validateInput()
	validator.validateMetadata()
	validator.validateBindings()
	return Result{Issues: validator.issues}
}

// Normalize persists deterministic defaults before hashing or storing a
// profile, keeping import, GraphQL, CLI, and runtime behavior identical.
func Normalize(profile *Profile) {
	if profile == nil {
		return
	}
	if canonical := CanonicalAuthType(profile.AuthType); canonical != "" {
		profile.AuthType = canonical
	}
	profile.AuthName = strings.TrimSpace(profile.AuthName)
	profile.OAuth2Flow = strings.TrimSpace(profile.OAuth2Flow)
	if profile.ResourceDiscovery == nil {
		return
	}
	if profile.ResourceDiscovery.Version == 0 {
		profile.ResourceDiscovery.Version = ResourceDiscoveryVersion
	}
	if strings.TrimSpace(profile.ResourceDiscovery.AutoRun) == "" {
		profile.ResourceDiscovery.AutoRun = "after_oauth_callback"
	}
	if strings.TrimSpace(profile.ResourceDiscovery.Stage) == "" {
		profile.ResourceDiscovery.Stage = "post_auth"
	}
	if strings.TrimSpace(profile.ResourceDiscovery.Lifecycle) == "" {
		profile.ResourceDiscovery.Lifecycle = "authoritative"
	}
}

type profileValidator struct {
	profile  *Profile
	contract Contract
	issues   []Issue
}

// validateTopLevel establishes invariants shared by discovery and input based
// connected resources before their specialized checks run.
func (v *profileValidator) validateTopLevel() {
	if v.profile == nil {
		return
	}
	v.validateAuthIdentity()
	if v.profile.ResourceDiscovery != nil && v.profile.ResourceInput != nil {
		v.addError("resource_source.conflict", "resource_discovery", "resource discovery and resource input are mutually exclusive")
	}
}

func (v *profileValidator) validateAuthIdentity() {
	if CanonicalAuthType(v.profile.AuthType) == "" {
		v.addError("auth_type.invalid", "auth_type", "auth type must be oauth or oidc")
		return
	}
	if !validAuthName(v.profile.AuthName) {
		v.addError("auth_name.invalid", "auth_name", "auth name is invalid")
		return
	}
	if !validOAuth2FlowSelection(v.profile.AuthType, v.profile.OAuth2Flow) {
		v.addError("oauth2_flow.invalid", "oauth2_flow", "OAuth2 flow must be a supported flow name and is only valid for OAuth")
		return
	}
	if v.contract.Complete {
		v.validatePinnedAuthIdentity(authConfigsForType(v.profile.AuthType, v.contract.AuthConfigs))
	}
}

func validOAuth2FlowSelection(authType, flow string) bool {
	if flow == "" {
		return true
	}
	if CanonicalAuthType(authType) != "oauth" {
		return false
	}
	switch flow {
	case "implicit", "password", "clientCredentials", "authorizationCode":
		return true
	default:
		return false
	}
}

func (v *profileValidator) validatePinnedAuthIdentity(matches []AuthConfig) {
	if len(matches) == 0 {
		v.addError("auth_type.unavailable", "auth_type", "auth type is not available on the pinned service version")
		return
	}
	if v.profile.AuthName == "" {
		if len(matches) > 1 {
			v.addError("auth_name.required", "auth_name", "auth name is required when several schemes use this auth type")
		}
		if len(matches) == 1 {
			v.validateOAuth2Flow(matches[0])
		}
		return
	}
	if !authNameAvailable(v.profile.AuthName, matches) {
		v.addError("auth_name.unavailable", "auth_name", "auth name is not available for the selected auth type")
		return
	}
	for _, match := range matches {
		if match.Name == v.profile.AuthName {
			v.validateOAuth2Flow(match)
			return
		}
	}
}

func (v *profileValidator) validateOAuth2Flow(config AuthConfig) {
	selected := strings.TrimSpace(v.profile.OAuth2Flow)
	if len(config.OAuth2Flows) == 0 {
		if selected != "" {
			v.addError("oauth2_flow.unavailable", "oauth2_flow", "OAuth2 flow is not declared by the selected auth scheme")
		}
		return
	}
	if selected == "" && len(config.OAuth2Flows) > 1 {
		v.addError("oauth2_flow.required", "oauth2_flow", "OAuth2 flow is required when the selected auth scheme declares alternatives")
		return
	}
	if selected != "" && !contains(config.OAuth2Flows, selected) {
		v.addError("oauth2_flow.unavailable", "oauth2_flow", "OAuth2 flow is not declared by the selected auth scheme")
	}
}

func validAuthName(name string) bool {
	return len(name) <= 128 && !strings.ContainsAny(name, "\r\n\x00")
}

// validateDiscovery delegates independent concerns to keep each decision path
// small and aligned with its runtime owner.
func (v *profileValidator) validateDiscovery() {
	if v.profile == nil || v.profile.ResourceDiscovery == nil {
		return
	}
	discovery := v.profile.ResourceDiscovery
	v.validateDiscoveryIdentity(discovery)
	v.validateDiscoveryRouting(discovery)
	v.validateDiscoveryOperation(discovery)
}

// validateDiscoveryIdentity keeps extraction and lifecycle options within the
// deliberately small feature set implemented by Engine.
func (v *profileValidator) validateDiscoveryIdentity(discovery *ResourceDiscoveryConfig) {
	if discovery.Version != ResourceDiscoveryVersion {
		v.addError("discovery.version.invalid", "resource_discovery.version", "version must be 1")
	}
	if strings.TrimSpace(discovery.OperationID) == "" || strings.TrimSpace(discovery.IDPath) == "" || strings.TrimSpace(discovery.ResourceType) == "" {
		v.addError("discovery.required", "resource_discovery", "operation_id, id_path, and resource_type are required")
	}
	v.validateJSONPath("resource_discovery.id_path", discovery.IDPath, true)
	v.validateJSONPath("resource_discovery.name_path", discovery.NamePath, false)
	v.validateJSONPath("resource_discovery.base_url_path", discovery.BaseURLPath, false)
	v.validateJSONPath("resource_discovery.scopes_path", discovery.ScopesPath, false)
	if discovery.AutoRun != "after_oauth_callback" {
		v.addError("discovery.auto_run.invalid", "resource_discovery.auto_run", "auto_run must be after_oauth_callback")
	}
	if discovery.Stage != "post_auth" {
		v.addError("discovery.stage.invalid", "resource_discovery.stage", "stage must be post_auth")
	}
	if discovery.Lifecycle != "authoritative" {
		v.addError("discovery.lifecycle.invalid", "resource_discovery.lifecycle", "lifecycle must be authoritative")
	}
}

// validateDiscoveryRouting allows context-only resources to retain a static
// service host while requiring an allowlist for dynamic dispatch URLs.
func (v *profileValidator) validateDiscoveryRouting(discovery *ResourceDiscoveryConfig) {
	if discovery.BaseURLPath == "" && discovery.BaseURLTemplate == "" && hasBaseURLBinding(v.profile.Bindings) {
		v.addError("discovery.base_url.required", "resource_discovery", "base_url_path or base_url_template is required for dynamic routing")
	}
	if (discovery.BaseURLPath != "" || discovery.BaseURLTemplate != "") && len(discovery.AllowedHosts) == 0 {
		v.addError("discovery.allowed_hosts.required", "resource_discovery.allowed_hosts", "allowed_hosts is required for dynamic routing")
	}
	v.validateHosts("resource_discovery.allowed_hosts", discovery.AllowedHosts)
}

// validateDiscoveryOperation binds token-bearing discovery to a pinned GET
// operation rather than a free-form profile URL.
func (v *profileValidator) validateDiscoveryOperation(discovery *ResourceDiscoveryConfig) {
	if discovery.Server != "" && v.contract.Complete && !contains(v.contract.Servers, discovery.Server) {
		v.addError("discovery.server.unknown", "resource_discovery.server", "named server does not exist on the pinned service version")
	}
	if !v.contract.Complete || discovery.OperationID == "" {
		return
	}
	operation, ok := operationByID(v.contract.Operations, discovery.OperationID)
	if !ok {
		v.addError("discovery.operation.unknown", "resource_discovery.operation_id", "operation does not exist on the pinned service version")
		return
	}
	if !strings.EqualFold(operation.Method, http.MethodGet) {
		v.addError("discovery.operation.method", "resource_discovery.operation_id", "discovery operation must use GET")
	}
}

// validateInput treats tenant input as constrained template data instead of a
// caller-controlled provider URL.
func (v *profileValidator) validateInput() {
	if v.profile == nil || v.profile.ResourceInput == nil {
		return
	}
	input := v.profile.ResourceInput
	if len(input.Fields) == 0 || strings.TrimSpace(input.BaseURLTemplate) == "" || strings.TrimSpace(input.ResourceType) == "" {
		v.addError("resource_input.required", "resource_input", "fields, base_url_template, and resource_type are required")
	}
	seen := v.validateInputFields(input.Fields)
	v.validateInputRouting(input, seen)
}

// validateInputFields compiles patterns at configuration time and returns the
// declared names needed for template validation.
func (v *profileValidator) validateInputFields(fields []ResourceInputField) map[string]struct{} {
	seen := map[string]struct{}{}
	for index, field := range fields {
		path := fmt.Sprintf("resource_input.fields[%d]", index)
		if !identifierPattern.MatchString(field.Name) {
			v.addError("resource_input.name.invalid", path+".name", "field name is invalid")
		}
		if _, ok := seen[field.Name]; ok {
			v.addError("resource_input.name.duplicate", path+".name", "field name must be unique")
		}
		seen[field.Name] = struct{}{}
		if field.Pattern != "" {
			if _, err := regexp.Compile(field.Pattern); err != nil {
				v.addError("resource_input.pattern.invalid", path+".pattern", "field pattern is invalid")
			}
		}
	}
	return seen
}

// validateInputRouting applies the same trusted-host boundary used for URLs
// returned by provider discovery.
func (v *profileValidator) validateInputRouting(input *ResourceInputConfig, fields map[string]struct{}) {
	v.validateInputTemplate(input, fields)
	if input.BaseURLTemplate != "" && len(input.AllowedHosts) == 0 {
		v.addError("resource_input.allowed_hosts.required", "resource_input.allowed_hosts", "allowed_hosts is required for dynamic routing")
	}
	v.validateHosts("resource_input.allowed_hosts", input.AllowedHosts)
}

// validateInputTemplate prevents undeclared placeholders from surviving into
// malformed or unexpectedly broad runtime URLs.
func (v *profileValidator) validateInputTemplate(input *ResourceInputConfig, fields map[string]struct{}) {
	for _, placeholder := range templatePlaceholders(input.BaseURLTemplate) {
		if _, ok := fields[placeholder]; !ok {
			v.addError("resource_input.template.unknown_field", "resource_input.base_url_template", "base URL template references an undeclared field")
		}
	}
}

// validateMetadata bounds and sorts extraction paths so publication output and
// resulting hashes stay deterministic.
func (v *profileValidator) validateMetadata() {
	if v.profile == nil {
		return
	}
	if len(v.profile.Metadata) > maxMetadataKeys {
		v.addError("metadata.limit", "metadata", "metadata key limit exceeded")
	}
	keys := make([]string, 0, len(v.profile.Metadata))
	for key := range v.profile.Metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !identifierPattern.MatchString(key) {
			v.addError("metadata.key.invalid", "metadata", "metadata key is invalid")
		}
		v.validateJSONPath("metadata."+key, v.profile.Metadata[key], true)
	}
}

// validateBindings compiles sources once and detects conflicting targets per
// operation before dispatch has to choose an ordering.
func (v *profileValidator) validateBindings() {
	if v.profile == nil {
		return
	}
	if len(v.profile.Bindings) > maxBindings {
		v.addError("binding.limit", "bindings", "binding limit exceeded")
	}
	seen := map[string]struct{}{}
	forcedBaseURL := map[string]struct{}{}
	for index, binding := range v.profile.Bindings {
		path := fmt.Sprintf("bindings[%d]", index)
		expression, err := ParseExpression(binding.Value)
		if err != nil {
			v.addError("binding.value.invalid", path+".value", "value must be a literal, $ENV, or complete resource expression")
		}
		v.validateBindingShape(path, binding, expression)
		operations := v.bindingOperations(path, binding)
		v.validateBindingTargets(path, binding, operations)
		v.detectBindingDuplicates(path, binding, operations, seen, forcedBaseURL)
	}
}

// validateBindingShape separates transport target rules from the expression
// rules that populate those targets.
func (v *profileValidator) validateBindingShape(path string, binding Binding, expression Expression) {
	v.validateBindingTargetShape(path, binding)
	v.validateBindingSourceShape(path, binding, expression)
}

// validateBindingTargetShape keeps location and name checks reusable across
// literal, environment, and connected-resource sources.
func (v *profileValidator) validateBindingTargetShape(path string, binding Binding) {
	v.validateBindingLocationMode(path, binding)
	v.validateBindingName(path, binding)
}

// validateBindingLocationMode limits profiles to dispatch behavior shared by
// every generated SDK runtime.
func (v *profileValidator) validateBindingLocationMode(path string, binding Binding) {
	if !contains([]string{"base_url", "header", "query", "path", "body"}, binding.Location) {
		v.addError("binding.location.invalid", path+".location", "location is invalid")
	}
	if binding.Mode != "default" && binding.Mode != "force" {
		v.addError("binding.mode.invalid", path+".mode", "mode must be default or force")
	}
}

// validateBindingName rejects ambiguous and protected transport names before a
// resource-derived value can reach an outbound request.
func (v *profileValidator) validateBindingName(path string, binding Binding) {
	if binding.Location != "base_url" && strings.TrimSpace(binding.Name) == "" {
		v.addError("binding.name.required", path+".name", "name is required outside base_url bindings")
	}
	if strings.ContainsAny(binding.Name, "\r\n") || (binding.Location == "header" && !headerNamePattern.MatchString(binding.Name)) {
		v.addError("binding.name.invalid", path+".name", "binding name is invalid")
	}
	if binding.Location == "header" && IsProtectedHeader(binding.Name) {
		v.addError("binding.header.protected", path+".name", "protected transport header cannot be targeted")
	}
}

// validateBindingSourceShape reserves base URL overrides for trusted resources
// and prevents dynamic provider data from rewriting request bodies.
func (v *profileValidator) validateBindingSourceShape(path string, binding Binding, expression Expression) {
	if expression.Kind == SourceConnectionResource && binding.Location == "body" {
		v.addError("binding.dynamic_body.unsupported", path+".location", "dynamic body bindings are not supported")
	}
	if binding.Location == "base_url" && (binding.Mode != "force" || expression.SourcePath != "base_url") {
		v.addError("binding.base_url.invalid", path, "base URL bindings must be forced and resource-derived")
	}
	if strings.HasPrefix(expression.SourcePath, "metadata.") {
		key := strings.TrimPrefix(expression.SourcePath, "metadata.")
		if _, ok := v.profile.Metadata[key]; !ok {
			v.addError("binding.metadata.unknown", path+".value", "dynamic metadata source is not declared by the profile")
		}
	}
}

// bindingOperations resolves applicability once so target and duplicate checks
// use the same pinned operation set.
func (v *profileValidator) bindingOperations(path string, binding Binding) []Operation {
	if len(v.contract.Operations) == 0 {
		if v.contract.Complete && len(binding.Operations) > 0 {
			v.addError("binding.operation.unknown", path+".operations", "binding operation does not exist on the pinned service version")
		}
		return nil
	}
	if len(binding.Operations) == 0 {
		return v.contract.Operations
	}
	operations := make([]Operation, 0, len(binding.Operations))
	for _, id := range binding.Operations {
		operation, ok := operationByID(v.contract.Operations, id)
		if !ok {
			v.addError("binding.operation.unknown", path+".operations", "binding operation does not exist on the pinned service version")
			continue
		}
		operations = append(operations, operation)
	}
	return operations
}

// validateBindingTargets distinguishes required path parameters from explicit
// provider extensions absent from an imported contract.
func (v *profileValidator) validateBindingTargets(path string, binding Binding, operations []Operation) {
	if binding.Location == "base_url" || binding.Location == "body" || len(operations) == 0 {
		return
	}
	for _, operation := range operations {
		if parameterExists(operation.Parameters, binding.Location, binding.Name) {
			continue
		}
		if binding.Location == "path" {
			v.addError("binding.path.unknown", path+".name", "path target is absent from an applicable operation")
			continue
		}
		if !binding.ProviderExtension {
			v.addWarning("binding.target.extension", path+".name", "target is absent from an applicable operation contract")
		}
	}
}

// detectBindingDuplicates keys targets by operation so JSON ordering cannot
// decide which conflicting value wins at runtime.
func (v *profileValidator) detectBindingDuplicates(path string, binding Binding, operations []Operation, seen, forcedBaseURL map[string]struct{}) {
	operationIDs := binding.Operations
	if len(operationIDs) == 0 {
		operationIDs = []string{"*"}
	}
	if len(operations) > 0 && len(binding.Operations) == 0 {
		operationIDs = make([]string, 0, len(operations))
		for _, operation := range operations {
			operationIDs = append(operationIDs, operation.ID)
		}
	}
	for _, operationID := range operationIDs {
		key := operationID + "\x00" + binding.Location + "\x00" + strings.ToLower(binding.Name)
		if _, ok := seen[key]; ok {
			v.addError("binding.duplicate", path, "duplicate binding target for an applicable operation")
		}
		seen[key] = struct{}{}
		if binding.Location == "base_url" && binding.Mode == "force" {
			if _, ok := forcedBaseURL[operationID]; ok {
				v.addError("binding.base_url.duplicate", path, "only one forced base URL binding may apply to an operation")
			}
			forcedBaseURL[operationID] = struct{}{}
		}
	}
}

// validateJSONPath accepts only the extraction subset Engine implements,
// keeping publication and runtime interpretation aligned.
func (v *profileValidator) validateJSONPath(field, path string, required bool) {
	if path == "" && !required {
		return
	}
	if !jsonPathPattern.MatchString(path) {
		v.addError("jsonpath.unsupported", field, "JSONPath uses an unsupported form")
	}
}

// validateHosts accepts exact hosts or one leading wildcard and rejects URL
// syntax that would make an allowlist entry ambiguous.
func (v *profileValidator) validateHosts(field string, hosts []string) {
	for _, host := range hosts {
		trimmed := strings.TrimSpace(strings.ToLower(host))
		if trimmed == "" || strings.ContainsAny(trimmed, "/:@?#\r\n") || strings.Count(trimmed, "*") > 1 || (strings.Contains(trimmed, "*") && !strings.HasPrefix(trimmed, "*.")) {
			v.addError("allowed_host.invalid", field, "allowed host must be an exact host or wildcard subdomain")
		}
	}
}

// addError centralizes the stable issue shape consumed by every config ingress.
func (v *profileValidator) addError(code, field, message string) {
	v.issues = append(v.issues, Issue{Code: code, Field: field, Message: message, Severity: SeverityError})
}

// addWarning records useful contract drift without blocking an explicit
// provider extension.
func (v *profileValidator) addWarning(code, field, message string) {
	v.issues = append(v.issues, Issue{Code: code, Field: field, Message: message, Severity: SeverityWarning})
}

// CanonicalAuthType collapses Registry spellings to the two connected auth
// families stored by profiles and selected by Engine.
func CanonicalAuthType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "oauth", "oauth2":
		return "oauth"
	case "oidc", "openidconnect", "open_id_connect":
		return "oidc"
	default:
		return ""
	}
}

// authTypeAvailable compares canonical families so Engine never falls back to
// whichever auth declaration happens to be first.
func authConfigsForType(profileType string, available []AuthConfig) []AuthConfig {
	want := CanonicalAuthType(profileType)
	matches := make([]AuthConfig, 0, len(available))
	for _, auth := range available {
		if CanonicalAuthType(auth.Type) == want {
			matches = append(matches, auth)
		}
	}
	return matches
}

func authNameAvailable(name string, available []AuthConfig) bool {
	for _, auth := range available {
		if auth.Name == name {
			return true
		}
	}
	return false
}

// operationByID searches the already bounded version contract, not a broader
// service catalogue or database result.
func operationByID(operations []Operation, id string) (Operation, bool) {
	for _, operation := range operations {
		if operation.ID == id {
			return operation, true
		}
	}
	return Operation{}, false
}

// parameterExists keeps location as part of identity while honoring HTTP's
// case-insensitive header naming.
func parameterExists(parameters []Parameter, location, name string) bool {
	for _, parameter := range parameters {
		if parameter.Location == location && strings.EqualFold(parameter.Name, name) {
			return true
		}
	}
	return false
}

// contains serves bounded configuration enums where a map would add state
// without reducing database or network work.
func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// hasBaseURLBinding identifies profiles that require discovered routing rather
// than static-host resource context.
func hasBaseURLBinding(bindings []Binding) bool {
	for _, binding := range bindings {
		if binding.Location == "base_url" {
			return true
		}
	}
	return false
}

// templatePlaceholders extracts the closed set compared with declared input
// fields; it does not attempt general template evaluation.
func templatePlaceholders(template string) []string {
	matches := regexp.MustCompile(`\{([^{}]+)\}`).FindAllStringSubmatch(template, -1)
	values := make([]string, 0, len(matches))
	for _, match := range matches {
		values = append(values, match[1])
	}
	return values
}

// IsProtectedHeader is shared by publication and dispatch so neither boundary
// can replace host, credential, cookie, or framing headers.
func IsProtectedHeader(name string) bool {
	_, ok := protectedHeaders[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// ValidateResolvedHeader is repeated at dispatch because dynamic metadata is
// unavailable during profile publication. Keeping the rule here prevents the
// static and runtime validators from drifting on transport protections.
func ValidateResolvedHeader(name, value string) error {
	if !headerNamePattern.MatchString(name) || strings.ContainsAny(name, "\r\n") {
		return errors.New("resolved binding header name is invalid")
	}
	if IsProtectedHeader(name) {
		return errors.New("resolved binding cannot target a protected header")
	}
	if strings.ContainsAny(value, "\r\n") {
		return errors.New("resolved binding header value is invalid")
	}
	return nil
}

// ValidateLiteralBucketValue limits the legacy value API to non-routing
// request targets. Host selection requires the stronger profile contract and
// a selected connection resource.
func ValidateLiteralBucketValue(location, name, value string) error {
	location = strings.ToLower(strings.TrimSpace(location))
	if location == "base_url" {
		return errors.New("base_url must be configured through a connection profile")
	}
	// Literal bucket values may be bound to 'header', 'query', 'path', 'body', or
	// 'env' (which aliases to bucket variables mapped purely for variable injections
	// like ${bucket.env.X} without needing to map to an HTTP transport boundary).
	if !contains([]string{"header", "query", "path", "body", "env"}, location) {
		return errors.New("bucket value location is invalid")
	}
	if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\r\n") {
		return errors.New("bucket value name is invalid")
	}
	if location == "header" {
		return ValidateResolvedHeader(name, value)
	}
	return nil
}
