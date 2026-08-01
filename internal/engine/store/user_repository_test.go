package store

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestNormalizeUserEmailPreservesDisplayAndNormalizesIdentity(t *testing.T) {
	normalized, display, err := normalizeUserEmail(" Alice.Example@Example.COM ")
	if err != nil {
		t.Fatalf("normalizeUserEmail: %v", err)
	}
	if normalized != "alice.example@example.com" || display != "Alice.Example@Example.COM" {
		t.Fatalf("email = %q/%q", normalized, display)
	}
	for _, invalid := range []string{"", "not-an-email", "Alice <alice@example.com>", "a@example.com\nBcc:x@example.com"} {
		if _, _, err := normalizeUserEmail(invalid); !errors.Is(err, ErrInvalidUser) {
			t.Fatalf("normalizeUserEmail(%q) error = %v, want ErrInvalidUser", invalid, err)
		}
	}
}

func TestGeneratedUserDisplayNameUsesBoundedLocalPart(t *testing.T) {
	if got := generatedUserDisplayName("person@example.com"); got != "person" {
		t.Fatalf("generated display name = %q", got)
	}
}

func TestValidateMutationActorRejectsUnsafeAuditContext(t *testing.T) {
	actor := MutationActor{SubjectID: uuid.New(), CredentialID: uuid.New(), RequestID: "fused_secret"}
	if err := validateMutationActor(actor); !errors.Is(err, ErrInvalidMutationActor) {
		t.Fatalf("unsafe request ID error = %v, want ErrInvalidMutationActor", err)
	}
}
