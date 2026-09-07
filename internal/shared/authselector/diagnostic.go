// Package authselector owns bounded, credential-free diagnostics for public
// auth_type/auth_name routing selectors.
package authselector

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

const (
	// SuggestionLimit keeps selector errors useful without dumping a large provider contract.
	SuggestionLimit = 5
	maxLabelBytes   = 128
	maxDetailRunes  = 900
)

// Selection is one exact public auth family and service-specific scheme name.
type Selection struct {
	AuthType string `json:"auth_type,omitempty"`
	AuthName string `json:"auth_name,omitempty"`
}

// NotFoundError is the shared pre-provider contract for an invalid auth selector.
type NotFoundError struct {
	Code        string      `json:"code"`
	Message     string      `json:"message"`
	Remediation string      `json:"remediation"`
	Requested   Selection   `json:"requested"`
	Available   []Selection `json:"available"`
}

// NewNotFoundError constructs one sanitized selector failure for runtime transports.
func NewNotFoundError(requested Selection, available []Selection) *NotFoundError {
	safeRequested := Selection{AuthType: SafeLabel(requested.AuthType), AuthName: SafeLabel(requested.AuthName)}
	safeAvailable := Normalize(available)
	message := "authentication selector " + FormatSelection(safeRequested, "auth_type", "auth_name") + " does not match this operation"
	// Exact alternatives turn a rejection into a correction without exposing any credential material.
	if len(safeAvailable) > 0 {
		message += "; valid auth selections: " + FormatSelections(available, "auth_type", "auth_name")
	} else {
		message += "; no safe auth selections are available for this operation"
	}
	return &NotFoundError{
		Code: "auth_selection_not_found", Message: BoundDetail(message),
		Remediation: "Use one of the listed auth_type/auth_name pairs and retry.",
		Requested:   safeRequested, Available: safeAvailable,
	}
}

// Error exposes only the already-sanitized shared diagnostic message.
func (err *NotFoundError) Error() string {
	return err.Message
}

// Normalize sanitizes, de-duplicates, sorts, and bounds exact selector choices.
func Normalize(selections []Selection) []Selection {
	normalized := normalizeUnbounded(selections)
	// The caller can inspect service authentication metadata when more valid choices exist.
	if len(normalized) > SuggestionLimit {
		normalized = normalized[:SuggestionLimit]
	}
	return normalized
}

// FormatSelections renders normalized exact choices using the caller surface's field names.
func FormatSelections(selections []Selection, typeKey, nameKey string) string {
	normalized := normalizeUnbounded(selections)
	visible := normalized[:min(len(normalized), SuggestionLimit)]
	labels := make([]string, 0, len(visible)+1)
	for _, selection := range visible {
		labels = append(labels, FormatSelection(selection, typeKey, nameKey))
	}
	// An omission marker prevents a bounded list from implying it is exhaustive.
	if len(normalized) > SuggestionLimit {
		labels = append(labels, "[additional selections omitted; inspect service authentication details]")
	}
	return BoundDetail(strings.Join(labels, "; "))
}

// normalizeUnbounded applies safety, de-duplication, and stable ordering before any presentation cap.
func normalizeUnbounded(selections []Selection) []Selection {
	normalized := make([]Selection, 0, len(selections))
	seen := make(map[string]struct{}, len(selections))
	for _, selection := range selections {
		authType, authName := SafeLabel(selection.AuthType), SafeLabel(selection.AuthName)
		// Unsafe or partial contract labels cannot become copyable selector guidance.
		if authType == "" || authName == "" {
			continue
		}
		key := authType + "\x00" + authName
		// De-duplication keeps the omitted count about distinct authored choices.
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, Selection{AuthType: authType, AuthName: authName})
	}
	sort.Slice(normalized, func(left, right int) bool {
		// Type-first ordering groups schemes using the same credential family.
		if normalized[left].AuthType == normalized[right].AuthType {
			return normalized[left].AuthName < normalized[right].AuthName
		}
		return normalized[left].AuthType < normalized[right].AuthType
	})
	return normalized
}

// FormatSelection renders one selector without ever emitting an unsafe label verbatim.
func FormatSelection(selection Selection, typeKey, nameKey string) string {
	return typeKey + "=" + QuoteLabel(selection.AuthType) + ", " + nameKey + "=" + QuoteLabel(selection.AuthName)
}

// QuoteLabel safely quotes one admitted display label or emits a fixed omission marker.
func QuoteLabel(value string) string {
	label := SafeLabel(value)
	// Unsafe metadata must be omitted rather than echoed or guessed.
	if label == "" {
		return "[label omitted]"
	}
	return fmt.Sprintf("%q", label)
}

// SafeLabel admits bounded display metadata while rejecting credential and terminal-control shapes.
func SafeLabel(value string) string {
	// Reject overlong values before scanning so metadata cannot inflate error output.
	if len(value) > maxLabelBytes {
		return ""
	}
	// Controls and bidi formatting must not spoof the selector named by an error.
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return ""
		}
	}
	lower := strings.ToLower(value)
	// Credential and transport shapes are never safe selector labels.
	for _, marker := range []string{"fsk_", "-----begin ", "://", "authorization:", "authorization=", "bearer ", "access_token=", "refresh_token=", "client_secret=", "password=", "api_key=", "apikey=", "secret="} {
		if strings.Contains(lower, marker) {
			return ""
		}
	}
	return strings.TrimSpace(value)
}

// BoundDetail preserves Unicode while leaving room under the CLI's 1,024-rune server-detail ceiling.
func BoundDetail(value string) string {
	runes := []rune(value)
	// Mark truncation explicitly so callers know the diagnostic is not exhaustive.
	if len(runes) > maxDetailRunes {
		return string(runes[:maxDetailRunes]) + "… [additional detail omitted]"
	}
	return value
}
