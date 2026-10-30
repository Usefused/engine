package bridgehost

import (
	"encoding/json"
	"fmt"
	"testing"

	bridgev1 "github.com/Usefused/engine/pkg/bridge/v1"
	"github.com/stretchr/testify/require"
)

// TestProfileConstraintSchemaProvenance checks the same rejection before Engine dispatch and snapshot lookup.
func TestProfileConstraintSchemaProvenance(t *testing.T) {
	base := json.RawMessage(`{"type":"object","required":["app"],"properties":{"app":{"type":"string","enum":["Calculator","Notes"]}},"additionalProperties":false}`)
	policy := json.RawMessage(`{"type":"object","required":["app"],"properties":{"app":{"enum":["Calculator"]}}}`)
	marked := map[string]any{"x-fused-bridge-profile-constraints": 1, "allOf": []json.RawMessage{base, policy}}
	schema, err := json.Marshal(marked)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, input     string
		allowed, policy bool
	}{
		{"allowed", `{"app":"Calculator"}`, true, false},
		{"profile denied", `{"app":"Notes"}`, false, true},
		{"connector denied", `{"app":"Messages"}`, false, false},
		{"wrong type", `{"app":42}`, false, false},
		{"extra property", `{"app":"Notes","private":"value"}`, false, false},
		{"missing property", `{}`, false, false},
		{"malformed", `{`, false, false},
	} {
		// Base-schema errors must never masquerade as additional profile restrictions.
		t.Run(tc.name, func(t *testing.T) {
			failure := validateBridgeInput(schema, []byte(tc.input))
			require.Equal(t, tc.allowed, failure == nil)
			require.Equal(t, tc.policy, IsProfileConstraintViolation(failure))
		})
	}
	delete(marked, "x-fused-bridge-profile-constraints")
	legacy, err := json.Marshal(marked)
	require.NoError(t, err)
	require.False(t, IsProfileConstraintViolation(validateBridgeInput(legacy, []byte(`{"app":"Notes"}`))))
	marked["x-fused-bridge-profile-constraints"] = 2
	unknownVersion, err := json.Marshal(marked)
	require.NoError(t, err)
	require.False(t, IsProfileConstraintViolation(validateBridgeInput(unknownVersion, []byte(`{"app":"Notes"}`))))
	marked["x-fused-bridge-profile-constraints"] = 1
	marked["not"] = map[string]any{}
	extraAssertion, err := json.Marshal(marked)
	require.NoError(t, err)
	require.False(t, IsProfileConstraintViolation(validateBridgeInput(extraAssertion, []byte(`{"app":"Notes"}`))))
}

// TestProfileConstraintResultCorrelation preserves only explicit, correlated pre-dispatch denials.
func TestProfileConstraintResultCorrelation(t *testing.T) {
	call := &bridgev1.Call{RequestId: "call", Generation: 7}
	for _, tc := range []struct {
		name, outcome, code, request string
		generation                   uint64
		denied                       bool
	}{
		{"policy", "not_started", "profile_constraint_violation", "call", 7, true},
		{"invalid input", "not_started", "invalid_input", "call", 7, false},
		{"offline", "not_started", "profile_offline", "call", 7, false},
		{"uncertain", "unknown", "profile_constraint_violation", "call", 7, false},
		{"wrong request", "not_started", "profile_constraint_violation", "other", 7, false},
		{"wrong generation", "not_started", "profile_constraint_violation", "call", 6, false},
	} {
		// Each case models protocol evidence without dispatching a connector command.
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateResult(&bridgev1.CallResult{RequestId: tc.request, Generation: tc.generation, Outcome: tc.outcome, ErrorCode: tc.code}, call, Contract{})
			require.Error(t, err)
			require.Equal(t, tc.denied, IsProfileConstraintViolation(fmt.Errorf("wrapped: %w", err)))
			// The safe explanation is emitted only for the positively identified policy refusal.
			if tc.denied {
				require.Equal(t, ProfileConstraintMessage, err.Error())
			}
		})
	}
}
