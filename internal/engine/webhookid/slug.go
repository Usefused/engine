// Package webhookid generates the opaque tokens used to identify Engine
// webhook ingress URLs (POST /webhook/{slug}-{serviceSlug}).
//
// This lives in its own package rather than being inlined in the apply
// handler or the ingress handler because both sides must agree on the exact
// same fixed character width: the apply path (engine/api) generates a slug
// once when a webhook registration is created, and the ingress path
// (engine/sandbox) truncates an inbound URL segment to that same width to
// discard the decorative "-{serviceSlug}" suffix and anything a provider
// glues onto the path after it (e.g. Plunk/Stripe appending an event name).
// One shared constant means those two call sites can never drift apart.
package webhookid

import (
	"crypto/rand"
	"fmt"
)

// SlugLength is the fixed character width of a generated webhook slug and
// the number of leading characters the ingress handler must keep when
// normalizing an inbound URL segment. Both call sites import this constant
// rather than hardcoding a number, so the two can never disagree.
const SlugLength = 21

// alphabet is nanoid's default URL-safe character set (64 characters, chosen
// so 256 % len(alphabet) == 0 -- see Generate). It deliberately includes '-',
// which is why the ingress truncation must always be fixed-width rather than
// delimiter-based: a generated slug can legitimately contain a '-' before
// the boundary we actually care about.
const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"

// Generate returns a new random SlugLength-character token suitable for use
// as an opaque webhook ingress identifier.
//
// Hand-rolled via crypto/rand instead of pulling in a nanoid dependency --
// mapping SlugLength random bytes through a 64-character alphabet is exactly
// what nanoid does internally, and 256 is an exact multiple of 64, so a
// simple modulo introduces no distribution bias. This keeps the dependency
// footprint unchanged while matching nanoid's collision characteristics.
func Generate() (string, error) {
	buf := make([]byte, SlugLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("webhookid: generate: %w", err)
	}
	out := make([]byte, SlugLength)
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out), nil
}
