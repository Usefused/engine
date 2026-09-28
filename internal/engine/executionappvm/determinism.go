package executionappvm

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"time"
)

const maxJavaScriptDateMillis int64 = 8640000000000000

// Determinism pins JavaScript time and random output for one execution and its replay.
type Determinism struct {
	StartedAtUnixMs int64  `json:"startedAtUnixMs,string"`
	RandomSeed      uint64 `json:"randomSeed,string"`
}

// NewDeterminism creates controls from Engine time and cryptographic entropy, not authored input.
func NewDeterminism() (Determinism, error) {
	var seed [8]byte
	// Entropy failure must stop a replayable execution instead of choosing a predictable fallback.
	if _, err := rand.Read(seed[:]); err != nil {
		return Determinism{}, errors.New("capability randomness is unavailable")
	}
	return Determinism{StartedAtUnixMs: time.Now().UTC().UnixMilli(), RandomSeed: binary.BigEndian.Uint64(seed[:])}, nil
}

// Valid reports whether controls can be represented by the JavaScript Date runtime.
func (control Determinism) Valid() bool {
	// Zero is reserved for absent controls, while JavaScript Date bounds cap forged evidence.
	return control.StartedAtUnixMs > 0 && control.StartedAtUnixMs <= maxJavaScriptDateMillis
}

// capabilityRandomSource uses a specified SplitMix64 stream with stable 53-bit float conversion.
func capabilityRandomSource(seed uint64) func() float64 {
	return func() float64 {
		seed += 0x9e3779b97f4a7c15
		value := seed
		value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9
		value = (value ^ (value >> 27)) * 0x94d049bb133111eb
		value ^= value >> 31
		return float64(value>>11) / (1 << 53)
	}
}
