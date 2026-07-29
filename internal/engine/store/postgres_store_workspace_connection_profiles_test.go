package store

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestValidateWorkspaceProfileWriteRequiresRegistryBaselineIdentity keeps the
// store boundary aligned with the database constraint, returning a useful
// error before a transaction attempts to persist an unauditable baseline.
func TestValidateWorkspaceProfileWriteRequiresRegistryBaselineIdentity(t *testing.T) {
	profile := validWorkspaceProfileForValidation("baseline")
	err := validateWorkspaceProfileWrite(profile, nil)
	if err == nil || !strings.Contains(err.Error(), "registry profile ID") {
		t.Fatalf("expected missing Registry profile identity error, got %v", err)
	}

	registryProfileID := uuid.New()
	profile.RegistryProfileID = &registryProfileID
	if err := validateWorkspaceProfileWrite(profile, nil); err != nil {
		t.Fatalf("valid baseline rejected: %v", err)
	}
}

// TestValidateWorkspaceProfileWriteRejectsRegistryIdentityOnOverride prevents
// workspace-authored state from masquerading as a Registry publication.
func TestValidateWorkspaceProfileWriteRejectsRegistryIdentityOnOverride(t *testing.T) {
	profile := validWorkspaceProfileForValidation("override")
	registryProfileID := uuid.New()
	profile.RegistryProfileID = &registryProfileID
	if err := validateWorkspaceProfileWrite(profile, nil); err == nil {
		t.Fatal("expected workspace override with Registry identity to be rejected")
	}
}

// validWorkspaceProfileForValidation supplies the unrelated mandatory fields
// so each test isolates one layer/Registry identity decision.
func validWorkspaceProfileForValidation(layer string) WorkspaceConnectionProfile {
	return WorkspaceConnectionProfile{
		ServiceID: uuid.New(), ServiceVersionID: uuid.New(), AuthType: "oauth", Layer: layer, ProfileRevision: 1,
		ProfileHash: "profile-hash", Provenance: map[string]string{"baseline": "provider", "override": "workspace"}[layer],
		ProfileSnapshot: []byte(`{"auth_type":"oauth"}`),
	}
}
