package ratelimitpolicy

import (
	"errors"
	"regexp"
	"strings"
)

const (
	maxIdentityInputs   = 8
	maxCostRules        = 10_000
	maxResponsePaths    = 512
	maxCooldownStatuses = 32
	maxCooldownHeaders  = 16
)

var (
	quotaBindingPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)
	quotaBodyPathPattern = regexp.MustCompile(`^\$(?:\.[A-Za-z0-9_-]+)+$`)
)

// validateV3Config treats every policy as a simultaneous quota dimension;
// duplicate names would collapse distinct runtime state into one bucket.
func validateV3Config(config Config) error {
	if config.Version != Version || len(config.Policies) == 0 || len(config.Policies) > MaxPolicies {
		return errors.New("rate limit v3 config is invalid")
	}
	names := make(map[string]struct{}, len(config.Policies))
	for _, policy := range config.Policies {
		if _, duplicate := names[policy.Name]; duplicate {
			return errors.New("rate limit policy name is duplicated")
		}
		if err := validateV3Policy(policy); err != nil {
			return err
		}
		names[policy.Name] = struct{}{}
	}
	return validateCooldown(config.Cooldown)
}

func validateV3Policy(policy Policy) error {
	if !validV3PolicyName(policy.Name) {
		return errors.New("rate limit policy name is invalid")
	}
	if !validV3Mode(policy.Mode) {
		return errors.New("rate limit policy mode is invalid")
	}
	if !validV3Unit(policy.Unit) {
		return errors.New("rate limit v3 policy shape is invalid")
	}
	if err := validateIdentity(policy.Identity); err != nil {
		return err
	}
	if err := validateCostPlan(policy.Cost); err != nil {
		return err
	}
	if err := validateV3Algorithm(policy); err != nil {
		return err
	}
	return validateResponseSignals(policy.ResponseSignals)
}

func validV3PolicyName(name string) bool {
	return name != "" && len(name) <= 64 && policyNamePattern.MatchString(name)
}
func validV3Mode(mode Mode) bool { return mode == ModeEnforce || mode == ModeObserve }

func validV3Unit(unit Unit) bool {
	return unit == UnitRequests || unit == UnitPoints || unit == UnitComplexity || unit == UnitQuotaUnits
}

// validateIdentity permits only bounded, structured inputs so provider names or
// resolved credential values cannot become an implicit bucket identity.
func validateIdentity(identity BucketIdentity) error {
	if len(identity.Inputs) == 0 || len(identity.Inputs) > maxIdentityInputs {
		return errors.New("rate limit identity inputs are invalid")
	}
	seen := make(map[IdentityKind]struct{}, len(identity.Inputs))
	for _, input := range identity.Inputs {
		if _, duplicate := seen[input.Kind]; duplicate || !validIdentityInput(input) {
			return errors.New("rate limit identity input is invalid")
		}
		seen[input.Kind] = struct{}{}
	}
	return nil
}

func validIdentityInput(input IdentityInput) bool {
	switch input.Kind {
	case IdentityAccount, IdentityServiceVersion, IdentityConnection:
		return input.Binding == "" && input.Name == ""
	case IdentityProject, IdentityTenant, IdentityResource, IdentityIPClass:
		return quotaBindingPattern.MatchString(input.Binding) && input.Name == ""
	case IdentityNamedSharedCredentialFamily:
		return input.Binding == "" && quotaBindingPattern.MatchString(input.Name)
	default:
		return false
	}
}

func validateCostPlan(plan CostPlan) error {
	if plan.Default < 0 || plan.Default > maxPolicyValue || len(plan.Rules) > maxCostRules {
		return errors.New("rate limit cost plan is invalid")
	}
	positive := plan.Default > 0
	seen := make(map[string]struct{}, len(plan.Rules))
	for _, rule := range plan.Rules {
		if !validCostRule(rule) {
			return errors.New("rate limit cost rule is invalid")
		}
		if _, duplicate := seen[rule.Operation]; duplicate {
			return errors.New("rate limit cost rule operation is duplicated")
		}
		seen[rule.Operation] = struct{}{}
		positive = positive || rule.Cost > 0
	}
	if !positive {
		return errors.New("rate limit cost plan requires a positive cost")
	}
	return nil
}

func validCostRule(rule CostRule) bool {
	return validOperationKey(rule.Operation) && rule.Cost >= 0 && rule.Cost <= maxPolicyValue
}

func validateV3Algorithm(policy Policy) error {
	if v3AlgorithmBranches(policy) != 1 {
		return errors.New("rate limit policy requires exactly one algorithm branch")
	}
	switch policy.Algorithm {
	case AlgorithmFixedWindow:
		return validateV3Window(policy.FixedWindow)
	case AlgorithmRollingWindow:
		return validateV3RollingWindow(policy.RollingWindow)
	case AlgorithmTokenBucket:
		return validateV3TokenBucket(policy.TokenBucket)
	case AlgorithmConcurrency:
		if policy.Concurrency == nil || policy.Concurrency.Limit < 1 || policy.Concurrency.Limit > 10_000 {
			return errors.New("rate limit concurrency is invalid")
		}
		return nil
	default:
		return errors.New("rate limit algorithm is invalid")
	}
}

func v3AlgorithmBranches(policy Policy) int {
	count := 0
	for _, set := range []bool{policy.FixedWindow != nil, policy.RollingWindow != nil, policy.TokenBucket != nil, policy.Concurrency != nil} {
		if set {
			count++
		}
	}
	return count
}
func validateV3Window(value *FixedWindow) error {
	if value == nil || !validPolicyValue(value.Limit) || !validInterval(value.DurationMs) {
		return errors.New("rate limit fixed window is invalid")
	}
	return nil
}
func validateV3RollingWindow(value *RollingWindow) error {
	if value == nil || !validPolicyValue(value.Limit) || !validInterval(value.DurationMs) {
		return errors.New("rate limit rolling window is invalid")
	}
	return nil
}
func validateV3TokenBucket(value *TokenBucket) error {
	if value == nil || !validPolicyValue(value.Capacity) || !validPolicyValue(value.RefillUnits) || !validInterval(value.RefillIntervalMs) {
		return errors.New("rate limit token bucket is invalid")
	}
	return nil
}

func validateResponseSignals(signals *ResponseSignals) error {
	if signals == nil {
		return nil
	}
	if responseSignalsEmpty(signals) {
		return errors.New("rate limit response signals are empty")
	}
	for _, signal := range []*ResponseSignal{signals.Limit, signals.Remaining, signals.Cost} {
		if signal != nil && !validResponseSignal(*signal) {
			return errors.New("rate limit response signal is invalid")
		}
	}
	if !validOptionalResetSignal(signals.Reset) {
		return errors.New("rate limit reset signal is invalid")
	}
	return nil
}

func responseSignalsEmpty(signals *ResponseSignals) bool {
	return signals.Limit == nil && signals.Remaining == nil && signals.Reset == nil && signals.Cost == nil
}

func validOptionalResetSignal(signal *ResetSignal) bool {
	return signal == nil || (validResponseSignal(signal.Signal) && validResetFormat(signal.Format))
}

func validResponseSignal(signal ResponseSignal) bool {
	if signal.Source == ResponseSignalHeader {
		return validHeaderName(signal.Name) && signal.Path == ""
	}
	return signal.Source == ResponseSignalBody && signal.Name == "" && validQuotaBodyPath(signal.Path)
}
func validQuotaBodyPath(path string) bool {
	return len(path) <= maxResponsePaths && strings.Count(path, ".") <= 32 && quotaBodyPathPattern.MatchString(path)
}
func validResetFormat(value ResetFormat) bool {
	return value == ResetDeltaSeconds || value == ResetDeltaMilliseconds || value == ResetUnixSeconds || value == ResetUnixMilliseconds || value == ResetRFC3339 || value == ResetHTTPDate
}

func validateCooldown(cooldown *Cooldown) error {
	if cooldown == nil {
		return nil
	}
	if !validCooldownCounts(cooldown) {
		return errors.New("rate limit cooldown is invalid")
	}
	if !validCooldownStatuses(cooldown.Statuses) {
		return errors.New("rate limit cooldown status is invalid")
	}
	for _, header := range cooldown.Headers {
		if !validCooldownHeader(header) {
			return errors.New("rate limit cooldown header is invalid")
		}
	}
	return nil
}

func validCooldownCounts(cooldown *Cooldown) bool {
	return len(cooldown.Statuses) > 0 && len(cooldown.Statuses) <= maxCooldownStatuses && len(cooldown.Headers) > 0 && len(cooldown.Headers) <= maxCooldownHeaders
}

func validCooldownStatuses(values []StatusRange) bool {
	for index, status := range values {
		if status.Min < 100 || status.Max > 599 || status.Min > status.Max {
			return false
		}
		for prior := 0; prior < index; prior++ {
			if status.Min <= values[prior].Max && values[prior].Min <= status.Max {
				return false
			}
		}
	}
	return true
}
func validCooldownHeader(header CooldownHeader) bool {
	if !validHeaderName(header.Name) || len(header.Formats) == 0 || len(header.Formats) > 8 || header.MaxDelayMs < 1 || header.MaxDelayMs > 86_400_000 {
		return false
	}
	for _, format := range header.Formats {
		if !validResetFormat(format) {
			return false
		}
	}
	return true
}
