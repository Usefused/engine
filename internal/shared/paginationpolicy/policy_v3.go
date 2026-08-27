package paginationpolicy

import (
	"net"
	"net/url"
	"regexp"
	"strings"
)

const (
	maxGraphQLTemplate   = 65_536
	maxRequestSteps      = 32
	maxResponseValues    = 32
	maxContinuationSteps = 8
	maxConditionalPaths  = 16
	maxTerminationValues = 32
	maxGraphQLBindings   = 32
	maxAllowedOrigins    = 16
)

var stateNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,127}$`)

// validateV3 validates the full state graph in dependency order so a later
// continuation or termination step cannot reference undeclared state.
func validateV3(config *Config) error {
	if err := validateLimits(EffectiveLimits(config.Limits)); err != nil {
		return err
	}
	states, err := validateV3Request(config.Request)
	if err != nil {
		return err
	}
	values, err := validateV3Response(config.Response, states)
	if err != nil {
		return err
	}
	if err := validateV3Continuation(config.Continuation, states, values, v3ResponseSources(config.Response)); err != nil {
		return err
	}
	if err := validateV3Termination(config.Termination, states, values); err != nil {
		return err
	}
	return validateV3GraphQL(config.GraphQL, states, config.Request, config.Response)
}

func validateV3Request(steps []RequestStep) (map[string]ValueType, error) {
	if len(steps) > maxRequestSteps {
		return nil, invalid("pagination request plan is too large")
	}
	states := make(map[string]ValueType, len(steps))
	targets := make(map[string]map[PageApplication]struct{}, len(steps))
	for _, step := range steps {
		if err := validateV3RequestStep(step, states, targets); err != nil {
			return nil, err
		}
	}
	return states, nil
}

func validateV3RequestStep(step RequestStep, states map[string]ValueType, targets map[string]map[PageApplication]struct{}) error {
	if !validV3ValueType(step.ValueType) || !validPageApplication(step.Apply) || !validV3Target(step.Target) {
		return invalid("pagination request step is invalid")
	}
	if err := validateV3RequestValue(step); err != nil {
		return err
	}
	if err := addV3RequestTarget(step, targets); err != nil {
		return err
	}
	return addV3RequestState(step, states)
}

func validateV3RequestValue(step RequestStep) error {
	if step.Initial != nil && step.Constant != nil {
		return invalid("pagination request step cannot set initial and constant values")
	}
	if step.Constant == nil && step.State == "" {
		return invalid("pagination dynamic request step requires state")
	}
	for _, value := range []*Scalar{step.Initial, step.Constant} {
		if value != nil && (validateScalar(value) != nil || value.Type != step.ValueType) {
			return invalid("pagination request scalar type is invalid")
		}
	}
	return nil
}

func addV3RequestState(step RequestStep, states map[string]ValueType) error {
	if step.State == "" {
		return nil
	}
	if !stateNamePattern.MatchString(step.State) {
		return invalid("pagination request state is invalid")
	}
	if _, duplicate := states[step.State]; duplicate {
		return invalid("pagination request state must be unique")
	}
	states[step.State] = step.ValueType
	return nil
}

// addV3RequestTarget rejects overlapping writes because two steps targeting the
// same page phase would make pagination depend on incidental declaration order.
func addV3RequestTarget(step RequestStep, targets map[string]map[PageApplication]struct{}) error {
	key := string(step.Target.Location) + "\x00" + step.Target.Name
	applications := targets[key]
	if _, duplicate := applications[step.Apply]; duplicate {
		return invalid("pagination request target and application must be unique")
	}
	if step.Apply == ApplyAll && len(applications) > 0 || hasV3Application(applications, ApplyAll) {
		return invalid("pagination apply all cannot overlap page-specific targets")
	}
	if applications == nil {
		applications = make(map[PageApplication]struct{})
		targets[key] = applications
	}
	applications[step.Apply] = struct{}{}
	return nil
}

func hasV3Application(values map[PageApplication]struct{}, wanted PageApplication) bool {
	_, ok := values[wanted]
	return ok
}

func validNewState(state string, existing map[string]ValueType) bool {
	if !stateNamePattern.MatchString(state) {
		return false
	}
	_, duplicate := existing[state]
	return !duplicate
}

func validPageApplication(value PageApplication) bool {
	return value == ApplyAll || value == ApplyFirst || value == ApplySubsequent
}

func validV3Target(target RequestTarget) bool {
	if target.Location == RequestGraphQLVariable {
		return stateNamePattern.MatchString(target.Name)
	}
	return validateRequestTarget(target) == nil
}

func validateV3Response(plan ResponsePlan, states map[string]ValueType) (map[string]ValueType, error) {
	if err := validateItemsSource(plan.Items, states); err != nil {
		return nil, err
	}
	values := make(map[string]ValueType, len(plan.Values))
	if len(plan.Values) > maxResponseValues {
		return nil, invalid("pagination response plan is too large")
	}
	for _, value := range plan.Values {
		if !validNewState(value.Name, values) {
			return nil, invalid("pagination response value name is invalid")
		}
		if err := validateV3Source(value.Source, states); err != nil {
			return nil, err
		}
		values[value.Name] = value.Source.ValueType
	}
	return values, nil
}

// validateItemsSource admits omission only for nested paths because a decoded
// root always exists and cannot represent an optional provider collection.
func validateItemsSource(source ItemsSource, states map[string]ValueType) error {
	if (source.Path == "") == (len(source.Paths) == 0) {
		return invalid("pagination items requires one path form")
	}
	// A root JSON document cannot be absent after successful decoding, so the omission opt-in would be meaningless.
	if source.MissingIsEmpty && itemsSourceUsesRoot(source) {
		return invalid("pagination missing_is_empty requires a nested collection path")
	}
	if source.Path != "" {
		return ValidateItemsPath(source.Path)
	}
	return validateConditionalPaths(source.Paths, states, true)
}

// itemsSourceUsesRoot keeps the omission invariant aligned across direct and
// conditionally selected collection paths.
func itemsSourceUsesRoot(source ItemsSource) bool {
	// The direct path is mutually exclusive with conditional paths after shape validation.
	if source.Path != "" {
		return source.Path == "$"
	}
	for _, candidate := range source.Paths {
		// Any root alternative makes a single source-wide omission rule internally inconsistent.
		if candidate.Path == "$" {
			return true
		}
	}
	return false
}

func validateV3Source(source ValueSource, states map[string]ValueType) error {
	if !validV3ValueType(source.ValueType) {
		return invalid("pagination response value type is invalid")
	}
	switch source.Location {
	case SourceBody, SourceGraphQL:
		return validateSourcePaths(source, states)
	case SourceHeader:
		return validateHeaderSource(source)
	case SourceLink:
		return validateLinkSource(source)
	case SourceItems:
		return validateItemSource(source)
	default:
		return invalid("pagination response source location is invalid")
	}
}

func validateSourcePaths(source ValueSource, states map[string]ValueType) error {
	if source.Name != "" || source.Relation != "" || source.Item != nil || (source.Path == "") == (len(source.Paths) == 0) {
		return invalid("pagination response path source is invalid")
	}
	if source.Path != "" {
		return ValidateBodyPath(source.Path)
	}
	return validateConditionalPaths(source.Paths, states, false)
}

func validateConditionalPaths(paths []ConditionalPath, states map[string]ValueType, items bool) error {
	if len(paths) == 0 || len(paths) > maxConditionalPaths {
		return invalid("pagination conditional paths are invalid")
	}
	for _, path := range paths {
		if err := validateRequestCondition(path.When, states); err != nil {
			return err
		}
		if items {
			if err := ValidateItemsPath(path.Path); err != nil {
				return err
			}
		} else if err := ValidateBodyPath(path.Path); err != nil {
			return err
		}
	}
	return nil
}

func validateItemSource(source ValueSource) error {
	if source.Path != "" || len(source.Paths) > 0 || source.Name != "" || source.Relation != "" || source.Item == nil || source.Item.Position != ItemLast {
		return invalid("pagination item source is invalid")
	}
	if source.Item.Path == "" {
		return nil
	}
	return ValidateItemsPath(source.Item.Path)
}

func validateV3Continuation(steps []ContinuationStep, states, values map[string]ValueType, sources map[string]ValueSource) error {
	if len(steps) == 0 || len(steps) > maxContinuationSteps {
		return invalid("pagination continuation plan is invalid")
	}
	seen := make(map[string]struct{}, len(steps))
	for _, step := range steps {
		if !stateNamePattern.MatchString(step.State) {
			return invalid("pagination continuation state is invalid")
		}
		if _, duplicate := seen[step.State]; duplicate {
			return invalid("pagination continuation state must be unique")
		}
		if err := validateContinuationStep(step, states, values, sources); err != nil {
			return err
		}
		seen[step.State] = struct{}{}
	}
	return nil
}

func validateContinuationStep(step ContinuationStep, states, values map[string]ValueType, sources map[string]ValueSource) error {
	switch step.Kind {
	case ContinuationToken:
		return validateTokenContinuation(step, states, values)
	case ContinuationOffset, ContinuationPage:
		return validateNumericContinuation(step, states, values)
	case ContinuationRFCLink, ContinuationNextURL:
		return validateV3URLContinuation(step, values, sources)
	default:
		return invalid("pagination continuation kind is invalid")
	}
}

func validateV3URLContinuation(step ContinuationStep, values map[string]ValueType, sources map[string]ValueSource) error {
	if values[step.ResponseValue] != ValueURL || step.Increment != nil || step.Origin == nil {
		return invalid("pagination URL continuation is invalid")
	}
	if step.Kind == ContinuationRFCLink && sources[step.ResponseValue].Location != SourceLink {
		return invalid("pagination RFC Link continuation source is invalid")
	}
	return validateOriginPolicy(*step.Origin)
}

func v3ResponseSources(plan ResponsePlan) map[string]ValueSource {
	result := make(map[string]ValueSource, len(plan.Values))
	for _, value := range plan.Values {
		result[value.Name] = value.Source
	}
	return result
}

func validateTokenContinuation(step ContinuationStep, states, values map[string]ValueType) error {
	wanted := states[step.State]
	actual, ok := values[step.ResponseValue]
	if !ok || actual != wanted || step.Increment != nil {
		return invalid("pagination continuation response value is invalid")
	}
	if wanted != ValueString && wanted != ValueInteger {
		return invalid("pagination token continuation type is invalid")
	}
	if step.Origin != nil {
		return invalid("pagination scalar continuation cannot set origin policy")
	}
	return nil
}

func validateNumericContinuation(step ContinuationStep, states, values map[string]ValueType) error {
	if states[step.State] != ValueInteger || step.Origin != nil || (step.ResponseValue == "") == (step.Increment == nil) {
		return invalid("pagination numeric continuation is invalid")
	}
	if step.ResponseValue != "" {
		if values[step.ResponseValue] != ValueInteger {
			return invalid("pagination numeric response value is invalid")
		}
		return nil
	}
	if step.Increment.Mode != IncrementFixed && step.Increment.Mode != IncrementItemsReturned {
		return invalid("pagination increment mode is invalid")
	}
	if step.Increment.Mode == IncrementFixed && step.Increment.Value < 1 {
		return invalid("pagination fixed increment is invalid")
	}
	return nil
}

func validateOriginPolicy(policy OriginPolicy) error {
	if policy.Mode == OriginSame {
		if len(policy.AllowedOrigins) == 0 {
			return nil
		}
		return invalid("same-origin pagination cannot set an allowlist")
	}
	if policy.Mode != OriginList || len(policy.AllowedOrigins) == 0 || len(policy.AllowedOrigins) > maxAllowedOrigins {
		return invalid("pagination origin policy is invalid")
	}
	for _, origin := range policy.AllowedOrigins {
		if !validOrigin(origin) {
			return invalid("pagination allowed origin is invalid")
		}
	}
	return nil
}

func validOrigin(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.IsAbs() && parsed.Host != "" && parsed.User == nil && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == "" && (parsed.Scheme == "https" || parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname()))
}

func isLoopbackHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}

func validateV3Termination(value Termination, states, values map[string]ValueType) error {
	if err := validateV3TerminationShape(value, states); err != nil {
		return err
	}
	if err := validateV3MissingValues(value.StopOnMissingValues, values); err != nil {
		return err
	}
	return validateV3ResponseConditions(value.Conditions, states, values)
}

func validateV3TerminationShape(value Termination, states map[string]ValueType) error {
	if !validRepeatedValue(value.RepeatedValue) {
		return invalid("pagination repeated-value behavior is invalid")
	}
	if !hasV3TerminationSignal(value) {
		return invalid("pagination requires an explicit termination signal")
	}
	if !v3TerminationWithinBounds(value) {
		return invalid("pagination termination plan is too large")
	}
	if value.StopOnShortPage != nil && states[value.StopOnShortPage.RequestState] != ValueInteger {
		return invalid("pagination short-page request state is invalid")
	}
	return nil
}

func validRepeatedValue(value RepeatedValueBehavior) bool {
	return value == RepeatedStop || value == RepeatedError
}

func hasV3TerminationSignal(value Termination) bool {
	return value.StopOnEmptyItems || value.StopOnShortPage != nil || len(value.StopOnMissingValues) > 0 || len(value.Conditions) > 0
}

func v3TerminationWithinBounds(value Termination) bool {
	return len(value.StopOnMissingValues) <= maxTerminationValues && len(value.Conditions) <= maxTerminationValues
}

func validateV3MissingValues(names []string, values map[string]ValueType) error {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, duplicate := seen[name]; duplicate {
			return invalid("pagination missing-value termination is duplicated")
		}
		if _, ok := values[name]; !ok {
			return invalid("pagination missing-value termination is invalid")
		}
		seen[name] = struct{}{}
	}
	return nil
}

func validateV3ResponseConditions(conditions []ResponseCondition, states, values map[string]ValueType) error {
	for _, condition := range conditions {
		valueType, ok := values[condition.ResponseValue]
		if !ok {
			return invalid("pagination response condition is invalid")
		}
		if condition.Operator == ConditionStateGTE {
			if valueType != ValueInteger || states[condition.State] != ValueInteger || condition.Value != nil {
				return invalid("pagination state total condition is invalid")
			}
			continue
		}
		if condition.State != "" || validateTypedCondition(condition.Operator, condition.Value, valueType) != nil {
			return invalid("pagination response condition is invalid")
		}
	}
	return nil
}

func validateRequestCondition(condition RequestCondition, states map[string]ValueType) error {
	valueType, ok := states[condition.State]
	if !ok {
		return invalid("pagination conditional path state is unknown")
	}
	return validateTypedCondition(condition.Operator, condition.Value, valueType)
}

func validateTypedCondition(operator ConditionOperator, value *Scalar, valueType ValueType) error {
	if operator == ConditionPresent || operator == ConditionAbsent {
		if value == nil {
			return nil
		}
		return invalid("pagination presence condition cannot set a value")
	}
	if operator != ConditionEquals && operator != ConditionNotEquals {
		return invalid("pagination condition operator is invalid")
	}
	if value == nil {
		return invalid("pagination comparison condition requires a value")
	}
	if err := validateScalar(value); err != nil || value.Type != valueType {
		return invalid("pagination condition value type is invalid")
	}
	return nil
}

func validateV3GraphQL(plan *GraphQLPlan, states map[string]ValueType, steps []RequestStep, response ResponsePlan) error {
	usesGraphQL := requestUsesGraphQLVariables(steps) || responseUsesGraphQL(response)
	if plan == nil {
		if usesGraphQL {
			return invalid("pagination GraphQL plan is required")
		}
		return nil
	}
	if err := validateGraphQLPlanShape(plan); err != nil {
		return err
	}
	if err := validateGraphQLVariables(plan.Variables, states); err != nil {
		return err
	}
	if err := validateGraphQLRequestBindings(steps, plan.Variables); err != nil {
		return err
	}
	return validateGraphQLAliases(plan.ResultAliases)
}

func validateGraphQLPlanShape(plan *GraphQLPlan) error {
	if !validGraphQLTemplate(plan.FirstPageTemplate) || !validGraphQLTemplate(plan.SubsequentPageTemplate) {
		return invalid("pagination GraphQL templates are invalid")
	}
	if len(plan.Variables) > maxGraphQLBindings || len(plan.ResultAliases) > maxGraphQLBindings {
		return invalid("pagination GraphQL bindings are too large")
	}
	return nil
}

func validGraphQLTemplate(value string) bool {
	return value != "" && len(value) <= maxGraphQLTemplate
}

func validateGraphQLVariables(bindings []GraphQLVariable, states map[string]ValueType) error {
	variables := make(map[string]GraphQLVariable, len(bindings))
	for _, variable := range bindings {
		if !validGraphQLName(variable.Name) || states[variable.State] != variable.ValueType {
			return invalid("pagination GraphQL variable is invalid")
		}
		if _, duplicate := variables[variable.Name]; duplicate {
			return invalid("pagination GraphQL variable is duplicated")
		}
		variables[variable.Name] = variable
	}
	return nil
}

func validateGraphQLRequestBindings(steps []RequestStep, variables []GraphQLVariable) error {
	bindings := make(map[string]GraphQLVariable, len(variables))
	for _, variable := range variables {
		bindings[variable.Name] = variable
	}
	for _, step := range steps {
		if step.Target.Location != RequestGraphQLVariable {
			continue
		}
		binding, ok := bindings[step.Target.Name]
		if !ok || binding.State != step.State || binding.ValueType != step.ValueType {
			return invalid("pagination GraphQL request binding is invalid")
		}
	}
	return nil
}

func validGraphQLName(value string) bool {
	return stateNamePattern.MatchString(value) && !strings.Contains(value, "-")
}

func requestUsesGraphQLVariables(steps []RequestStep) bool {
	for _, step := range steps {
		if step.Target.Location == RequestGraphQLVariable {
			return true
		}
	}
	return false
}

func responseUsesGraphQL(plan ResponsePlan) bool {
	for _, value := range plan.Values {
		if value.Source.Location == SourceGraphQL {
			return true
		}
	}
	return false
}

func validateGraphQLAliases(aliases []GraphQLResultAlias) error {
	seen := make(map[string]struct{}, len(aliases))
	seenAliases := make(map[string]struct{}, len(aliases))
	for _, alias := range aliases {
		if !validGraphQLName(alias.Name) || !validGraphQLName(alias.Alias) {
			return invalid("pagination GraphQL result alias is invalid")
		}
		if _, duplicate := seen[alias.Name]; duplicate {
			return invalid("pagination GraphQL result alias is duplicated")
		}
		if _, duplicate := seenAliases[alias.Alias]; duplicate {
			return invalid("pagination GraphQL result alias value is duplicated")
		}
		seen[alias.Name] = struct{}{}
		seenAliases[alias.Alias] = struct{}{}
	}
	return nil
}

func validV3ValueType(value ValueType) bool {
	return value == ValueString || value == ValueInteger || value == ValueBoolean || value == ValueURL
}
