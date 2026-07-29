package requestbinding

import (
	"reflect"
	"testing"
)

func TestExtractVariables(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "no variables",
			input:    "Bearer some-token",
			expected: nil,
		},
		{
			name:     "single variable",
			input:    "Bearer ${bucket.env.API_KEY}",
			expected: []string{"bucket.env.API_KEY"},
		},
		{
			name:     "multiple variables",
			input:    "${bucket.env.USER}:${bucket.secrets.PASS}",
			expected: []string{"bucket.env.USER", "bucket.secrets.PASS"},
		},
		{
			name:     "unbalanced brackets",
			input:    "Bearer ${bucket.env.API_KEY",
			expected: nil,
		},
		{
			name:     "nested brackets ignored or matched to first",
			input:    "${bucket.env.${NESTED}}",
			expected: []string{"bucket.env.${NESTED"}, // As per simple regex `[^}]+`, it stops at first `}`.
		},
		{
			name:     "empty brackets",
			input:    "Bearer ${}",
			expected: nil, // Our regex `[^}]+` requires at least one character.
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExtractVariables(tt.input)
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("ExtractVariables(%q) = %v, expected %v", tt.input, result, tt.expected)
			}
		})
	}
}

func TestInterpolate(t *testing.T) {
	values := map[string]string{
		"bucket.env.API_KEY":  "secret123",
		"bucket.env.USER":     "admin",
		"bucket.secrets.PASS": "hunter2",
	}

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "no interpolation needed",
			input:    "Bearer some-token",
			expected: "Bearer some-token",
		},
		{
			name:     "single interpolation",
			input:    "Bearer ${bucket.env.API_KEY}",
			expected: "Bearer secret123",
		},
		{
			name:     "multiple interpolation",
			input:    "${bucket.env.USER}:${bucket.secrets.PASS}",
			expected: "admin:hunter2",
		},
		{
			name:     "missing key leaves tag unresolved",
			input:    "Bearer ${bucket.env.MISSING}",
			expected: "Bearer ${bucket.env.MISSING}",
		},
		{
			name:     "mixed found and missing",
			input:    "${bucket.env.USER}:${bucket.secrets.MISSING}",
			expected: "admin:${bucket.secrets.MISSING}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Interpolate(tt.input, values)
			if result != tt.expected {
				t.Errorf("Interpolate(%q) = %q, expected %q", tt.input, result, tt.expected)
			}
		})
	}
}
