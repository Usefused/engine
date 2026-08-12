package sandbox

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Usefused/engine/internal/shared/authrouting"
)

func TestRuntimeSecurityAlternativesAboveLegacyBoundRoundTrip(t *testing.T) {
	const alternativeCount = 98
	want := runtimeSecurityAlternatives(alternativeCount)
	payload, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal security requirements: %v", err)
	}
	var got authrouting.Requirements
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal security requirements: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("security requirements changed during JSON round trip")
	}
	if err := validateSecurityRequirements(got, map[string]string{"oauth": "oauth"}); err != nil {
		t.Fatalf("validateSecurityRequirements: %v", err)
	}
}

func TestRuntimeSecurityAlternativesRemainBounded(t *testing.T) {
	requirements := runtimeSecurityAlternatives(maxRuntimeSecurityAlternatives + 1)
	if err := validateSecurityRequirements(requirements, map[string]string{"oauth": "oauth"}); err == nil {
		t.Fatal("expected oversized security requirements to be rejected")
	}
}

func runtimeSecurityAlternatives(count int) authrouting.Requirements {
	requirements := make(authrouting.Requirements, count)
	for index := range requirements {
		requirements[index] = authrouting.Alternative{Schemes: []authrouting.Requirement{{
			Scheme: "oauth", Scopes: []string{"read"},
		}}}
	}
	return requirements
}
