package apptokeninvalidation

import "github.com/google/uuid"

// FanoutInvalidator keeps cache eviction and volatile-session teardown behind
// the existing opaque-token contract. The revocation service therefore has one
// ordering guarantee without learning which runtime consumers hold the token.
type FanoutInvalidator struct {
	targets []Invalidator
}

func NewFanoutInvalidator(targets ...Invalidator) *FanoutInvalidator {
	filtered := make([]Invalidator, 0, len(targets))
	for _, target := range targets {
		if target != nil {
			filtered = append(filtered, target)
		}
	}
	return &FanoutInvalidator{targets: filtered}
}

func (invalidator *FanoutInvalidator) InvalidateToken(tokenID uuid.UUID) int {
	if invalidator == nil {
		return 0
	}
	total := 0
	for _, target := range invalidator.targets {
		total += target.InvalidateToken(tokenID)
	}
	return total
}
