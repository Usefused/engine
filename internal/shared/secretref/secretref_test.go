package secretref

import "testing"

func TestParse_ExplicitBucketForm(t *testing.T) {
	ref, err := Parse("${bucket.prod.secret.webhook_signing}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.Bucket != "prod" || ref.Kind != KindSecret || ref.Key != "webhook_signing" {
		t.Fatalf("got %+v", ref)
	}
}

func TestParse_ExplicitBucketEnvForm(t *testing.T) {
	ref, err := Parse("${bucket.prod.env.region}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.Bucket != "prod" || ref.Kind != KindEnv || ref.Key != "region" {
		t.Fatalf("got %+v", ref)
	}
}

func TestParse_ShorthandDefaultsToDefaultBucket(t *testing.T) {
	ref, err := Parse("${bucket.secret.webhook_signing}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.Bucket != DefaultBucket || ref.Kind != KindSecret || ref.Key != "webhook_signing" {
		t.Fatalf("got %+v, want bucket=%q key=webhook_signing", ref, DefaultBucket)
	}
}

func TestParse_RejectsMalformedReferences(t *testing.T) {
	cases := []string{
		"",
		"webhook_signing",
		"$WEBHOOK_SECRET",
		"bucket.prod.secret.webhook_signing", // missing brackets (old grammar)
		"${bucket.prod.webhook_signing}",     // missing kind segment
		"${bucket..secret.key}",
		"${bucket.prod.secret.}",
		"${bucket.prod.secret}",
		"${bucket.prod.secret.key.extra}",
		"${resource.provider_resource_id}", // wrong namespace
		"prefix-${bucket.prod.secret.key}", // merged with surrounding text
		"${bucket.prod.secret.key}-suffix", // merged with surrounding text
		"${}",
	}
	for _, value := range cases {
		if _, err := Parse(value); err == nil {
			t.Errorf("Parse(%q): expected error, got none", value)
		}
	}
}

func TestRef_StringRendersCanonicalForm(t *testing.T) {
	ref := Ref{Bucket: "prod", Kind: KindSecret, Key: "webhook_signing"}
	if got, want := ref.String(), "${bucket.prod.secret.webhook_signing}"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRef_StringRendersEnvKind(t *testing.T) {
	ref := Ref{Bucket: "prod", Kind: KindEnv, Key: "region"}
	if got, want := ref.String(), "${bucket.prod.env.region}"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRef_SecretKeyNameAddsPrefix(t *testing.T) {
	ref := Ref{Bucket: "prod", Kind: KindSecret, Key: "webhook_signing"}
	if got, want := ref.SecretKeyName(), "secret:webhook_signing"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParse_ShorthandRoundTripsToExplicitCanonicalForm(t *testing.T) {
	ref, err := Parse("${bucket.secret.webhook_signing}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := ref.String(), "${bucket.default.secret.webhook_signing}"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSingleTag(t *testing.T) {
	tests := []struct {
		name  string
		value string
		inner string
		ok    bool
	}{
		{name: "valid tag", value: "${bucket.secret.key}", inner: "bucket.secret.key", ok: true},
		{name: "no braces", value: "plain-value", ok: false},
		{name: "missing suffix", value: "${bucket.secret.key", ok: false},
		{name: "missing prefix", value: "bucket.secret.key}", ok: false},
		{name: "empty tag", value: "${}", ok: false},
		{name: "merged with prefix text", value: "prefix-${bucket.secret.key}", ok: false},
		{name: "merged with suffix text", value: "${bucket.secret.key}-suffix", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner, ok := SingleTag(tt.value)
			if ok != tt.ok || inner != tt.inner {
				t.Fatalf("SingleTag(%q) = (%q, %v), want (%q, %v)", tt.value, inner, ok, tt.inner, tt.ok)
			}
		})
	}
}
