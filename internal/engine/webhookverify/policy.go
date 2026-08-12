package webhookverify

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/shared/signaturepolicy"
	"github.com/tidwall/gjson"
)

const (
	CodePolicyInvalid      = "policy_invalid"
	CodeRuleUnmatched      = "rule_unmatched"
	CodeSecretUnavailable  = "secret_unavailable"
	CodeChallengeResponded = "challenge_responded"
	CodeJWTInvalid         = "jwt_invalid"
	CodeJWTUnsupported     = "jwt_unsupported"
	maxSignatureBodyBytes  = 2 << 20
)

// PolicyInput keeps immutable request bytes and trusted registration state
// separate. CallbackURL must never be reconstructed from caller-controlled
// Host or forwarding headers.
type PolicyInput struct {
	Request     *http.Request
	RawBody     []byte
	CallbackURL string
	Now         time.Time
	Resolve     func(context.Context, string) (string, error)
}

type PolicyResult struct {
	VerifyResult
	ChallengeBody []byte
	StatusCode    int
}

// VerifyPolicy executes the first matching rule so challenge traffic cannot
// fall through into an event recipe.
func VerifyPolicy(ctx context.Context, policy *signaturepolicy.Config, input PolicyInput) PolicyResult {
	if signaturepolicy.Validate(policy) != nil || input.Request == nil || len(input.RawBody) > maxSignatureBodyBytes {
		return policyFail(CodePolicyInvalid, "webhook signature policy is invalid")
	}
	rule := firstMatchingRule(policy.Rules, input)
	if rule == nil {
		return policyFail(CodeRuleUnmatched, "no webhook verification rule matched")
	}
	switch rule.Verification.Kind {
	case signaturepolicy.VerificationChallenge:
		return challengeResult(rule.Verification.Challenge, input)
	case signaturepolicy.VerificationSignature:
		return verifyRecipe(ctx, rule.Verification.Signature, input)
	case signaturepolicy.VerificationJWT:
		return verifyJWT(ctx, rule.Verification.JWT, input)
	default:
		return policyFail(CodePolicyInvalid, "webhook verification kind is unsupported")
	}
}

func policyFail(code, reason string) PolicyResult {
	return PolicyResult{VerifyResult: fail(code, reason)}
}

func firstMatchingRule(rules []signaturepolicy.Rule, input PolicyInput) *signaturepolicy.Rule {
	for index := range rules {
		if predicatesMatch(rules[index].Predicates, input) {
			return &rules[index]
		}
	}
	return nil
}

func predicatesMatch(predicates []signaturepolicy.Predicate, input PolicyInput) bool {
	for _, predicate := range predicates {
		value, present := sourceValue(predicate.Source, input)
		if !predicateMatches(predicate, value, present) {
			return false
		}
	}
	return true
}

func predicateMatches(predicate signaturepolicy.Predicate, value string, present bool) bool {
	switch predicate.Operator {
	case signaturepolicy.PredicatePresent:
		return present
	case signaturepolicy.PredicateAbsent:
		return !present
	case signaturepolicy.PredicateEquals:
		return present && value == predicate.Value
	default:
		return false
	}
}

func sourceValue(source signaturepolicy.ValueSource, input PolicyInput) (string, bool) {
	switch source.Location {
	case signaturepolicy.LocationHeader:
		values := input.Request.Header.Values(source.Name)
		return firstValue(values)
	case signaturepolicy.LocationQuery:
		return firstValue(input.Request.URL.Query()[source.Name])
	case signaturepolicy.LocationBody:
		result := gjson.GetBytes(input.RawBody, strings.TrimPrefix(source.Path, "$."))
		return result.String(), result.Exists()
	default:
		return "", false
	}
}

func firstValue(values []string) (string, bool) {
	if len(values) == 0 {
		return "", false
	}
	return values[0], true
}

func challengeResult(challenge *signaturepolicy.ChallengeResponse, input PolicyInput) PolicyResult {
	if challenge == nil {
		return policyFail(CodePolicyInvalid, "challenge configuration is invalid")
	}
	value, present := sourceValue(challenge.Value, input)
	if !present {
		return policyFail(CodeCredentialMissing, "challenge value is missing")
	}
	body, err := json.Marshal(map[string]string{challenge.BodyField: value})
	if err != nil {
		return policyFail(CodePolicyInvalid, "challenge response is invalid")
	}
	return PolicyResult{VerifyResult: VerifyResult{OK: true, Code: CodeChallengeResponded}, ChallengeBody: body, StatusCode: challenge.StatusCode}
}

func verifyRecipe(ctx context.Context, recipe *signaturepolicy.SignatureVerification, input PolicyInput) PolicyResult {
	if recipe == nil || input.Resolve == nil {
		return policyFail(CodePolicyInvalid, "signature recipe is invalid")
	}
	secret, err := input.Resolve(ctx, recipe.SecretRef)
	if err != nil || secret == "" {
		return policyFail(CodeSecretUnavailable, "signature secret is unavailable")
	}
	provided, present := sourceValue(recipe.Signature, input)
	if !present {
		return policyFail(CodeCredentialMissing, "signature is missing")
	}
	message, err := signatureMessage(recipe, input)
	if err != nil {
		return policyFail(CodePolicyInvalid, "signature input is invalid")
	}
	expected, err := encodedHMAC(recipe.Algorithm, recipe.Encoding, secret, message)
	if err != nil {
		return policyFail(CodePolicyInvalid, "signature algorithm is unsupported")
	}
	provided = strings.TrimPrefix(provided, recipe.Prefix)
	if subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		return policyFail(CodeCredentialInvalid, "signature is invalid")
	}
	return PolicyResult{VerifyResult: ok()}
}

func signatureMessage(recipe *signaturepolicy.SignatureVerification, input PolicyInput) ([]byte, error) {
	parts := make([]string, len(recipe.Components))
	for index, component := range recipe.Components {
		value, err := componentValue(component, input)
		if err != nil {
			return nil, err
		}
		parts[index] = value
	}
	return []byte(strings.Join(parts, recipe.ComponentSeparator)), nil
}

func componentValue(component signaturepolicy.InputComponent, input PolicyInput) (string, error) {
	switch component.Kind {
	case signaturepolicy.ComponentRawBody:
		return string(input.RawBody), nil
	case signaturepolicy.ComponentExactCallbackURL:
		if input.CallbackURL == "" {
			return "", errors.New("trusted callback URL is missing")
		}
		return input.CallbackURL, nil
	case signaturepolicy.ComponentSelectedHeaders:
		return joinedSelected(component.Names, input.Request.Header.Values, component.Join), nil
	case signaturepolicy.ComponentSelectedQuery:
		values := input.Request.URL.Query()
		return joinedSelected(component.Names, func(name string) []string { return values[name] }, component.Join), nil
	case signaturepolicy.ComponentSortedForm:
		return sortedForm(input.RawBody, component.Join)
	case signaturepolicy.ComponentBodyHash:
		return encodedHash(component.Algorithm, component.Encoding, input.RawBody)
	default:
		return "", errors.New("unsupported signature component")
	}
}

type namedValue struct{ name, value string }

func joinedSelected(names []string, lookup func(string) []string, join signaturepolicy.ComponentJoin) string {
	pairs := make([]namedValue, 0, len(names))
	for _, name := range names {
		for _, value := range lookup(name) {
			pairs = append(pairs, namedValue{name: name, value: value})
		}
	}
	return joinPairs(pairs, join)
}

func sortedForm(body []byte, join signaturepolicy.ComponentJoin) (string, error) {
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return "", err
	}
	pairs := make([]namedValue, 0)
	for name, items := range values {
		for _, value := range items {
			pairs = append(pairs, namedValue{name: name, value: value})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].name < pairs[j].name || pairs[i].name == pairs[j].name && pairs[i].value < pairs[j].value
	})
	return joinPairs(pairs, join), nil
}

func joinPairs(pairs []namedValue, join signaturepolicy.ComponentJoin) string {
	values := make([]string, len(pairs))
	for index, pair := range pairs {
		switch join {
		case signaturepolicy.JoinConcatValues:
			values[index] = pair.value
		case signaturepolicy.JoinConcatNameValue:
			values[index] = pair.name + pair.value
		case signaturepolicy.JoinFormURLEncoded:
			values[index] = rfc3986(pair.name) + "=" + rfc3986(pair.value)
		}
	}
	separator := ""
	if join == signaturepolicy.JoinFormURLEncoded {
		separator = "&"
	}
	return strings.Join(values, separator)
}

func rfc3986(value string) string { return strings.ReplaceAll(url.QueryEscape(value), "+", "%20") }

func encodedHMAC(algorithm signaturepolicy.Algorithm, encoding signaturepolicy.Encoding, secret string, message []byte) (string, error) {
	constructor := hmacHash(algorithm)
	if constructor == nil {
		return "", errors.New("unsupported HMAC")
	}
	mac := hmac.New(constructor, []byte(secret))
	_, _ = mac.Write(message)
	return encodeDigest(mac.Sum(nil), encoding)
}

func hmacHash(algorithm signaturepolicy.Algorithm) func() hash.Hash {
	switch algorithm {
	case signaturepolicy.AlgorithmHMACSHA1:
		return sha1.New
	case signaturepolicy.AlgorithmHMACSHA256:
		return sha256.New
	case signaturepolicy.AlgorithmHMACSHA512:
		return sha512.New
	default:
		return nil
	}
}

func encodedHash(algorithm signaturepolicy.Algorithm, encoding signaturepolicy.Encoding, body []byte) (string, error) {
	var digest []byte
	switch algorithm {
	case signaturepolicy.AlgorithmSHA256:
		value := sha256.Sum256(body)
		digest = value[:]
	case signaturepolicy.AlgorithmSHA512:
		value := sha512.Sum512(body)
		digest = value[:]
	default:
		return "", errors.New("unsupported hash")
	}
	return encodeDigest(digest, encoding)
}

func encodeDigest(digest []byte, encoding signaturepolicy.Encoding) (string, error) {
	switch encoding {
	case signaturepolicy.EncodingHex:
		return hex.EncodeToString(digest), nil
	case signaturepolicy.EncodingBase64:
		return base64.StdEncoding.EncodeToString(digest), nil
	case signaturepolicy.EncodingBase64URL:
		return base64.RawURLEncoding.EncodeToString(digest), nil
	default:
		return "", errors.New("unsupported encoding")
	}
}

func verifyJWT(ctx context.Context, policy *signaturepolicy.JWTVerification, input PolicyInput) PolicyResult {
	if policy == nil || input.Resolve == nil {
		return policyFail(CodePolicyInvalid, "JWT policy is invalid")
	}
	secret, err := input.Resolve(ctx, policy.SecretRef)
	if err != nil || secret == "" {
		return policyFail(CodeSecretUnavailable, "JWT secret is unavailable")
	}
	token, present := sourceValue(policy.Token, input)
	if !present {
		return policyFail(CodeCredentialMissing, "JWT is missing")
	}
	token = bearerToken(token)
	if err := validateHMACJWT(token, secret, policy, effectiveNow(input.Now)); err != nil {
		return policyFail(CodeJWTInvalid, "JWT is invalid")
	}
	return PolicyResult{VerifyResult: ok()}
}

func bearerToken(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 7 && strings.EqualFold(value[:7], "bearer ") {
		return strings.TrimSpace(value[7:])
	}
	return value
}

func effectiveNow(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now()
	}
	return value
}

type jwtHeader struct {
	Algorithm string `json:"alg"`
}
type jwtClaims struct {
	Issuer    string          `json:"iss"`
	Audience  json.RawMessage `json:"aud"`
	Expires   int64           `json:"exp"`
	NotBefore int64           `json:"nbf"`
}

func validateHMACJWT(token, secret string, policy *signaturepolicy.JWTVerification, now time.Time) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("malformed JWT")
	}
	var header jwtHeader
	var claims jwtClaims
	if decodeJWTPart(parts[0], &header) != nil || decodeJWTPart(parts[1], &claims) != nil || !stringAllowed(header.Algorithm, policy.Algorithms) {
		return errors.New("JWT algorithm is invalid")
	}
	constructor := jwtHMACHash(header.Algorithm)
	if constructor == nil {
		return errors.New("JWT algorithm is unsupported")
	}
	mac := hmac.New(constructor, []byte(secret))
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	provided, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(provided, mac.Sum(nil)) {
		return errors.New("JWT signature is invalid")
	}
	return validateClaims(claims, policy, now)
}

func decodeJWTPart(value string, target any) error {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(decoded, target)
}

func stringAllowed(value string, allowed []string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func jwtHMACHash(algorithm string) func() hash.Hash {
	switch algorithm {
	case "HS256":
		return sha256.New
	case "HS384":
		return sha512.New384
	case "HS512":
		return sha512.New
	default:
		return nil
	}
}

func validateClaims(claims jwtClaims, policy *signaturepolicy.JWTVerification, now time.Time) error {
	skew := time.Duration(policy.ClockSkewMs) * time.Millisecond
	if claims.Expires > 0 && now.After(time.Unix(claims.Expires, 0).Add(skew)) {
		return errors.New("JWT expired")
	}
	if claims.NotBefore > 0 && now.Add(skew).Before(time.Unix(claims.NotBefore, 0)) {
		return errors.New("JWT not active")
	}
	if policy.Issuer != "" && claims.Issuer != policy.Issuer {
		return errors.New("JWT issuer mismatch")
	}
	if policy.Audience != "" && !audienceContains(claims.Audience, policy.Audience) {
		return errors.New("JWT audience mismatch")
	}
	return nil
}

func audienceContains(raw json.RawMessage, wanted string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == wanted
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil {
		return false
	}
	return stringAllowed(wanted, many)
}
