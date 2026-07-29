// Package secretref parses the explicit, self-contained bucket-reference
// grammar introduced by plans/plan-service-config-restructure.md item 4:
//
//	${bucket.<bucket-name>.secret.<key-name>}
//	${bucket.secret.<key-name>}            (shorthand: bucket name omitted -> "default")
//
// (and the "env" counterpart of each, for a bucket's non-secret values).
//
// This grammar is deliberately whole-value only -- a value must be exactly
// one ${...} tag, never merged with surrounding text -- because a webhook
// registration's SecretRef is a standalone reference field, not a template.
// Contrast engine/requestbinding, whose ${...} tags are allowed to merge
// with arbitrary surrounding text because SDK/MCP injection values are
// genuinely templates (e.g. "Bearer ${bucket.secrets.API_KEY}").
//
// The bucket name is mandatory to name explicitly (or default) here because,
// unlike SDK/MCP dispatch, webhook verification has no caller-selected
// bucket in its path (see webhookIngressHandler) to fall back on -- every
// reference must be self-contained.
package secretref

import (
	"errors"
	"strings"
)

// KeyPrefix namespaces every bucket-scoped named secret's row in
// fused_workspace_secrets (KeyName column) so this generic mechanism can
// never collide with the older, webhook-specific "webhook_secret:<label>"
// scheme it replaces.
const KeyPrefix = "secret:"

// DefaultBucket is the bucket a reference resolves against when the
// bucket-name segment is omitted -- matches the pre-existing (if accidental)
// hardcoded default in the old GetWebhookSecret lookup, so single-bucket
// workspaces don't need to name a bucket explicitly.
const DefaultBucket = "default"

// Kind distinguishes which bucket-scoped store a reference resolves
// against. Both are recognized by the grammar; today only Secret is
// actually consumed (by webhook signing secrets) -- Env exists so the same
// standalone-reference grammar can address a bucket's non-secret values
// later without inventing a second parser.
type Kind int

const (
	KindEnv Kind = iota
	KindSecret
)

func (k Kind) String() string {
	if k == KindSecret {
		return "secret"
	}
	return "env"
}

var errInvalidRef = errors.New(`bucket reference must be "${bucket.<name>.env|secret.<key>}" or "${bucket.env|secret.<key>}"`)

// Ref is a parsed, self-contained bucket reference: which bucket, which
// store within that bucket (env values vs. secrets), which key. Bucket is
// always the resolved name -- Parse never returns an empty Bucket, even for
// the shorthand form.
type Ref struct {
	Bucket string
	Kind   Kind
	Key    string
}

// SingleTag returns the inner content of value when it is exactly one
// ${...} tag with no other characters, and reports false otherwise.
// Exported so other whole-value-only grammars (connectionprofile's
// ${resource.*} expressions) can share this check instead of re-implementing
// the same prefix/suffix trim -- see that package's parseDynamicExpression.
func SingleTag(value string) (string, bool) {
	if !strings.HasPrefix(value, "${") || !strings.HasSuffix(value, "}") {
		return "", false
	}
	inner := value[2 : len(value)-1]
	if inner == "" {
		return "", false
	}
	return inner, true
}

// Parse accepts exactly the four forms documented on the package. Errors
// never echo the input back verbatim: a malformed reference is closer to
// config material than to a value worth including in an error string, and
// keeping the message static avoids ever echoing something that turns out to
// contain a pasted secret by mistake.
func Parse(value string) (Ref, error) {
	inner, ok := SingleTag(value)
	if !ok {
		return Ref{}, errInvalidRef
	}
	return parseBucketPath(inner)
}

// parseBucketPath classifies the content already extracted from a tag by
// SingleTag. Named and shorthand forms share this one switch because the
// only difference between them is whether the bucket segment is present.
func parseBucketPath(path string) (Ref, error) {
	parts := strings.Split(path, ".")
	switch {
	case len(parts) == 4 && parts[0] == "bucket" && isKindWord(parts[2]):
		return newRef(parts[1], parts[2], parts[3])
	case len(parts) == 3 && parts[0] == "bucket" && isKindWord(parts[1]):
		return newRef(DefaultBucket, parts[1], parts[2])
	default:
		return Ref{}, errInvalidRef
	}
}

func isKindWord(word string) bool {
	return word == "env" || word == "secret"
}

func newRef(bucket, kindWord, key string) (Ref, error) {
	if strings.TrimSpace(bucket) == "" || strings.TrimSpace(key) == "" {
		return Ref{}, errInvalidRef
	}
	kind := KindEnv
	if kindWord == "secret" {
		kind = KindSecret
	}
	return Ref{Bucket: bucket, Kind: kind, Key: key}, nil
}

// String renders the canonical explicit form. Used to normalize a
// shorthand reference before it's persisted, so every stored row is
// resolvable the same way regardless of which form the caller wrote.
func (r Ref) String() string {
	return "${bucket." + r.Bucket + "." + r.Kind.String() + "." + r.Key + "}"
}

// SecretKeyName is the KeyName this reference resolves to inside its
// bucket's row in fused_workspace_secrets. Only meaningful for Kind ==
// KindSecret; a Kind == KindEnv reference resolves against the bucket's
// separate non-secret value store instead (see store.GetBucketValues) and
// has no use for this method.
func (r Ref) SecretKeyName() string {
	return KeyPrefix + r.Key
}
