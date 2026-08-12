// Package signaturepolicy owns the credential-free inbound verification
// contract shared by Registry and Engine.
package signaturepolicy

type RuleKind string
type PredicateOperator string
type ValueLocation string
type VerificationKind string
type ComponentKind string
type Algorithm string
type Encoding string
type Comparison string
type ComponentJoin string

const (
	Version = 1

	RuleChallenge RuleKind = "challenge"
	RuleEvent     RuleKind = "event"

	PredicatePresent PredicateOperator = "present"
	PredicateAbsent  PredicateOperator = "absent"
	PredicateEquals  PredicateOperator = "equals"

	LocationHeader ValueLocation = "header"
	LocationQuery  ValueLocation = "query"
	LocationBody   ValueLocation = "body"

	VerificationSignature VerificationKind = "signature"
	VerificationJWT       VerificationKind = "jwt"
	VerificationChallenge VerificationKind = "challenge_response"

	ComponentRawBody          ComponentKind = "raw_body"
	ComponentExactCallbackURL ComponentKind = "exact_callback_url"
	ComponentSelectedHeaders  ComponentKind = "selected_headers"
	ComponentSelectedQuery    ComponentKind = "selected_query"
	ComponentSortedForm       ComponentKind = "sorted_form"
	ComponentBodyHash         ComponentKind = "body_hash"

	AlgorithmHMACSHA1   Algorithm = "hmac_sha1"
	AlgorithmHMACSHA256 Algorithm = "hmac_sha256"
	AlgorithmHMACSHA512 Algorithm = "hmac_sha512"
	AlgorithmSHA256     Algorithm = "sha256"
	AlgorithmSHA512     Algorithm = "sha512"
	EncodingHex         Encoding  = "hex"
	EncodingBase64      Encoding  = "base64"
	EncodingBase64URL   Encoding  = "base64url"

	ComparisonConstantTime Comparison = "constant_time"

	JoinConcatValues    ComponentJoin = "concat_values"
	JoinConcatNameValue ComponentJoin = "concat_name_value"
	JoinFormURLEncoded  ComponentJoin = "form_urlencoded"
)

type Config struct {
	Version int    `json:"version" yaml:"version"`
	Rules   []Rule `json:"rules" yaml:"rules"`
}

// Rules are first-match so a challenge request cannot accidentally fall
// through into an event verification recipe.
type Rule struct {
	Name         string       `json:"name" yaml:"name"`
	Kind         RuleKind     `json:"kind" yaml:"kind"`
	Predicates   []Predicate  `json:"predicates" yaml:"predicates"`
	Verification Verification `json:"verification" yaml:"verification"`
}

type Predicate struct {
	Source   ValueSource       `json:"source" yaml:"source"`
	Operator PredicateOperator `json:"operator" yaml:"operator"`
	Value    string            `json:"value,omitempty" yaml:"value,omitempty"`
}

type ValueSource struct {
	Location ValueLocation `json:"location" yaml:"location"`
	Name     string        `json:"name,omitempty" yaml:"name,omitempty"`
	Path     string        `json:"path,omitempty" yaml:"path,omitempty"`
}

type Verification struct {
	Kind      VerificationKind       `json:"kind" yaml:"kind"`
	Signature *SignatureVerification `json:"signature,omitempty" yaml:"signature,omitempty"`
	JWT       *JWTVerification       `json:"jwt,omitempty" yaml:"jwt,omitempty"`
	Challenge *ChallengeResponse     `json:"challenge,omitempty" yaml:"challenge,omitempty"`
}

type SignatureVerification struct {
	SecretRef  string           `json:"secret_ref" yaml:"secret_ref"`
	Signature  ValueSource      `json:"signature" yaml:"signature"`
	Components []InputComponent `json:"components" yaml:"components"`
	Algorithm  Algorithm        `json:"algorithm" yaml:"algorithm"`
	Encoding   Encoding         `json:"encoding" yaml:"encoding"`
	Comparison Comparison       `json:"comparison" yaml:"comparison"`
	Prefix     string           `json:"prefix,omitempty" yaml:"prefix,omitempty"`
	// ComponentSeparator is inserted only between component byte strings. It
	// is explicit because provider recipes differ between raw concatenation and
	// delimiter-separated inputs; Engine must never guess from a provider name.
	ComponentSeparator string `json:"component_separator" yaml:"component_separator"`
}

// Component order is part of the signed message. raw_body contributes the
// untouched request bytes. exact_callback_url comes from the immutable local
// webhook registration (trusted public base plus registered route), never Host
// or forwarding headers. Selected header/query values use Names order and
// preserve repeated parsed values in wire order. Header values are the HTTP
// parser's field values; query values are percent-decoded UTF-8 strings.
// sorted_form sorts repeated pairs by UTF-8 field name and then value. Join
// defines exact serialization: values only, name immediately followed by
// value, or RFC3986 percent-encoded name=value pairs joined by '&'. body_hash
// contributes the encoded digest of the exact raw body.
type InputComponent struct {
	Kind      ComponentKind `json:"kind" yaml:"kind"`
	Names     []string      `json:"names" yaml:"names"`
	Join      ComponentJoin `json:"join,omitempty" yaml:"join,omitempty"`
	Algorithm Algorithm     `json:"algorithm,omitempty" yaml:"algorithm,omitempty"`
	Encoding  Encoding      `json:"encoding,omitempty" yaml:"encoding,omitempty"`
}

type JWTVerification struct {
	SecretRef   string      `json:"secret_ref" yaml:"secret_ref"`
	Token       ValueSource `json:"token" yaml:"token"`
	Algorithms  []string    `json:"algorithms" yaml:"algorithms"`
	Issuer      string      `json:"issuer,omitempty" yaml:"issuer,omitempty"`
	Audience    string      `json:"audience,omitempty" yaml:"audience,omitempty"`
	ClockSkewMs int64       `json:"clock_skew_ms" yaml:"clock_skew_ms"`
}

type ChallengeResponse struct {
	Value      ValueSource `json:"value" yaml:"value"`
	BodyField  string      `json:"body_field" yaml:"body_field"`
	StatusCode int         `json:"status_code" yaml:"status_code"`
}
