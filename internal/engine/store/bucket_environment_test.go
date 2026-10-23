package store

import (
	"maps"
	"testing"

	"github.com/google/uuid"
)

// TestBucketEnvironmentResolution keeps global values usable while preserving service overrides and bucket isolation.
func TestBucketEnvironmentResolution(t *testing.T) {
	fixture := setupWorkspaceSecretsIntegrationFixture(t)
	for _, value := range []BucketValue{
		{BucketID: fixture.bucketID, ServiceID: uuid.Nil, KeyName: "REGION", Location: "env", Value: "global"},
		{BucketID: fixture.bucketID, ServiceID: fixture.serviceID, KeyName: "REGION", Location: "env", Value: "service"},
		{BucketID: fixture.bucketID, ServiceID: uuid.Nil, KeyName: "BASE_URL", Location: "env", Value: "https://example.test"},
		{BucketID: fixture.bucketID, ServiceID: uuid.Nil, KeyName: "GLOBAL_HEADER", Location: "header", Value: "private-to-global-scope"},
	} {
		// Seed only through the production storage validator.
		if err := fixture.store.UpsertBucketValue(fixture.ctx, value); err != nil {
			t.Fatal(err)
		}
	}
	values, err := fixture.store.GetBucketValues(fixture.ctx, fixture.bucketID, fixture.serviceID, []string{"REGION", "BASE_URL", "GLOBAL_HEADER", "MISSING"})
	assertBucketEnvironmentValues(t, values, err, map[string]string{"REGION": "service", "BASE_URL": "https://example.test"})
	values, err = fixture.store.GetBucketValues(fixture.ctx, fixture.bucketID, uuid.New(), []string{"REGION"})
	assertBucketEnvironmentValues(t, values, err, map[string]string{"REGION": "global"})
	values, err = fixture.store.GetBucketValues(fixture.ctx, uuid.New(), fixture.serviceID, []string{"REGION"})
	assertBucketEnvironmentValues(t, values, err, map[string]string{})
}

// assertBucketEnvironmentValues verifies both exact key/value resolution and absence of duplicate fallback rows.
func assertBucketEnvironmentValues(t *testing.T, values []BucketValue, err error, expected map[string]string) {
	t.Helper()
	// Failed reads cannot be interpreted as missing variables.
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, value := range values {
		got[value.KeyName] = value.Value
	}
	// Counts detect duplicate scoped/global rows even if a map would hide them.
	if len(values) != len(expected) || !maps.Equal(got, expected) {
		t.Fatalf("environment=%v want=%v", got, expected)
	}
}
