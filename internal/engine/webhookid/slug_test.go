package webhookid

import (
	"strings"
	"testing"
)

func TestGenerate_FixedLength(t *testing.T) {
	slug, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if len(slug) != SlugLength {
		t.Fatalf("expected length %d, got %d (%q)", SlugLength, len(slug), slug)
	}
}

func TestGenerate_OnlyUsesDeclaredAlphabet(t *testing.T) {
	slug, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	for _, r := range slug {
		if !strings.ContainsRune(alphabet, r) {
			t.Fatalf("slug contains character outside declared alphabet: %q in %q", r, slug)
		}
	}
}

// TestGenerate_NoCollisionsAcrossManyCalls is a basic sanity check, not a
// statistical uniqueness proof -- it just confirms repeated calls don't
// panic and don't trivially repeat within a small sample.
func TestGenerate_NoCollisionsAcrossManyCalls(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		slug, err := Generate()
		if err != nil {
			t.Fatalf("Generate() error on iteration %d: %v", i, err)
		}
		if len(slug) != SlugLength {
			t.Fatalf("unexpected length on iteration %d: got %d", i, len(slug))
		}
		if seen[slug] {
			t.Fatalf("collision detected within %d generations: %q", i, slug)
		}
		seen[slug] = true
	}
}
