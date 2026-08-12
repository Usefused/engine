package signaturepolicy

import (
	"strings"

	"github.com/Usefused/engine/internal/shared/secretref"
)

func Normalize(config *Config) {
	if config == nil {
		return
	}
	for index := range config.Rules {
		normalizeRule(&config.Rules[index])
	}
}

func NormalizeAndValidate(config *Config) error {
	Normalize(config)
	return Validate(config)
}

func normalizeRule(rule *Rule) {
	rule.Name = strings.TrimSpace(rule.Name)
	rule.Kind = RuleKind(strings.ToLower(strings.TrimSpace(string(rule.Kind))))
	if rule.Predicates == nil {
		rule.Predicates = []Predicate{}
	}
	for index := range rule.Predicates {
		normalizeSource(&rule.Predicates[index].Source)
		rule.Predicates[index].Operator = PredicateOperator(strings.ToLower(strings.TrimSpace(string(rule.Predicates[index].Operator))))
	}
	normalizeVerification(&rule.Verification)
}

func normalizeVerification(verification *Verification) {
	verification.Kind = VerificationKind(strings.ToLower(strings.TrimSpace(string(verification.Kind))))
	if verification.Signature != nil {
		normalizeSignature(verification.Signature)
	}
	if verification.JWT != nil {
		normalizeSource(&verification.JWT.Token)
		verification.JWT.SecretRef = normalizeSecretRef(verification.JWT.SecretRef)
		for index := range verification.JWT.Algorithms {
			verification.JWT.Algorithms[index] = strings.ToUpper(strings.TrimSpace(verification.JWT.Algorithms[index]))
		}
	}
	if verification.Challenge != nil {
		normalizeSource(&verification.Challenge.Value)
		verification.Challenge.BodyField = strings.TrimSpace(verification.Challenge.BodyField)
	}
}

func normalizeSignature(signature *SignatureVerification) {
	signature.SecretRef = normalizeSecretRef(signature.SecretRef)
	normalizeSource(&signature.Signature)
	signature.Algorithm = Algorithm(strings.ToLower(strings.TrimSpace(string(signature.Algorithm))))
	signature.Encoding = Encoding(strings.ToLower(strings.TrimSpace(string(signature.Encoding))))
	signature.Comparison = Comparison(strings.ToLower(strings.TrimSpace(string(signature.Comparison))))
	for index := range signature.Components {
		component := &signature.Components[index]
		component.Kind = ComponentKind(strings.ToLower(strings.TrimSpace(string(component.Kind))))
		component.Join = ComponentJoin(strings.ToLower(strings.TrimSpace(string(component.Join))))
		component.Algorithm = Algorithm(strings.ToLower(strings.TrimSpace(string(component.Algorithm))))
		component.Encoding = Encoding(strings.ToLower(strings.TrimSpace(string(component.Encoding))))
		if component.Names == nil {
			component.Names = []string{}
		}
		for nameIndex := range component.Names {
			component.Names[nameIndex] = strings.TrimSpace(component.Names[nameIndex])
		}
	}
}

func normalizeSource(source *ValueSource) {
	source.Location = ValueLocation(strings.ToLower(strings.TrimSpace(string(source.Location))))
	source.Name = strings.TrimSpace(source.Name)
	source.Path = strings.TrimSpace(source.Path)
}

func normalizeSecretRef(value string) string {
	parsed, err := secretref.Parse(strings.TrimSpace(value))
	if err != nil {
		return value
	}
	return parsed.String()
}
