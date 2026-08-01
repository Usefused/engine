package accesscontrol

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var (
	ErrInvalidRequirement = errors.New("invalid authorization requirement")
	ErrPermissionDenied   = errors.New("permission denied")
)

type Grant struct {
	Permission Permission
	Resource   ResourceRef
}

type AuthorizedScope struct {
	All bool
	IDs []uuid.UUID
}

type AuthorizationSnapshot struct {
	Revision int64
	grants   map[Permission]*permissionGrant
}

type permissionGrant struct {
	all         bool
	resourceIDs map[ResourceType]map[uuid.UUID]struct{}
}

// EffectiveGrants returns a stable, defensive projection of the permissions
// already loaded for the actor. Workspace-wide grants are projected against
// the actor's workspace because the compact snapshot intentionally stores
// them as an all-resources bit instead of retaining a redundant UUID.
func (s AuthorizationSnapshot) EffectiveGrants(workspaceID uuid.UUID) []Grant {
	grants := make([]Grant, 0, len(s.grants))
	for permission, permissionScope := range s.grants {
		if permissionScope.all {
			grants = append(grants, Grant{Permission: permission, Resource: ResourceRef{Type: ResourceWorkspace, ID: workspaceID}})
			continue
		}
		for resourceType, resourceIDs := range permissionScope.resourceIDs {
			for resourceID := range resourceIDs {
				grants = append(grants, Grant{Permission: permission, Resource: ResourceRef{Type: resourceType, ID: resourceID}})
			}
		}
	}
	sort.Slice(grants, func(i, j int) bool {
		return grantSortKey(grants[i]) < grantSortKey(grants[j])
	})
	return grants
}

func grantSortKey(grant Grant) string {
	return string(grant.Permission) + "\x00" + string(grant.Resource.Type) + "\x00" + grant.Resource.ID.String()
}

type PermissionDeniedError struct {
	Missing      []Requirement
	DisplayNames map[ResourceRef]string
}

// MissingRequirements returns a defensive copy of the requirements that
// actually caused a denial. Non-permission errors intentionally return nil.
func MissingRequirements(err error) []Requirement {
	var denied *PermissionDeniedError
	if !errors.As(err, &denied) {
		return nil
	}
	return append([]Requirement(nil), denied.Missing...)
}

func (e *PermissionDeniedError) Error() string {
	return fmt.Sprintf("%v: %d requirement(s) missing", ErrPermissionDenied, len(e.Missing))
}

func (e *PermissionDeniedError) Unwrap() error {
	return ErrPermissionDenied
}

type Authorizer interface {
	CheckAll(ctx context.Context, actor Actor, requirements ...Requirement) error
	Scope(ctx context.Context, actor Actor, permission Permission, resourceType ResourceType) (AuthorizedScope, error)
}

type SnapshotAuthorizer struct{}

func NewAuthorizationSnapshot(revision int64, grants ...Grant) (AuthorizationSnapshot, error) {
	snapshot := AuthorizationSnapshot{
		Revision: revision,
		grants:   make(map[Permission]*permissionGrant),
	}
	for _, grant := range grants {
		if err := validateGrant(grant); err != nil {
			return AuthorizationSnapshot{}, err
		}
		snapshot.addGrant(grant)
	}
	return snapshot, nil
}

func (SnapshotAuthorizer) CheckAll(ctx context.Context, actor Actor, requirements ...Requirement) error {
	started := time.Now()
	outcome := "invalid"
	defer func() { recordAuthorizationDuration(ctx, started, outcome) }()
	unique, err := uniqueRequirements(requirements)
	if err != nil {
		recordAuthorizationCheck(ctx, len(requirements), 0, "invalid")
		return err
	}

	missing := make([]Requirement, 0)
	for _, requirement := range unique {
		if !actor.Authorization.allows(requirement) {
			missing = append(missing, requirement)
		}
	}
	if len(missing) > 0 {
		outcome = "denied"
		recordAuthorizationCheck(ctx, len(unique), len(missing), "denied")
		return &PermissionDeniedError{Missing: missing}
	}
	outcome = "allowed"
	recordAuthorizationCheck(ctx, len(unique), 0, "allowed")
	return nil
}

func (SnapshotAuthorizer) Scope(ctx context.Context, actor Actor, permission Permission, resourceType ResourceType) (AuthorizedScope, error) {
	started := time.Now()
	outcome := "invalid"
	defer func() { recordAuthorizationDuration(ctx, started, outcome) }()
	if err := ValidatePermission(permission); err != nil {
		return AuthorizedScope{}, fmt.Errorf("%w: %v", ErrInvalidRequirement, err)
	}
	if err := ValidateResourceType(resourceType); err != nil {
		return AuthorizedScope{}, fmt.Errorf("%w: %v", ErrInvalidRequirement, err)
	}

	scope := actor.Authorization.scope(permission, resourceType)
	outcome = "scoped"
	recordAuthorizationCheck(ctx, 1, 0, "scoped")
	return scope, nil
}

func (s AuthorizationSnapshot) addGrant(grant Grant) {
	permissionScope := s.grants[grant.Permission]
	if permissionScope == nil {
		permissionScope = &permissionGrant{resourceIDs: make(map[ResourceType]map[uuid.UUID]struct{})}
		s.grants[grant.Permission] = permissionScope
	}
	// A workspace binding intentionally dominates resource-specific bindings;
	// retaining their IDs would waste memory and imply a narrower grant.
	if grant.Resource.Type == ResourceWorkspace {
		permissionScope.all = true
		permissionScope.resourceIDs = nil
		return
	}
	if permissionScope.all {
		return
	}
	ids := permissionScope.resourceIDs[grant.Resource.Type]
	if ids == nil {
		ids = make(map[uuid.UUID]struct{})
		permissionScope.resourceIDs[grant.Resource.Type] = ids
	}
	ids[grant.Resource.ID] = struct{}{}
}

func (s AuthorizationSnapshot) allows(requirement Requirement) bool {
	permissionScope := s.grants[requirement.Permission]
	if permissionScope == nil {
		return false
	}
	if permissionScope.all {
		return true
	}
	_, ok := permissionScope.resourceIDs[requirement.Resource.Type][requirement.Resource.ID]
	return ok
}

func (s AuthorizationSnapshot) scope(permission Permission, resourceType ResourceType) AuthorizedScope {
	permissionScope := s.grants[permission]
	if permissionScope == nil {
		return AuthorizedScope{}
	}
	if permissionScope.all {
		return AuthorizedScope{All: true}
	}

	ids := make([]uuid.UUID, 0, len(permissionScope.resourceIDs[resourceType]))
	for id := range permissionScope.resourceIDs[resourceType] {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return AuthorizedScope{IDs: ids}
}

func uniqueRequirements(requirements []Requirement) ([]Requirement, error) {
	unique := make([]Requirement, 0, len(requirements))
	seen := make(map[Requirement]struct{}, len(requirements))
	for _, requirement := range requirements {
		if err := validateRequirement(requirement); err != nil {
			return nil, err
		}
		if _, duplicate := seen[requirement]; duplicate {
			continue
		}
		seen[requirement] = struct{}{}
		unique = append(unique, requirement)
	}
	return unique, nil
}

func validateRequirement(requirement Requirement) error {
	if err := ValidatePermission(requirement.Permission); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequirement, err)
	}
	if err := ValidateResourceType(requirement.Resource.Type); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequirement, err)
	}
	if requirement.Resource.ID == uuid.Nil {
		return fmt.Errorf("%w: resource ID is required", ErrInvalidRequirement)
	}
	return nil
}

func validateGrant(grant Grant) error {
	return validateRequirement(Requirement(grant))
}

func recordAuthorizationCheck(ctx context.Context, requirements, missing int, outcome string) {
	trace.SpanFromContext(ctx).AddEvent("engine.authorization.check", trace.WithAttributes(
		attribute.Int("engine.authorization.requirements", requirements),
		attribute.Int("engine.authorization.missing", missing),
		attribute.String("engine.authorization.outcome", outcome),
	))
}
