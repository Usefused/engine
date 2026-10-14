package store

import (
	"testing"

	"github.com/google/uuid"
)

// TestSecretExportPostgres verifies migration defaults, fresh policy reads, metadata projection, and atomic replacement against PostgreSQL.
func TestSecretExportPostgres(t *testing.T) {
	fixture := setupWorkspaceSecretsIntegrationFixture(t)
	repository := fixture.store
	secret := WorkspaceSecret{WorkspaceSecretMeta: WorkspaceSecretMeta{BucketID: fixture.bucketID, ServiceID: fixture.serviceID, KeyName: "export-test", CredentialType: "api_key"}, EncryptedDEK: "wrapped", EncryptedValue: "ciphertext"}
	// Existing callers omit the flag and must create private credentials.
	if err := repository.UpsertSecret(fixture.ctx, secret); err != nil {
		t.Fatal(err)
	}
	metas, err := repository.ListSecretMeta(fixture.ctx, fixture.bucketID)
	// Metadata must expose only the flag and opaque identity, with private as the default.
	if err != nil || len(metas) != 1 || metas[0].Exportable {
		t.Fatalf("metadata err=%v count=%d", err, len(metas))
	}
	secretID := metas[0].ID
	assertExportRead(t, repository, fixture, secretID, false)
	assertExportPolicyLifecycle(t, fixture, secretID)
	assertExportReplacement(t, fixture, secret, secretID)
}

// assertExportRead checks only presence and opaque identity so test failures never print stored values.
func assertExportRead(t *testing.T, repository Store, fixture workspaceSecretsIntegrationFixture, id uuid.UUID, allowed bool) {
	t.Helper()
	secret, err := repository.GetExportableSecret(fixture.ctx, fixture.bucketID, id)
	// Presence must match current policy on every read, without a warmed runtime cache changing the result.
	if err != nil || (secret != nil) != allowed {
		t.Fatalf("read allowed=%t error=%v", allowed, err)
	}
}

// assertExportPolicyLifecycle verifies point reads, grouped metadata, and immediate policy revocation.
func assertExportPolicyLifecycle(t *testing.T, fixture workspaceSecretsIntegrationFixture, secretID uuid.UUID) {
	t.Helper()
	repository := fixture.store
	changed, err := repository.SetSecretExportable(fixture.ctx, fixture.bucketID, secretID, true)
	// Policy opt-in must affect the exact existing row without replacing its encrypted value.
	if err != nil || !changed {
		t.Fatalf("opt in: %v", err)
	}
	assertExportRead(t, repository, fixture, secretID, true)
	wrong, err := repository.GetExportableSecret(fixture.ctx, uuid.New(), secretID)
	// Another bucket cannot reveal this row even when its ID is known.
	if err != nil || wrong != nil {
		t.Fatal("cross-bucket reveal")
	}
	paged, _, err := repository.ListSecretMetaPage(fixture.ctx, fixture.bucketID, 20, 0)
	// Grouped metadata must retain export state without decrypting any material.
	if err != nil || len(paged) != 1 || !paged[0].Exportable {
		t.Fatalf("paged export metadata: %v", err)
	}
	_, err = repository.SetSecretExportable(fixture.ctx, fixture.bucketID, secretID, false)
	// Revocation is visible through the same fresh storage path immediately.
	if err != nil {
		t.Fatal(err)
	}
	assertExportRead(t, repository, fixture, secretID, false)
}

// assertExportReplacement proves that single and bulk writes replace consent and cannot expose unsupported material.
func assertExportReplacement(t *testing.T, fixture workspaceSecretsIntegrationFixture, secret WorkspaceSecret, secretID uuid.UUID) {
	t.Helper()
	repository := fixture.store

	secret.Exportable = true
	// Bulk and single writes must share explicit flag semantics.
	if err := repository.UpsertSecrets(fixture.ctx, []WorkspaceSecret{secret}); err != nil {
		t.Fatal(err)
	}
	assertExportRead(t, repository, fixture, secretID, true)
	secret.Exportable = false
	// Replacing without renewed consent resets exportability instead of inheriting old policy.
	if err := repository.UpsertSecret(fixture.ctx, secret); err != nil {
		t.Fatal(err)
	}
	assertExportRead(t, repository, fixture, secretID, false)
	secret.CredentialType = "oauth"
	secret.Exportable = true
	// Even inconsistent stored flags must not make unsupported credential families retrievable.
	if err := repository.UpsertSecret(fixture.ctx, secret); err != nil {
		t.Fatal(err)
	}
	assertExportRead(t, repository, fixture, secretID, false)
	changed, err := repository.SetSecretExportable(fixture.ctx, fixture.bucketID, secretID, true)
	// Policy mutation must also reject unsupported rows independently of read filtering.
	if err != nil || changed {
		t.Fatal("OAuth export policy accepted")
	}
}

// TestSecretExportPostgresLegacyAndGeneric preserves old UI key spellings and service-independent secret ownership.
func TestSecretExportPostgresLegacyAndGeneric(t *testing.T) {
	fixture := setupWorkspaceSecretsIntegrationFixture(t)
	for _, metadata := range []WorkspaceSecretMeta{
		{BucketID: fixture.bucketID, ServiceID: fixture.serviceID, KeyName: "legacy", CredentialType: "apiKey"},
		{BucketID: fixture.bucketID, ServiceID: uuid.Nil, KeyName: "secret:generic", CredentialType: "bucket_secret"},
	} {
		secret := WorkspaceSecret{WorkspaceSecretMeta: metadata, EncryptedDEK: "wrapped", EncryptedValue: "ciphertext"}
		// Seed the exact historical spelling without involving current write normalization.
		if err := fixture.store.UpsertSecret(fixture.ctx, secret); err != nil {
			t.Fatal(err)
		}
		stored, err := fixture.store.GetSecret(fixture.ctx, metadata.BucketID, metadata.ServiceID, metadata.KeyName)
		// Point lookup yields the authoritative opaque ID independently of metadata paging.
		if err != nil || stored == nil {
			t.Fatal("missing seeded secret")
		}
		changed, err := fixture.store.SetSecretExportable(fixture.ctx, metadata.BucketID, stored.ID, true)
		// Both allowed families must accept opt-in without rewriting their stored value.
		if err != nil || !changed {
			t.Fatal("eligible secret rejected")
		}
		assertExportRead(t, fixture.store, fixture, stored.ID, true)
	}
}
