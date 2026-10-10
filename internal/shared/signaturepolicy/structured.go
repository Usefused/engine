package signaturepolicy

import (
	"errors"
	"strings"
)

// validateSourceComponent forbids unrelated serialization knobs on a singular selected value.
func validateSourceComponent(c InputComponent) error {
	// A source component contributes exactly one value; joins and digest options would imply different bytes.
	if c.Source == nil || len(c.Names) != 0 || c.Join != "" || c.Algorithm != "" || c.Encoding != "" {
		return errors.New("source component requires only source")
	}
	return validateSource(*c.Source)
}

// SameSource compares extraction semantics while respecting HTTP's case-insensitive header names.
func SameSource(a, b ValueSource) bool {
	// Different locations and body paths can never authenticate the same selected bytes.
	if a.Location != b.Location || a.Path != b.Path {
		return false
	}
	namesEqual := a.Name == b.Name
	// Only HTTP header names are case-insensitive; member keys remain exact.
	if a.Location == LocationHeader {
		namesEqual = strings.EqualFold(a.Name, b.Name)
	}
	// An absent selector means the entire header, not any extracted member.
	if a.Field == nil || b.Field == nil {
		return namesEqual && a.Field == nil && b.Field == nil
	}
	return namesEqual && *a.Field == *b.Field
}

// validateSignedTimestampSource ensures freshness is measured against authenticated bytes.
func validateSignedTimestampSource(components []InputComponent, stamp *SignatureTimestamp) error {
	// Header and source are mutually exclusive to avoid a hidden fallback between timestamp identities.
	if stamp.Header != "" || stamp.Source.Location != LocationHeader {
		return errors.New("timestamp requires either header or a header source")
	}
	// Validate the complete selector before checking its inclusion in the message.
	if err := validateSource(*stamp.Source); err != nil {
		return err
	}
	for _, c := range components {
		// Extracted timestamps require the same singular source in the signed message.
		if c.Kind == ComponentSource && c.Source != nil && SameSource(*c.Source, *stamp.Source) {
			return nil
		}
	}
	return errors.New("timestamp source must be signed")
}

// validateStructuredVersion prevents older runtimes from ignoring security-bearing source fields.
func validateStructuredVersion(config *Config) error {
	// Version three explicitly declares support for the full structured-source contract.
	if config.Version == VersionStructuredHeaders {
		return nil
	}
	for _, rule := range config.Rules {
		sources := []ValueSource{}
		for _, p := range rule.Predicates {
			sources = append(sources, p.Source)
		}
		// All optional branches are scanned, including ones later rejected by branch validation.
		if rule.Response != nil {
			sources = append(sources, rule.Response.Value)
		}
		// Standalone challenge values are another scalar extraction boundary.
		if rule.Verification.Challenge != nil {
			sources = append(sources, rule.Verification.Challenge.Value)
		}
		// JWT token extraction also needs version-three negotiation.
		if rule.Verification.JWT != nil {
			sources = append(sources, rule.Verification.JWT.Token)
		}
		// HMAC recipes carry both digest and signed-message selectors.
		if s := rule.Verification.Signature; s != nil {
			sources = append(sources, s.Signature)
			// Timestamp selectors and source components alter authenticated bytes independently of extraction.
			if s.Timestamp != nil && s.Timestamp.Source != nil {
				return errors.New("timestamp source requires signature policy v3")
			}
			for _, c := range s.Components {
				// Even a misplaced source must not be dropped by a v1/v2 decoder.
				if c.Source != nil || c.Kind == ComponentSource {
					return errors.New("source component requires signature policy v3")
				}
			}
		}
		for _, source := range sources {
			// Field extraction must not degrade into comparing an entire header on older runtimes.
			if source.Field != nil {
				return errors.New("header field extraction requires signature policy v3")
			}
		}
	}
	return nil
}
