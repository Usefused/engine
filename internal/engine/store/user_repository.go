package store

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

type UserStatus string
type MembershipRole string

const (
	UserStatusInvited   UserStatus = "invited"
	UserStatusActive    UserStatus = "active"
	UserStatusSuspended UserStatus = "suspended"
	UserStatusArchived  UserStatus = "archived"

	MembershipRoleMember  MembershipRole = "member"
	MembershipRoleManager MembershipRole = "manager"
)

var (
	ErrInvalidUser               = errors.New("invalid user")
	ErrUserNotFound              = errors.New("user not found")
	ErrUserEmailConflict         = errors.New("user email already exists")
	ErrUserArchived              = errors.New("archived user cannot be changed")
	ErrInvalidTeamMembership     = errors.New("invalid team membership")
	ErrTeamMembershipNotFound    = errors.New("team membership not found")
	ErrInvalidControlCredential  = errors.New("invalid control credential")
	ErrControlCredentialNotFound = errors.New("control credential not found")
	ErrLastEffectiveOwner        = errors.New("operation would remove the last effective Owner")
	ErrSelfSuspensionForbidden   = errors.New("you cannot suspend your own account")
)

type User struct {
	ID                   uuid.UUID
	Email                string
	DisplayName          string
	Status               UserStatus
	OwnerProtected       bool
	Memberships          []TeamMembership
	MembershipsTruncated bool
	Credentials          []ControlCredential
	CredentialsTruncated bool
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type TeamMembership struct {
	TeamID     uuid.UUID      `json:"team_id"`
	TeamName   string         `json:"team_name"`
	TeamSlug   string         `json:"team_slug"`
	TeamStatus TeamStatus     `json:"team_status"`
	Role       MembershipRole `json:"role"`
	CreatedAt  time.Time      `json:"created_at"`
}

type TeamMember struct {
	UserID         uuid.UUID
	Email          string
	DisplayName    string
	UserStatus     UserStatus
	MembershipRole MembershipRole
	CreatedAt      time.Time
}

type ControlCredential struct {
	ID         uuid.UUID  `json:"id"`
	UserID     uuid.UUID  `json:"user_id"`
	KeyPrefix  string     `json:"key_prefix"`
	Name       string     `json:"name"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

type EffectiveAccessGrant struct {
	Permission        accesscontrol.Permission
	Resource          accesscontrol.ResourceRef
	RoleSlug          string
	SourceType        string
	SourceID          uuid.UUID
	SourceDisplayName string
}

type UserListOptions struct {
	Statuses        []UserStatus
	Search          string
	Limit           int
	Offset          int
	IncludeChildren bool
}

type CreateUserInput struct {
	Email       string
	DisplayName string
	Actor       MutationActor
}

type UserPatch struct {
	Email       *string
	DisplayName *string
	Actor       MutationActor
}

type TeamMemberMutation struct {
	TeamID uuid.UUID
	UserID uuid.UUID
	Role   MembershipRole
	Actor  MutationActor
}

type AddTeamMemberByEmailInput struct {
	TeamID      uuid.UUID
	Email       string
	DisplayName string
	Role        MembershipRole
	Actor       MutationActor
}

type IssueCredentialInput struct {
	UserID     uuid.UUID
	Name       string
	ExpiresAt  *time.Time
	Source     string
	AuthMethod string
	Actor      MutationActor
}

type UserMutationResult struct {
	User                  User
	AuthorizationRevision int64
	Changed               bool
}

type MembershipMutationResult struct {
	User                  User
	Membership            TeamMembership
	AuthorizationRevision int64
	Changed               bool
	CreatedUser           bool
}

type IssuedControlCredential struct {
	Credential            ControlCredential
	RawKey                string
	AuthorizationRevision int64
	Changed               bool
}

type CredentialMutationResult struct {
	Credential            ControlCredential
	AuthorizationRevision int64
	Changed               bool
}

type UserRepository interface {
	CreateUser(context.Context, CreateUserInput) (UserMutationResult, error)
	GetUser(context.Context, uuid.UUID) (User, error)
	ListUsers(context.Context, UserListOptions) ([]User, int, error)
	UpdateUser(context.Context, uuid.UUID, UserPatch) (UserMutationResult, error)
	SuspendUser(context.Context, uuid.UUID, MutationActor) (UserMutationResult, error)
	ReactivateUser(context.Context, uuid.UUID, MutationActor) (UserMutationResult, error)
	AddTeamMember(context.Context, TeamMemberMutation) (MembershipMutationResult, error)
	AddTeamMemberByEmail(context.Context, AddTeamMemberByEmailInput) (MembershipMutationResult, error)
	RemoveTeamMember(context.Context, uuid.UUID, uuid.UUID, MutationActor) (MembershipMutationResult, error)
	ListTeamMembers(context.Context, uuid.UUID, UserListOptions) ([]TeamMember, int, error)
	GetUserEffectiveAccess(context.Context, uuid.UUID) ([]EffectiveAccessGrant, int64, error)
	IssueUserControlCredential(context.Context, IssueCredentialInput) (IssuedControlCredential, error)
	ListUserControlCredentials(context.Context, uuid.UUID) ([]ControlCredential, error)
	RevokeUserControlCredential(context.Context, uuid.UUID, uuid.UUID, MutationActor) (CredentialMutationResult, error)
}

func normalizeUserEmail(email string) (string, string, error) {
	display := strings.TrimSpace(email)
	if display == "" || len(display) > 254 || strings.ContainsAny(display, "\r\n") {
		return "", "", fmt.Errorf("%w: invalid email", ErrInvalidUser)
	}
	parsed, err := mail.ParseAddress(display)
	if err != nil || parsed.Name != "" || !strings.EqualFold(parsed.Address, display) {
		return "", "", fmt.Errorf("%w: invalid email", ErrInvalidUser)
	}
	return strings.ToLower(display), display, nil
}

func validateUserDisplayName(name string) error {
	if name == "" || len(name) > 100 || strings.TrimSpace(name) != name {
		return fmt.Errorf("%w: display name must be trimmed and between 1 and 100 characters", ErrInvalidUser)
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			return fmt.Errorf("%w: display name contains control characters", ErrInvalidUser)
		}
	}
	return nil
}

func validateMembershipRole(role MembershipRole) error {
	if role == MembershipRoleMember || role == MembershipRoleManager {
		return nil
	}
	return fmt.Errorf("%w: invalid membership role %q", ErrInvalidTeamMembership, role)
}

func validateUserStatus(status UserStatus) error {
	switch status {
	case UserStatusInvited, UserStatusActive, UserStatusSuspended, UserStatusArchived:
		return nil
	default:
		return fmt.Errorf("%w: invalid status %q", ErrInvalidUser, status)
	}
}
