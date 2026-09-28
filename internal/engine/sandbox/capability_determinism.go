package sandbox

import "github.com/Usefused/engine/internal/engine/executionappvm"

// CapabilityDeterminism pins JavaScript time and random output for one execution and replay.
type CapabilityDeterminism = executionappvm.Determinism

// NewCapabilityDeterminism creates replay controls from Engine time and cryptographic entropy.
func NewCapabilityDeterminism() (CapabilityDeterminism, error) {
	return executionappvm.NewDeterminism()
}
