// Package workerlimits shares bounded worker capacity between deployment configuration and the isolated child protocol.
package workerlimits

const DefaultConcurrency = 4
const MaxConcurrency = 1024

// Occupancy retains the existing 28 waiting places while allowing the configured number of active interpreters.
func Occupancy(concurrency int) int { return concurrency + 28 }
