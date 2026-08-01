package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

type TeamStatus string

const (
	TeamStatusActive   TeamStatus = "active"
	TeamStatusArchived TeamStatus = "archived"

	maxTeamNameLength        = 100
	maxTeamSlugLength        = 63
	maxTeamDescriptionLength = 500
)

var (
	ErrInvalidTeam          = errors.New("invalid team")
	ErrInvalidTeamBinding   = errors.New("invalid team binding")
	ErrInvalidMutationActor = errors.New("invalid mutation actor")
	ErrTeamNotFound         = errors.New("team not found")
	ErrTeamSlugConflict     = errors.New("team slug already exists")
	ErrTeamArchiveConflict  = errors.New("team cannot be archived while it owns resources or has access bindings")
	ErrTeamArchived         = errors.New("archived team cannot receive access bindings")
	teamSlugPattern         = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

type Team struct {
	ID          uuid.UUID
	Name        string
	Slug        string
	Description string
	Status      TeamStatus
	Bindings    []TeamBinding
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type TeamBinding struct {
	ID                  uuid.UUID
	TeamID              uuid.UUID
	RoleSlug            string
	RoleDisplayName     string
	Resource            accesscontrol.ResourceRef
	ResourceDisplayName string
	CreatedAt           time.Time
}

type MutationActor struct {
	SubjectID    uuid.UUID
	CredentialID uuid.UUID
	RequestID    string
	TraceID      string
}

type TeamMutation struct {
	Name        string
	Slug        string
	Description string
	Actor       MutationActor
}

// TeamPatch keeps partial GraphQL updates atomic; the repository applies the
// patch to the locked row rather than making handlers perform a racy read then
// write sequence.
type TeamPatch struct {
	Name        *string
	Slug        *string
	Description *string
	Actor       MutationActor
}

type TeamListOptions struct {
	Statuses []TeamStatus
	Search   string
	Limit    int
	Offset   int
}

type TeamBindingMutation struct {
	TeamID   uuid.UUID
	RoleSlug string
	Resource accesscontrol.ResourceRef
	Actor    MutationActor
}

type TeamMutationResult struct {
	Team                  Team
	AuthorizationRevision int64
	Changed               bool
}

type TeamBindingMutationResult struct {
	Binding               TeamBinding
	AuthorizationRevision int64
	Changed               bool
}

type TeamArchiveConflictError struct {
	BindingCount        int
	ActiveArtifactCount int
}

func (e *TeamArchiveConflictError) Error() string {
	return fmt.Sprintf("%v: %d binding(s), %d active artifact(s)", ErrTeamArchiveConflict, e.BindingCount, e.ActiveArtifactCount)
}

func (e *TeamArchiveConflictError) Unwrap() error { return ErrTeamArchiveConflict }

type TeamRepository interface {
	CreateTeam(context.Context, TeamMutation) (TeamMutationResult, error)
	GetTeam(context.Context, uuid.UUID) (Team, error)
	ListTeams(context.Context, TeamListOptions) ([]Team, int, error)
	UpdateTeam(context.Context, uuid.UUID, TeamPatch) (TeamMutationResult, error)
	ArchiveTeam(context.Context, uuid.UUID, MutationActor) (TeamMutationResult, error)
	AddTeamBinding(context.Context, TeamBindingMutation) (TeamBindingMutationResult, error)
	RemoveTeamBinding(context.Context, TeamBindingMutation) (TeamBindingMutationResult, error)
	ClearTeamWorkspaceRole(context.Context, uuid.UUID, uuid.UUID, MutationActor) (TeamBindingMutationResult, error)
}

func validateTeamMutation(input TeamMutation) error {
	if err := validateTeamText("name", input.Name, maxTeamNameLength, true); err != nil {
		return err
	}
	if err := validateTeamSlug(input.Slug); err != nil {
		return err
	}
	return validateTeamText("description", input.Description, maxTeamDescriptionLength, false)
}

func validateTeamPatch(input TeamPatch) error {
	if input.Name != nil {
		if err := validateTeamText("name", *input.Name, maxTeamNameLength, true); err != nil {
			return err
		}
	}
	if input.Slug != nil {
		if err := validateTeamSlug(*input.Slug); err != nil {
			return err
		}
	}
	if input.Description != nil {
		return validateTeamText("description", *input.Description, maxTeamDescriptionLength, false)
	}
	return nil
}

func validateTeamText(field, value string, maxLength int, required bool) error {
	if strings.TrimSpace(value) != value || len(value) > maxLength || (required && value == "") {
		return fmt.Errorf("%w: %s must be trimmed and between %d and %d characters", ErrInvalidTeam, field, boolInt(required), maxLength)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%w: %s contains control characters", ErrInvalidTeam, field)
		}
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func validateTeamSlug(slug string) error {
	if len(slug) < 1 || len(slug) > maxTeamSlugLength || !teamSlugPattern.MatchString(slug) {
		return fmt.Errorf("%w: slug must contain lowercase letters, numbers, and single hyphens", ErrInvalidTeam)
	}
	return nil
}

func validateTeamStatus(status TeamStatus) error {
	if status == TeamStatusActive || status == TeamStatusArchived {
		return nil
	}
	return fmt.Errorf("%w: invalid status %q", ErrInvalidTeam, status)
}

func validateMutationActor(actor MutationActor) error {
	if actor.SubjectID == uuid.Nil || actor.CredentialID == uuid.Nil {
		return ErrInvalidMutationActor
	}
	audit := accesscontrol.AuditEvent{
		Action: "access.mutation", Outcome: accesscontrol.AuditSucceeded,
		RequestID: actor.RequestID, TraceID: actor.TraceID,
	}
	if err := audit.Validate(); err != nil {
		return ErrInvalidMutationActor
	}
	return nil
}

func validateTeamBindingMutation(input TeamBindingMutation) error {
	if input.TeamID == uuid.Nil || input.Resource.ID == uuid.Nil || validateMutationActor(input.Actor) != nil {
		return ErrInvalidTeamBinding
	}
	if err := accesscontrol.ValidateResourceType(input.Resource.Type); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidTeamBinding, err)
	}
	expected, ok := teamRoleResourceTypes[input.RoleSlug]
	if !ok || expected != input.Resource.Type {
		return fmt.Errorf("%w: role %q cannot bind to %q", ErrInvalidTeamBinding, input.RoleSlug, input.Resource.Type)
	}
	return nil
}

var teamRoleResourceTypes = map[string]accesscontrol.ResourceType{
	accesscontrol.RoleOwner:           accesscontrol.ResourceWorkspace,
	accesscontrol.RoleAdmin:           accesscontrol.ResourceWorkspace,
	accesscontrol.RoleBuilder:         accesscontrol.ResourceWorkspace,
	accesscontrol.RoleViewer:          accesscontrol.ResourceWorkspace,
	accesscontrol.RoleServiceUser:     accesscontrol.ResourceService,
	accesscontrol.RoleServiceManager:  accesscontrol.ResourceService,
	accesscontrol.RoleBucketUser:      accesscontrol.ResourceBucket,
	accesscontrol.RoleBucketManager:   accesscontrol.ResourceBucket,
	accesscontrol.RoleArtifactReader:  accesscontrol.ResourceArtifact,
	accesscontrol.RoleArtifactManager: accesscontrol.ResourceArtifact,
}
