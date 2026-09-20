package signaturepolicy

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Usefused/engine/internal/shared/secretref"
)

const (
	MaxRules       = 16
	MaxPredicates  = 16
	MaxComponents  = 16
	MaxNames       = 32
	MaxText        = 512
	MaxClockSkewMs = int64(300_000)
)

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)
	headerPattern     = regexp.MustCompile(`^[!#$%&'*+\-.^_` + "`" + `|~0-9A-Za-z]+$`)
	jsonPathPattern   = regexp.MustCompile(`^\$(?:\.[A-Za-z0-9_-]+)+$`)
)

// Validate admits only bounded, versioned signature semantics.
func Validate(config *Config) error {
	// An omitted policy belongs to legacy auth handling; a present policy must validate completely.
	if config == nil {
		return nil
	}
	// Older policy versions must not silently ignore security-bearing extensions.
	if config.Version != Version && config.Version != VersionAuthenticated {
		return errors.New("signature_policy version must be 1 or 2")
	}
	// Bounded rule count limits work at the public ingress boundary.
	if len(config.Rules) < 1 || len(config.Rules) > MaxRules {
		return fmt.Errorf("signature_policy rules must contain between 1 and %d entries", MaxRules)
	}
	// New verification semantics must never be silently interpreted as a v1 recipe.
	if err := validatePolicyVersion(config); err != nil {
		return err
	}
	return validateRules(config.Rules)
}

func validateRules(rules []Rule) error {
	seen := make(map[string]struct{}, len(rules))
	for index := range rules {
		if err := validateRule(&rules[index]); err != nil {
			return fmt.Errorf("rules[%d]: %w", index, err)
		}
		if _, exists := seen[rules[index].Name]; exists {
			return fmt.Errorf("rules[%d].name is duplicated", index)
		}
		seen[rules[index].Name] = struct{}{}
	}
	return nil
}

// validateRule checks identity, predicates and authenticated response shape.
func validateRule(rule *Rule) error {
	// Stable bounded rule names keep persisted policy identity unambiguous.
	if !identifierPattern.MatchString(rule.Name) {
		return errors.New("name is invalid")
	}
	// Unknown rule kinds cannot acquire event or response behavior implicitly.
	if rule.Kind != RuleChallenge && rule.Kind != RuleEvent {
		return fmt.Errorf("kind %q is unsupported", rule.Kind)
	}
	// Predicate fan-out is bounded independently of the number of rules.
	if len(rule.Predicates) > MaxPredicates {
		return fmt.Errorf("predicates may contain at most %d entries", MaxPredicates)
	}
	for index := range rule.Predicates {
		// A malformed matcher could accidentally select a more permissive verification branch.
		if err := validatePredicate(rule.Predicates[index]); err != nil {
			return fmt.Errorf("predicates[%d]: %w", index, err)
		}
	}
	return validateRuleVerification(rule)
}

func validatePredicate(predicate Predicate) error {
	if err := validateSource(predicate.Source); err != nil {
		return err
	}
	switch predicate.Operator {
	case PredicatePresent, PredicateAbsent:
		if predicate.Value != "" {
			return errors.New("present/absent predicate cannot declare value")
		}
	case PredicateEquals:
		if !validText(predicate.Value) {
			return errors.New("equals predicate requires a bounded value")
		}
	default:
		return fmt.Errorf("operator %q is unsupported", predicate.Operator)
	}
	return nil
}

func validateVerification(ruleKind RuleKind, verification *Verification) error {
	if verificationBranchCount(verification) != 1 {
		return errors.New("exactly one verification branch is required")
	}
	if ruleKind == RuleChallenge && verification.Kind != VerificationChallenge {
		return errors.New("challenge rules require challenge_response verification")
	}
	if ruleKind == RuleEvent && verification.Kind == VerificationChallenge {
		return errors.New("event rules cannot use challenge_response verification")
	}
	switch verification.Kind {
	case VerificationSignature:
		return validateSignature(verification.Signature)
	case VerificationJWT:
		return validateJWT(verification.JWT)
	case VerificationChallenge:
		if ruleKind != RuleChallenge {
			return errors.New("challenge_response requires a challenge rule")
		}
		return validateChallenge(verification.Challenge)
	default:
		return fmt.Errorf("verification kind %q is unsupported", verification.Kind)
	}
}

func verificationBranchCount(verification *Verification) int {
	count := 0
	for _, present := range []bool{verification.Signature != nil, verification.JWT != nil, verification.Challenge != nil} {
		if present {
			count++
		}
	}
	return count
}

// validateSignature binds the secret reference, signed inputs and freshness policy.
func validateSignature(signature *SignatureVerification) error {
	// Selecting signature verification requires a complete recipe, never an empty branch.
	if signature == nil {
		return errors.New("signature branch is required")
	}
	// Only a bucket reference may identify key material in a published policy.
	if err := validateSecretRef(signature.SecretRef); err != nil {
		return err
	}
	// A declared signature source must be understood before input reconstruction.
	if err := validateSource(signature.Signature); err != nil {
		return fmt.Errorf("signature source: %w", err)
	}
	// Unsupported digest or serialization settings cannot fall back to defaults.
	if err := validateSignatureOptions(signature); err != nil {
		return err
	}
	// Freshness checks must authenticate the same timestamp bytes used in the HMAC.
	if err := validateSignatureTimestamp(signature); err != nil {
		return err
	}
	return validateComponents(signature.Components)
}

func validateSignatureOptions(signature *SignatureVerification) error {
	if !validSignatureAlgorithm(signature.Algorithm) || !validEncoding(signature.Encoding) {
		return errors.New("signature algorithm or encoding is unsupported")
	}
	if signature.Comparison != ComparisonConstantTime {
		return errors.New("signature comparison must be constant_time")
	}
	if !validBoundedFragment(signature.Prefix, 64) {
		return errors.New("signature prefix is invalid")
	}
	if !validBoundedFragment(signature.ComponentSeparator, 16) {
		return errors.New("component_separator is invalid")
	}
	return nil
}

func validateComponents(components []InputComponent) error {
	if len(components) < 1 || len(components) > MaxComponents {
		return fmt.Errorf("components must contain between 1 and %d entries", MaxComponents)
	}
	for index := range components {
		if err := validateComponent(components[index]); err != nil {
			return fmt.Errorf("components[%d]: %w", index, err)
		}
	}
	return nil
}

// validateComponent rejects fields unrelated to the selected signed input.
func validateComponent(component InputComponent) error {
	// Literal values belong only to constant components; ignored values would hide malformed recipes.
	if component.Kind != ComponentConstant && component.Value != "" {
		return errors.New("only constant components accept value")
	}
	// Each signed input admits only its own fields so ignored metadata cannot change perceived coverage.
	switch component.Kind {
	case ComponentConstant:
		return validateConstantComponent(component)
	case ComponentSelectedHeaders:
		return validateSelectedComponent(component, true)
	case ComponentSelectedQuery:
		return validateSelectedComponent(component, false)
	case ComponentBodyHash:
		return validateBodyHashComponent(component)
	case ComponentSortedForm:
		return validateSortedFormComponent(component)
	case ComponentRawBody, ComponentExactCallbackURL:
		return validateBareComponent(component)
	default:
		return fmt.Errorf("kind %q is unsupported", component.Kind)
	}
}

func validateBodyHashComponent(component InputComponent) error {
	if hasComponentNamesOrJoin(component) || !validHashAlgorithm(component.Algorithm) || !validEncoding(component.Encoding) {
		return errors.New("body_hash requires only a hash algorithm and encoding")
	}
	return nil
}

func validateSortedFormComponent(component InputComponent) error {
	if len(component.Names) != 0 || component.Algorithm != "" || component.Encoding != "" {
		return errors.New("sorted_form requires only an explicit join strategy")
	}
	return validateJoin(component.Join)
}

func validateBareComponent(component InputComponent) error {
	if hasComponentNamesOrJoin(component) || component.Algorithm != "" || component.Encoding != "" {
		return errors.New("component does not accept names, join, algorithm, or encoding")
	}
	return nil
}

func hasComponentNamesOrJoin(component InputComponent) bool {
	return len(component.Names) != 0 || component.Join != ""
}

func validateSelectedComponent(component InputComponent, headers bool) error {
	if component.Algorithm != "" || component.Encoding != "" {
		return errors.New("selected component cannot declare algorithm or encoding")
	}
	if err := validateJoin(component.Join); err != nil {
		return err
	}
	return validateNames(component.Names, headers)
}

func validateJoin(join ComponentJoin) error {
	if join == JoinConcatValues || join == JoinConcatNameValue || join == JoinFormURLEncoded {
		return nil
	}
	return fmt.Errorf("join %q is unsupported", join)
}

func validateNames(names []string, headers bool) error {
	if len(names) < 1 || len(names) > MaxNames {
		return fmt.Errorf("names must contain between 1 and %d entries", MaxNames)
	}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		key := name
		if headers {
			key = strings.ToLower(name)
			if !headerPattern.MatchString(name) {
				return errors.New("header name is invalid")
			}
		} else if !identifierPattern.MatchString(name) {
			return errors.New("query name is invalid")
		}
		if _, duplicate := seen[key]; duplicate {
			return errors.New("component name is duplicated")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateJWT(jwt *JWTVerification) error {
	if jwt == nil {
		return errors.New("jwt branch is required")
	}
	if err := validateSecretRef(jwt.SecretRef); err != nil {
		return err
	}
	if err := validateSource(jwt.Token); err != nil {
		return fmt.Errorf("jwt token source: %w", err)
	}
	if err := validateJWTAlgorithms(jwt.Algorithms); err != nil {
		return err
	}
	if !optionalText(jwt.Issuer) || !optionalText(jwt.Audience) {
		return errors.New("jwt issuer or audience is invalid")
	}
	if jwt.ClockSkewMs < 0 || jwt.ClockSkewMs > MaxClockSkewMs {
		return errors.New("jwt issuer, audience, or clock_skew_ms is invalid")
	}
	return nil
}

func validateJWTAlgorithms(algorithms []string) error {
	if len(algorithms) < 1 || len(algorithms) > 8 {
		return errors.New("jwt algorithms must contain between 1 and 8 entries")
	}
	for _, algorithm := range algorithms {
		if !validJWTAlgorithm(algorithm) {
			return fmt.Errorf("jwt algorithm %q is unsupported", algorithm)
		}
	}
	return nil
}

func validateChallenge(challenge *ChallengeResponse) error {
	if challenge == nil {
		return errors.New("challenge branch is required")
	}
	if err := validateSource(challenge.Value); err != nil {
		return err
	}
	if !identifierPattern.MatchString(challenge.BodyField) || challenge.StatusCode < 200 || challenge.StatusCode > 299 {
		return errors.New("challenge response body_field or status_code is invalid")
	}
	return nil
}

func validateSource(source ValueSource) error {
	switch source.Location {
	case LocationHeader:
		return validateNamedSource(source, headerPattern, "header")
	case LocationQuery:
		return validateNamedSource(source, identifierPattern, "query")
	case LocationBody:
		return validateBodySource(source)
	default:
		return fmt.Errorf("source location %q is unsupported", source.Location)
	}
}

func validateNamedSource(source ValueSource, pattern *regexp.Regexp, name string) error {
	if !pattern.MatchString(source.Name) || source.Path != "" {
		return fmt.Errorf("%s source requires only a valid name", name)
	}
	return nil
}

func validateBodySource(source ValueSource) error {
	if source.Name != "" || !jsonPathPattern.MatchString(source.Path) || len(source.Path) > MaxText {
		return errors.New("body source requires only a bounded path")
	}
	return nil
}

func validateSecretRef(value string) error {
	parsed, err := secretref.Parse(value)
	if err != nil || parsed.Kind != secretref.KindSecret {
		return errors.New("secret_ref must be a bucket secret reference")
	}
	return nil
}

func validSignatureAlgorithm(value Algorithm) bool {
	return value == AlgorithmHMACSHA1 || value == AlgorithmHMACSHA256 || value == AlgorithmHMACSHA512
}

func validHashAlgorithm(value Algorithm) bool {
	return value == AlgorithmSHA256 || value == AlgorithmSHA512
}
func validEncoding(value Encoding) bool {
	return value == EncodingHex || value == EncodingBase64 || value == EncodingBase64URL
}

func validJWTAlgorithm(value string) bool {
	switch value {
	case "HS256", "HS384", "HS512":
		return true
	default:
		return false
	}
}

func validText(value string) bool {
	return value != "" && len(value) <= MaxText && !strings.ContainsAny(value, "\r\n\x00")
}
func optionalText(value string) bool { return value == "" || validText(value) }

func validBoundedFragment(value string, maximum int) bool {
	return len(value) <= maximum && !strings.ContainsAny(value, "\r\n\x00")
}

// validatePolicyVersion prevents older runtimes from silently dropping security-bearing v2 fields.
func validatePolicyVersion(config *Config) error {
	// Version two explicitly negotiates the extended recipe contract.
	if config.Version == VersionAuthenticated {
		return nil
	}
	for _, rule := range config.Rules {
		// A response after verification changes the challenge trust boundary.
		if rule.Response != nil {
			return errors.New("authenticated responses require signature policy v2")
		}
		// Rules without an HMAC recipe cannot contain new signature fields.
		if rule.Verification.Signature == nil {
			continue
		}
		signature := rule.Verification.Signature
		// A v1 timestamp field must not imply freshness protection on an older Engine.
		if signature.Timestamp != nil {
			return errors.New("signature timestamps require signature policy v2")
		}
		for _, component := range signature.Components {
			// Constants alter the bytes authenticated by the provider.
			if component.Kind == ComponentConstant {
				return errors.New("constant inputs require signature policy v2")
			}
		}
	}
	return nil
}

// validateRuleVerification makes challenge authentication and response emission one explicit rule.
func validateRuleVerification(rule *Rule) error {
	// Existing standalone challenge rules preserve their original v1 semantics.
	if rule.Response == nil {
		return validateVerification(rule.Kind, &rule.Verification)
	}
	// Authenticated response bodies must be covered by the signature over the raw request.
	if rule.Kind != RuleChallenge || rule.Verification.Kind != VerificationSignature || rule.Response.Value.Location != LocationBody {
		return errors.New("authenticated challenge requires a body value and signature verification")
	}
	// Event validation enforces exactly one signature branch without allowing challenge bypass.
	if err := validateVerification(RuleEvent, &rule.Verification); err != nil {
		return err
	}
	// Challenge content must come from bytes protected by this same request signature.
	if !signatureHasRawBody(rule.Verification.Signature) {
		return errors.New("authenticated challenge requires signed raw body")
	}
	return validateChallenge(rule.Response)
}

// signatureHasRawBody establishes that a challenge value is covered by the authenticated message.
func signatureHasRawBody(signature *SignatureVerification) bool {
	for _, component := range signature.Components {
		// Raw bytes cover every body field independently of JSON parsing order.
		if component.Kind == ComponentRawBody {
			return true
		}
	}
	return false
}

// validateConstantComponent keeps provider version literals bounded and free of unrelated input fields.
func validateConstantComponent(component InputComponent) error {
	// Empty or unbounded literals are malformed rather than implicit empty components.
	if component.Value == "" || !validBoundedFragment(component.Value, MaxText) {
		return errors.New("constant requires a bounded value")
	}
	return validateBareComponent(component)
}

// validateSignatureTimestamp limits freshness checks to one authenticated header and a five-minute maximum window.
func validateSignatureTimestamp(signature *SignatureVerification) error {
	stamp := signature.Timestamp
	// Recipes without timestamp requirements retain their established semantics.
	if stamp == nil {
		return nil
	}
	// Positive bounded age and bounded future skew avoid disabled checks and duration overflow.
	if !headerPattern.MatchString(stamp.Header) || stamp.MaxAgeMs < 1 || stamp.MaxAgeMs > MaxClockSkewMs || stamp.MaxFutureMs < 0 || stamp.MaxFutureMs > MaxClockSkewMs {
		return errors.New("signature timestamp window or header is invalid")
	}
	return validateSignedTimestampHeader(signature.Components, stamp.Header)
}

// validateSignedTimestampHeader rejects freshness checks over data absent from the signed message.
func validateSignedTimestampHeader(components []InputComponent, header string) error {
	for _, component := range components {
		// The exact header value must occur in the signed message, not only in an unsigned policy check.
		if component.Kind != ComponentSelectedHeaders {
			continue
		}
		for _, name := range component.Names {
			// HTTP header names are case-insensitive at both verification and validation boundaries.
			if strings.EqualFold(name, header) {
				return nil
			}
		}
	}
	return errors.New("signature timestamp header must be signed")
}
