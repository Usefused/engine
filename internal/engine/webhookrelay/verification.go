package webhookrelay

import "github.com/Usefused/engine/internal/shared/signaturepolicy"

// ValidateExportVerification requires body-authenticated, fresh provider events before the registration can export resource claims.
func ValidateExportVerification(policy *signaturepolicy.Config) error {
	// Unsigned and legacy challenge-only registrations cannot attest to remote provider resource ownership.
	if policy == nil || policy.Version != signaturepolicy.VersionAuthenticated || len(policy.Rules) == 0 {
		return ErrDenied
	}
	// Every event-accepting branch must authenticate the same body from which routing identity is extracted.
	for _, rule := range policy.Rules {
		// Challenge-only responses do not enter the stream, but their policy must remain authenticated as well.
		if rule.Verification.Kind != signaturepolicy.VerificationSignature {
			return ErrDenied
		}
		// Timestamp and body coverage are checked independently of the operator's choice of event predicates.
		if err := validateExportSignature(rule.Verification.Signature); err != nil {
			return err
		}
	}
	return nil
}

// validateExportSignature restricts delegated proof to supported keyed digests covering the complete raw provider body.
func validateExportSignature(s *signaturepolicy.SignatureVerification) error {
	// A fresh signed timestamp is necessary to bound provider replay before durable identity deduplication.
	if s == nil || s.Timestamp == nil {
		return ErrDenied
	}
	// Unkeyed body hashes cannot establish a provider's identity.
	if s.Algorithm != signaturepolicy.AlgorithmHMACSHA256 && s.Algorithm != signaturepolicy.AlgorithmHMACSHA512 {
		return ErrDenied
	}
	// At least one whole-body component must bind every routing claim to the provider signature.
	for _, component := range s.Components {
		// Both literal body bytes and a keyed digest over its hash bind the same immutable body.
		if component.Kind == signaturepolicy.ComponentRawBody || component.Kind == signaturepolicy.ComponentBodyHash {
			return nil
		}
	}
	return ErrDenied
}
