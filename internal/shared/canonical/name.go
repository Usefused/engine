// Package canonical provides shared Unicode-aware name normalization for
// SDK and MCP identity. Engine and CLI must use exactly the same rules so
// the same name groups to the same app family regardless of which component
// issues the grouping.
//
// Rules are designed to be stable, idempotent, and safe to use as a
// database unique key without worrying about presentation-only variations.
package canonical

import (
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// AppName normalizes a raw SDK or MCP name into its canonical identity form.
// It returns the normalized form and the original casing preserved as
// display_name. An error is returned only for names that are empty or contain
// forbidden control characters.
//
// Normalization steps:
//  1. Trim surrounding Unicode whitespace (includes non-breaking spaces).
//  2. Normalize to Unicode NFC so composed/decomposed forms are identical.
//  3. Apply Unicode case folding for identity — "MySdk" and "mysdk" are the
//     same family.
//
// Punctuation — including colons — is preserved. Names containing colons are
// valid; splitting on ':' to extract name from a composite key is never
// correct and must be done from the structured desired-state fields instead.
func AppName(raw string) (canonical string, display string, err error) {
	if raw == "" {
		return "", "", &InvalidNameError{Reason: "name is empty"}
	}

	// Step 1: trim Unicode whitespace, not just ASCII space.
	trimmed := strings.TrimFunc(raw, unicode.IsSpace)
	if trimmed == "" {
		return "", "", &InvalidNameError{Reason: "name contains only whitespace"}
	}

	// Step 2: Unicode NFC normalization so that é (precomposed) and e + ´
	// (decomposed) produce the same canonical form.
	nfc := norm.NFC.String(trimmed)

	// Step 3: case folding for identity. We keep the original NFC form as
	// the display name so UI surfaces show what the user typed.
	folded := cases.Fold().String(nfc)

	// Forbid control characters that would be invisible or dangerous in
	// logs, config keys, or URLs.
	for _, r := range folded {
		if unicode.IsControl(r) {
			return "", "", &InvalidNameError{Reason: "name contains control characters"}
		}
	}

	return folded, nfc, nil
}

// InvalidNameError describes why a name was rejected.
type InvalidNameError struct {
	Reason string
}

func (e *InvalidNameError) Error() string {
	return "invalid app name: " + e.Reason
}
