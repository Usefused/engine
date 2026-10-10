package webhookverify

import (
	"strings"

	"github.com/Usefused/engine/internal/shared/signaturepolicy"
)

const maxStructuredHeaderBytes = 4096
const maxStructuredMembers = 32
const maxSignatureCandidates = 8

// headerFieldValues parses an explicitly configured unquoted key/value header without provider inference.
func headerFieldValues(source signaturepolicy.ValueSource, input PolicyInput) ([]string, bool) {
	// Extraction is valid only on a single HTTP field; duplicate field lines fail closed.
	if source.Location != signaturepolicy.LocationHeader || source.Field == nil {
		return nil, false
	}
	raw, ok := singularValue(input.Request.Header.Values(source.Name))
	// Bound parser work independently of request-body limits.
	if !ok || len(raw) > maxStructuredHeaderBytes {
		return nil, false
	}
	members := strings.Split(raw, source.Field.Separator)
	// Extra unknown members cannot be used to allocate unbounded parser state.
	if len(members) > maxStructuredMembers {
		return nil, false
	}
	values := []string{}
	for _, member := range members {
		key, value, found := strings.Cut(strings.Trim(member, " \t"), source.Field.Assignment)
		// Reject malformed members, quoting and escapes instead of guessing a different grammar.
		if !found || !unquotedToken(key) || !unquotedToken(value) {
			return nil, false
		}
		// Unknown keys are permitted for version negotiation, but never become signature candidates.
		if key == source.Field.Key {
			values = append(values, value)
		}
	}
	return values, len(values) > 0
}

// unquotedToken excludes whitespace, controls, quotes and escapes; authenticated values retain exact bytes.
func unquotedToken(value string) bool {
	// Empty members cannot stand for a timestamp or a digest.
	if value == "" {
		return false
	}
	for _, b := range []byte(value) {
		// This parser intentionally does not implement quoted-string or escaping semantics.
		if b < 0x21 || b > 0x7e || b == '"' || b == '\\' {
			return false
		}
	}
	return true
}

// singularExtractedValue makes scalar consumers fail closed on repeated structured keys.
func singularExtractedValue(values []string, valid bool) (string, bool) {
	// A failed parse cannot be repaired by selecting a previously parsed member.
	if !valid {
		return "", false
	}
	return singularValue(values)
}

// signatureCandidates permits repeated values only for an explicitly structured digest selector.
func signatureCandidates(source signaturepolicy.ValueSource, input PolicyInput) ([]string, bool) {
	// Existing policies retain exactly-one-credential behavior.
	if source.Field == nil {
		value, ok := singleSignatureValue(source, input)
		return []string{value}, ok
	}
	values, ok := headerFieldValues(source, input)
	return values, ok && len(values) <= maxSignatureCandidates
}
