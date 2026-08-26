package mcpsession

import (
	"strings"
	"testing"
)

// TestMetadataAdmission rejects oversized/private display controls and noncanonical addresses before durable storage.
func TestMetadataAdmission(t *testing.T) {
	cases := []struct {
		value Metadata
		valid bool
	}{
		{Metadata{}, true},
		{Metadata{ClientName: "Example Agent", ClientVersion: "1.2.3", InitialClientIP: "2001:db8::1"}, true},
		{Metadata{ClientName: strings.Repeat("a", 128)}, true},
		{Metadata{ClientName: strings.Repeat("a", 129)}, false},
		{Metadata{ClientVersion: "1\n2"}, false},
		{Metadata{ClientName: string([]byte{0xff})}, false},
		{Metadata{InitialClientIP: "not-an-address"}, false},
		{Metadata{InitialClientIP: "::ffff:192.0.2.1"}, false},
		{Metadata{InitialClientIP: "fe80::1%zone"}, false},
	}
	for index, test := range cases {
		// Producer and worker must agree on the exact same bounded metadata contract.
		if test.value.Valid() != test.valid {
			t.Fatalf("case %d validity mismatch", index)
		}
	}
}
