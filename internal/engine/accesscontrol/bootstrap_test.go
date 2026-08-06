package accesscontrol

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

type bootstrapRepositoryStub struct {
	input  BootstrapInput
	result BootstrapResult
	err    error
}

func (s *bootstrapRepositoryStub) ReconcileBootstrapOwner(_ context.Context, input BootstrapInput) (BootstrapResult, error) {
	s.input = input
	return s.result, s.err
}

func TestBootstrapOwnerHashesLicenseKeyAndSeedsRoles(t *testing.T) {
	accountID := uuid.New()
	repository := &bootstrapRepositoryStub{result: BootstrapResult{SubjectID: uuid.New(), Revision: 2, Changed: true}}
	result, err := BootstrapOwner(context.Background(), repository, accountID, "fsk_license_secret", "owner@example.com")
	if err != nil {
		t.Fatalf("BootstrapOwner: %v", err)
	}
	if result != repository.result {
		t.Fatalf("result = %#v, want %#v", result, repository.result)
	}
	if repository.input.AccountID != accountID {
		t.Fatalf("account ID = %s, want %s", repository.input.AccountID, accountID)
	}
	if repository.input.CredentialHash == "fsk_license_secret" || repository.input.CredentialHash != HashControlCredential("fsk_license_secret") {
		t.Fatalf("credential was not hashed correctly: %q", repository.input.CredentialHash)
	}
	if repository.input.CredentialPrefix != "fsk_lice" {
		t.Fatalf("credential prefix = %q, want fsk_lice", repository.input.CredentialPrefix)
	}
	if repository.input.OwnerEmail != "owner@example.com" {
		t.Fatalf("owner email = %q, want owner@example.com", repository.input.OwnerEmail)
	}
	if len(repository.input.Roles) != len(BuiltInRoles()) {
		t.Fatalf("role count = %d, want %d", len(repository.input.Roles), len(BuiltInRoles()))
	}
}

func TestBootstrapOwnerRejectsMissingInputs(t *testing.T) {
	tests := []struct {
		name       string
		repository BootstrapRepository
		accountID  uuid.UUID
		licenseKey string
	}{
		{name: "repository", accountID: uuid.New(), licenseKey: "key"},
		{name: "account", repository: &bootstrapRepositoryStub{}, licenseKey: "key"},
		{name: "license", repository: &bootstrapRepositoryStub{}, accountID: uuid.New()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := BootstrapOwner(context.Background(), test.repository, test.accountID, test.licenseKey)
			if !errors.Is(err, ErrInvalidBootstrap) {
				t.Fatalf("error = %v, want ErrInvalidBootstrap", err)
			}
		})
	}
}

func TestCredentialPrefixHandlesShortCredentials(t *testing.T) {
	got := CredentialPrefix("short")
	if got == "short" || got != HashControlCredential("short")[:8] {
		t.Fatalf("CredentialPrefix = %q, want non-reversible fingerprint", got)
	}
}
