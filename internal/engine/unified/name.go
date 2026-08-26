package unified

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidPublicName shares exact, bounded display identity across authoring,
// runtime admission and durable receipts so an executable target cannot lose its audit.
func ValidPublicName(value string, maxBytes int) bool {
	return value != "" && len(value) <= maxBytes && value == strings.TrimSpace(value) && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}
