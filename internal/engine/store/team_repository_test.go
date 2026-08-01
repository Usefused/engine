package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func TestTeamValidation(t *testing.T) {
	valid := TeamMutation{Name: "Payments", Slug: "payments-platform", Description: "Owns payment integrations"}
	if err := validateTeamMutation(valid); err != nil {
		t.Fatalf("valid team: %v", err)
	}
	for name, input := range map[string]TeamMutation{
		"blank name":       {Name: "", Slug: "payments"},
		"untrimmed name":   {Name: " Payments", Slug: "payments"},
		"uppercase slug":   {Name: "Payments", Slug: "Payments"},
		"repeated hyphen":  {Name: "Payments", Slug: "payments--ops"},
		"long description": {Name: "Payments", Slug: "payments", Description: strings.Repeat("x", maxTeamDescriptionLength+1)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateTeamMutation(input); !errors.Is(err, ErrInvalidTeam) {
				t.Fatalf("validation error = %v, want ErrInvalidTeam", err)
			}
		})
	}
}

func TestTeamBindingValidationRequiresRoleAtMatchingScope(t *testing.T) {
	actor := MutationActor{SubjectID: uuid.New(), CredentialID: uuid.New()}
	valid := TeamBindingMutation{
		TeamID: uuid.New(), RoleSlug: accesscontrol.RoleServiceUser,
		Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: uuid.New()}, Actor: actor,
	}
	if err := validateTeamBindingMutation(valid); err != nil {
		t.Fatalf("valid service binding: %v", err)
	}
	valid.Resource.Type = accesscontrol.ResourceBucket
	if err := validateTeamBindingMutation(valid); !errors.Is(err, ErrInvalidTeamBinding) {
		t.Fatalf("scope mismatch error = %v, want ErrInvalidTeamBinding", err)
	}
	valid.Resource.Type = accesscontrol.ResourceService
	valid.RoleSlug = "custom-role"
	if err := validateTeamBindingMutation(valid); !errors.Is(err, ErrInvalidTeamBinding) {
		t.Fatalf("custom role error = %v, want ErrInvalidTeamBinding", err)
	}
}
