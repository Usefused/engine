// Package mcpsession owns the bounded, credential-free session provenance contract.
package mcpsession

import (
	"net/netip"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxClientFieldBytes = 128

// Metadata is permission-gated history, never a tracing or metrics dimension.
type Metadata struct {
	ClientName      string `json:"client_name,omitempty"`
	ClientVersion   string `json:"client_version,omitempty"`
	InitialClientIP string `json:"initial_client_ip,omitempty"`
}

// ValidText rejects control characters and oversized client claims instead of truncating identities.
func ValidText(value string) bool {
	return len(value) <= MaxClientFieldBytes && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}

// Valid admits absent historical metadata but forbids values that cannot safely enter durable history.
func (metadata Metadata) Valid() bool {
	// Client claims are display-only and must obey the same bounds at producer and worker admission.
	if !ValidText(metadata.ClientName) || !ValidText(metadata.ClientVersion) {
		return false
	}
	// Older sessions cannot be retroactively attributed to a network peer.
	if metadata.InitialClientIP == "" {
		return true
	}
	address, err := netip.ParseAddr(metadata.InitialClientIP)
	return err == nil && address.Zone() == "" && address.Unmap().String() == metadata.InitialClientIP
}
