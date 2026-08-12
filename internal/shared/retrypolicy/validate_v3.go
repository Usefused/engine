package retrypolicy

import (
	"errors"
	"regexp"
	"strings"
)

const (
	maxV3Methods           = 16
	maxV3OperationKinds    = 8
	maxV3Statuses          = 32
	maxV3Errors            = 16
	maxV3RequiredHeaders   = 16
	maxV3RetryAfterHeaders = 8
	maxV3ElapsedMs         = int64(86_400_000)
)

var retryMethodPattern = regexp.MustCompile(`^[A-Z][A-Z0-9!#$%&'*+.^_` + "`" + `|~-]{0,31}$`)

// validateV3Exact preserves rule order and validates each independently because
// runtime retry selection is first-match rather than a merged policy.
func validateV3Exact(config *Config) error {
	if config.Version != Version || len(config.Rules) == 0 || len(config.Rules) > maxRules {
		return errors.New("retry v3 config is invalid")
	}
	for _, rule := range config.Rules {
		if err := validateV3Rule(rule); err != nil {
			return err
		}
	}
	return nil
}

func validateV3Rule(rule Rule) error {
	if err := validateV3Predicates(rule.Predicates); err != nil {
		return err
	}
	return validateV3Action(rule.Action)
}

// validateV3Predicates requires explicit replay and idempotency proof for unsafe
// retries so a transport failure cannot duplicate a provider mutation.
func validateV3Predicates(value Predicates) error {
	if !validV3PredicateCounts(value) {
		return errors.New("retry predicate list exceeds its bound")
	}
	if (len(value.Statuses) == 0) == (len(value.Errors) == 0) {
		return errors.New("exactly one retry failure family is required")
	}
	if !validV3PredicateLists(value) {
		return errors.New("retry predicates are invalid")
	}
	if !validV3ReplaySelectors(value) {
		return errors.New("retry replay predicates are invalid")
	}
	if value.BodyReplayability == BodyNotReplayable || (unsafeRetryRule(value) && (value.BodyReplayability != BodyReplayable || value.IdempotencyKey.Requirement != IdempotencyKeyRequired)) {
		return errors.New("unsafe retry requires replayable body and idempotency key")
	}
	return nil
}

func validV3PredicateCounts(value Predicates) bool {
	return len(value.Methods) <= maxV3Methods && len(value.OperationKinds) <= maxV3OperationKinds && len(value.Statuses) <= maxV3Statuses && len(value.Errors) <= maxV3Errors && len(value.RequiredProviderHeaders) <= maxV3RequiredHeaders
}

func validV3PredicateLists(value Predicates) bool {
	return uniqueValidMethods(value.Methods) && uniqueValidKinds(value.OperationKinds) && validNonoverlappingStatuses(value.Statuses) && uniqueValidErrors(value.Errors) && uniqueValidHeaderList(value.RequiredProviderHeaders)
}

func validV3ReplaySelectors(value Predicates) bool {
	return validBodyReplayability(value.BodyReplayability) && validV3Idempotency(value.IdempotencyKey)
}

func uniqueValidMethods(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !retryMethodPattern.MatchString(value) {
			return false
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func uniqueValidKinds(values []OperationKind) bool {
	seen := make(map[OperationKind]struct{}, len(values))
	for _, value := range values {
		if !validV3OperationKind(value) {
			return false
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func validV3OperationKind(value OperationKind) bool {
	return value == OperationRead || value == OperationWrite || value == OperationDelete || value == OperationStream || value == OperationQuery || value == OperationMutation
}

func validNonoverlappingStatuses(values []StatusRange) bool {
	for index, value := range values {
		if value.Min < 100 || value.Max > 599 || value.Min > value.Max {
			return false
		}
		for prior := 0; prior < index; prior++ {
			if value.Min <= values[prior].Max && values[prior].Min <= value.Max {
				return false
			}
		}
	}
	return true
}

func uniqueValidErrors(values []ErrorKind) bool {
	seen := make(map[ErrorKind]struct{}, len(values))
	for _, value := range values {
		if !validV3Error(value) {
			return false
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func validV3Error(value ErrorKind) bool {
	return value == ErrorConnectTimeout || value == ErrorReadTimeout || value == ErrorConnectionReset || value == ErrorTemporaryDNS || value == ErrorTLSHandshake || value == ErrorTransport
}

func validV3Idempotency(value IdempotencyKeyPredicate) bool {
	if value.Requirement == IdempotencyKeyRequired {
		return validRetryHeader(value.Header)
	}
	return (value.Requirement == IdempotencyKeyAny || value.Requirement == IdempotencyKeyAbsent) && value.Header == ""
}

func unsafeRetryRule(value Predicates) bool {
	if len(value.Methods) == 0 && len(value.OperationKinds) == 0 {
		return true
	}
	for _, method := range value.Methods {
		if method == "POST" || method == "PATCH" {
			return true
		}
	}
	for _, kind := range value.OperationKinds {
		if kind == OperationWrite || kind == OperationMutation {
			return true
		}
	}
	return false
}

func uniqueValidHeaderList(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		key := strings.ToLower(value)
		if !validRetryHeader(value) {
			return false
		}
		if _, duplicate := seen[key]; duplicate {
			return false
		}
		seen[key] = struct{}{}
	}
	return true
}

func validateV3Action(value Action) error {
	if !validV3ActionBounds(value) {
		return errors.New("retry action bounds are invalid")
	}
	if !validV3BackoffStrategy(value.Backoff.Strategy) {
		return errors.New("retry backoff strategy is invalid")
	}
	if !validV3BackoffBounds(value.Backoff) {
		return errors.New("retry backoff bounds are invalid")
	}
	return validateV3RetryHeaders(value.RetryAfterHeaders, value.MaxElapsedMs)
}

func validV3ActionBounds(value Action) bool {
	return value.MaxAttempts >= 2 && value.MaxAttempts <= maxAttempts && value.MaxElapsedMs >= 1 && value.MaxElapsedMs <= maxV3ElapsedMs
}

func validV3BackoffStrategy(value BackoffStrategy) bool {
	return value == BackoffFixed || value == BackoffExponential
}

func validV3BackoffBounds(value Backoff) bool {
	return value.BaseDelayMs >= 0 && value.MaxDelayMs >= value.BaseDelayMs && value.MaxDelayMs <= maxDelayMs && value.JitterMs >= 0 && value.JitterMs <= value.MaxDelayMs
}

func validateV3RetryHeaders(values []RetryAfterHeader, maxElapsed int64) error {
	if len(values) > maxV3RetryAfterHeaders {
		return errors.New("retry header plan is too large")
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		key := strings.ToLower(value.Name)
		if !validRetryAfterHeader(value) || value.MaxDelayMs > maxElapsed {
			return errors.New("retry header plan is invalid")
		}
		if _, duplicate := seen[key]; duplicate {
			return errors.New("retry header name is duplicated")
		}
		seen[key] = struct{}{}
		formats := make(map[RetryAfterFormat]struct{}, len(value.Formats))
		for _, format := range value.Formats {
			if _, duplicate := formats[format]; duplicate {
				return errors.New("retry header format is duplicated")
			}
			formats[format] = struct{}{}
		}
	}
	return nil
}
