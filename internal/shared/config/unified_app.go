package config

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
)

// UnifiedAppAdmissionConfig is deployment policy shared by every hosted app on an Engine.
type UnifiedAppAdmissionConfig struct {
	MaxConcurrency      int `yaml:"max_concurrency"`
	QueueCapacity       int `yaml:"queue_capacity"`
	QueueTimeoutSeconds int `yaml:"queue_timeout_seconds"`
}

// DefaultUnifiedAppAdmissionConfig leaves scheduler headroom while bounding queued requests and their lifetime.
func DefaultUnifiedAppAdmissionConfig() UnifiedAppAdmissionConfig {
	return UnifiedAppAdmissionConfig{MaxConcurrency: max(1, runtime.GOMAXPROCS(0)-1), QueueCapacity: 256, QueueTimeoutSeconds: 5}
}

// ResolveUnifiedAppAdmission applies explicit process overrides after YAML and validates the final policy.
func ResolveUnifiedAppAdmission(policy UnifiedAppAdmissionConfig) (UnifiedAppAdmissionConfig, error) {
	fields := []struct {
		name  string
		value *int
	}{
		{"FUSED_UNIFIED_APP_MAX_CONCURRENCY", &policy.MaxConcurrency},
		{"FUSED_UNIFIED_APP_QUEUE_CAPACITY", &policy.QueueCapacity},
		{"FUSED_UNIFIED_APP_QUEUE_TIMEOUT_SECONDS", &policy.QueueTimeoutSeconds},
	}
	for _, field := range fields {
		raw, present := os.LookupEnv(field.name)
		// Absent overrides preserve the YAML value or its default, including partial YAML sections.
		if !present {
			continue
		}
		parsed, err := strconv.Atoi(raw)
		// An explicitly empty or malformed override must not silently use another concurrency policy.
		if err != nil {
			return policy, fmt.Errorf("%s must be a positive integer", field.name)
		}
		*field.value = parsed
	}
	return policy, policy.Validate()
}

// Validate rejects unsafe limits regardless of whether they came from YAML, environment, or an embedded caller.
func (policy UnifiedAppAdmissionConfig) Validate() error {
	fields := []struct {
		name           string
		value, maximum int
	}{
		{"max_concurrency", policy.MaxConcurrency, 1024},
		{"queue_capacity", policy.QueueCapacity, 65536},
		{"queue_timeout_seconds", policy.QueueTimeoutSeconds, 300},
	}
	for _, field := range fields {
		// Zero is an explicit invalid value, not an implicit unlimited or default sentinel.
		if field.value < 1 || field.value > field.maximum {
			return fmt.Errorf("engine.unified_apps.%s must be between 1 and %d", field.name, field.maximum)
		}
	}
	return nil
}
