package engine

import (
	"testing"

	"github.com/Usefused/engine/internal/shared/paginationpolicy"
)

func FuzzPaginationRepeatedContinuationTerminates(f *testing.F) {
	f.Add("cursor-1")
	f.Add("")
	f.Add("https://provider.test/items?page=2")
	f.Fuzz(func(t *testing.T, next string) {
		policy := &paginationpolicy.Config{
			Continuation: []paginationpolicy.ContinuationStep{{
				Kind: paginationpolicy.ContinuationToken, State: "cursor", ResponseValue: "next",
			}},
			Termination: paginationpolicy.Termination{RepeatedValue: paginationpolicy.RepeatedStop},
		}
		state := &paginationV3State{values: map[string]any{}, visited: map[string]map[string]struct{}{}}
		response := paginationV3Response{values: map[string]any{"next": next}, present: map[string]bool{"next": true}}
		stopped, err := advancePaginationV3(policy, state, response, nil)
		if err != nil || stopped {
			t.Fatalf("first continuation stopped=%t err=%v", stopped, err)
		}
		stopped, err = advancePaginationV3(policy, state, response, nil)
		if err != nil || !stopped || state.stopReason != "repeated_value" {
			t.Fatalf("repeated continuation stopped=%t reason=%q err=%v", stopped, state.stopReason, err)
		}
	})
}
