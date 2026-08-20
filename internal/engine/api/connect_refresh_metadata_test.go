package api

import (
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// TestProjectAuthConnectionRefreshMetadata proves safe refresh scheduling state
// is visible without projecting private CAS lease or encrypted credential data.
func TestProjectAuthConnectionRefreshMetadata(t *testing.T) {
	versionID := uuid.New()
	attemptedAt := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	refreshedAt := attemptedAt.Add(time.Minute)
	retryAt := attemptedAt.Add(time.Hour)
	connection := store.AuthConnection{
		ID: uuid.New(), BucketID: uuid.New(), ServiceID: uuid.New(), ServiceVersionID: versionID,
		AuthType: "oauth", AuthName: "OAuth2", RefreshState: "ok",
		LastRefreshAttemptAt: &attemptedAt, LastRefreshedAt: &refreshedAt, RefreshRetryNotBefore: &retryAt,
		EncryptedAccessToken: "private-access", EncryptedRefreshToken: "private-refresh", EncryptedDEK: "private-dek",
	}

	response := projectAuthConnection(connection)
	if response.ServiceVersionID == nil || *response.ServiceVersionID != versionID {
		t.Fatal("expected exact service version identity")
	}
	if response.LastRefreshAttemptAt != &attemptedAt || response.LastRefreshedAt != &refreshedAt || response.RefreshRetryNotBefore != &retryAt {
		t.Fatal("expected safe refresh timestamps")
	}
	projection := projectGraphQLAuthConnection(connection)
	assertRefreshMetadataProjection(t, projection, versionID, attemptedAt, refreshedAt, retryAt)
}

// assertRefreshMetadataProjection checks the public GraphQL map without ever
// copying credential values into failure output.
func assertRefreshMetadataProjection(t *testing.T, projection map[string]interface{}, versionID uuid.UUID, attemptedAt, refreshedAt, retryAt time.Time) {
	t.Helper()
	expected := map[string]interface{}{
		"service_version_id":       versionID.String(),
		"last_refresh_attempt_at":  attemptedAt.Format(time.RFC3339Nano),
		"last_refreshed_at":        refreshedAt.Format(time.RFC3339Nano),
		"refresh_retry_not_before": retryAt.Format(time.RFC3339Nano),
	}
	for key, value := range expected {
		if projection[key] != value {
			t.Fatalf("unexpected safe refresh metadata for %s", key)
		}
	}
	for _, forbidden := range []string{"encrypted_access_token", "encrypted_refresh_token", "encrypted_dek", "refresh_lease_token", "refresh_lease_expires_at"} {
		if _, exists := projection[forbidden]; exists {
			t.Fatalf("private refresh field %s was projected", forbidden)
		}
	}
}

// TestProjectAuthConnectionKeepsLegacyVersionAmbiguous proves migration-safe
// rows do not masquerade as the all-zero service version in public metadata.
func TestProjectAuthConnectionKeepsLegacyVersionAmbiguous(t *testing.T) {
	response := projectAuthConnection(store.AuthConnection{})
	if response.ServiceVersionID != nil {
		t.Fatal("expected ambiguous legacy version to remain null")
	}
	if projectGraphQLAuthConnection(store.AuthConnection{})["service_version_id"] != nil {
		t.Fatal("expected GraphQL legacy version to remain null")
	}
}
