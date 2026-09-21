// Package oauthmapping validates credential-free authorization principal selectors.
package oauthmapping

import (
	"errors"
	"regexp"
	"strings"
)

var fieldName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// Validate prevents scope aliases from overriding Engine-owned authorization fields and bounds token traversal.
func Validate(parameter string, path []string) error {
	// An empty parameter preserves the standard OAuth scope field.
	if parameter != "" && !fieldName.MatchString(parameter) {
		return errors.New("invalid OAuth scope parameter")
	}
	// Alternate scope locations must never overwrite credentials or authorization session integrity.
	switch strings.ToLower(parameter) {
	case "response_type", "client_id", "redirect_uri", "state", "code_challenge", "code_challenge_method", "nonce", "client_secret", "grant_type", "code", "code_verifier", "refresh_token", "access_token", "id_token":
		return errors.New("reserved OAuth scope parameter")
	}
	// Provider token selectors are literal object keys, not executable queries or unbounded traversals.
	if len(path) > 8 {
		return errors.New("OAuth token response path is too deep")
	}
	for _, segment := range path {
		// Simple identifiers intentionally exclude array indexes, wildcard queries, and ambiguous escaping.
		if !fieldName.MatchString(segment) {
			return errors.New("invalid OAuth token response path")
		}
	}
	return nil
}
