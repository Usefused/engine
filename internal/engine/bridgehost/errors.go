package bridgehost

import (
	"encoding/json"
	"errors"
)

const ProfileConstraintCode = "bridge_profile_constraint_violation"
const ProfileConstraintMessage = "Operation blocked by Bridge profile constraints."

// IsProfileConstraintViolation requires explicit Bridge evidence that no connector operation started.
func IsProfileConstraintViolation(err error) bool {
	var invocation *InvocationError
	return errors.As(err, &invocation) && invocation != nil && invocation.Outcome == "not_started" && invocation.Code == "profile_constraint_violation"
}

// validateBridgeInput uses explicit Bridge provenance to distinguish policy from connector input errors.
func validateBridgeInput(schema, input []byte) error {
	validation := ValidatePayload(schema, input)
	// Valid input keeps the ordinary dispatch path and needs no provenance inspection.
	if validation == nil {
		return nil
	}
	var root map[string]json.RawMessage
	// Legacy or arbitrary connector schemas are never guessed to represent profile policy.
	if json.Unmarshal(schema, &root) != nil || string(root["x-fused-bridge-profile-constraints"]) != "1" {
		return validation
	}
	for key := range root {
		// Version 1 describes a conjunction only; other root assertions could explain the failure.
		if key != "allOf" && key != "$schema" && key != "x-fused-bridge-profile-constraints" {
			return validation
		}
	}
	var branches []json.RawMessage
	// Bridge's versioned wrapper puts the untouched connector schema first and policy second.
	if json.Unmarshal(root["allOf"], &branches) != nil || len(branches) != 2 {
		return validation
	}
	// Only connector-valid input that fails the additional policy is a profile denial.
	if ValidatePayload(branches[0], input) == nil && ValidatePayload(branches[1], input) != nil {
		return &InvocationError{Outcome: "not_started", Code: "profile_constraint_violation"}
	}
	return validation
}
