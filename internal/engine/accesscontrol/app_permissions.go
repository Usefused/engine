package accesscontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// AppPermissionResolver binds internal shared-app policies to persisted identity.
// Generic actions are not credentials or grants; only their resolved permissions are.
type AppPermissionResolver interface {
	ResolveAppPermission(context.Context, uuid.UUID, uuid.UUID, Permission) (Permission, error)
	ResolveAppPermissionScope(context.Context, uuid.UUID, []Grant, Permission) (AuthorizedScope, error)
}

// IsAppAction identifies internal policy templates, which must never enter consent.
func IsAppAction(permission Permission) bool {
	// An explicit list prevents unknown app strings from becoming dispatch templates.
	switch permission {
	case PermissionAppRead, PermissionAppUse, PermissionAppCreate, PermissionAppManage, PermissionAppTokensManage:
		return true
	default:
		return false
	}
}

// ValidatePolicyPermission accepts internal templates only at trusted policy boundaries.
func ValidatePolicyPermission(permission Permission) error {
	// External grants and OAuth continue to use the stricter ValidatePermission.
	if IsAppAction(permission) {
		return nil
	}
	return ValidatePermission(permission)
}

// AppPermission selects one real permission; unsupported types/actions fail closed.
func AppPermission(appType string, action Permission) Permission {
	permission := Permission("app." + appType + "." + strings.TrimPrefix(string(action), "app."))
	// Never return an accepted scope for a misspelled type or unsupported operation.
	if ValidatePermission(permission) != nil {
		return Permission("invalid.app.permission")
	}
	return permission
}

// AppPermissions expands an internal role template into explicit, reviewable grants.
func AppPermissions(action Permission) []Permission {
	permissions := make([]Permission, 0, 4)
	for _, appType := range []string{"sdk", "mcp", "api", "webhook"} {
		permission := AppPermission(appType, action)
		// Webhooks have no execution-token or app-use lifecycle to authorize.
		if ValidatePermission(permission) == nil {
			permissions = append(permissions, permission)
		}
	}
	return permissions
}

// HasAnyAppPermission gates read-only mixed-type discovery without requiring a matching row to exist.
// It must not authorize mutations; those require one concrete type and resource.
func HasAnyAppPermission(actor Actor, action Permission, resource ResourceType) bool {
	for _, permission := range AppPermissions(action) {
		scope := actor.Authorization.scope(permission, resource)
		// A workspace read grant still permits an empty list before the first app exists.
		if scope.All || len(scope.IDs) > 0 {
			return true
		}
	}
	return false
}

// AppTypeFromConfig distinguishes package-free API creation on the shared SDK route.
func AppTypeFromConfig(configType string, raw []byte) (string, error) {
	var document struct {
		Kind     string `json:"kind"`
		Generate *bool  `json:"generate"`
	}
	// An unreadable or mismatched document cannot establish creation authority.
	if json.Unmarshal(raw, &document) != nil || (document.Kind != "" && document.Kind != configType) {
		return "", ErrPolicyDenied
	}
	// Route/plan type is authoritative; config keys and names never select permission type.
	switch configType {
	case "mcp", "webhook":
		return configType, nil
	case "sdk":
		// Only an explicit package-free configuration belongs to the API namespace.
		if document.Generate != nil && !*document.Generate {
			return "api", nil
		}
		return "sdk", nil
	default:
		return "", fmt.Errorf("%w: unsupported app configuration", ErrPolicyDenied)
	}
}

// ResolveAppRequirements removes shared-policy templates before an authorization check.
func ResolveAppRequirements(ctx context.Context, actor Actor, requirements []Requirement) ([]Requirement, error) {
	resolved := append([]Requirement(nil), requirements...)
	for i, requirement := range resolved {
		// Ordinary permissions already identify their exact action and resource.
		if !IsAppAction(requirement.Permission) {
			continue
		}
		// Creation must always name a type, even for an administrator.
		if requirement.Resource.Type != ResourceApp || requirement.Permission == PermissionAppCreate {
			return nil, ErrPolicyDenied
		}
		// Production actors resolve family identity through the authenticated store.
		if actor.AppPermissions != nil {
			permission, err := actor.AppPermissions.ResolveAppPermission(ctx, actor.AccountID, requirement.Resource.ID, requirement.Permission)
			// Missing or cross-account identity must not become a broad app grant.
			if err != nil {
				return nil, err
			}
			resolved[i].Permission = permission
			continue
		}
		// A trusted in-process actor with every concrete action needs no type lookup.
		// Requiring all types here cannot widen a narrow OAuth grant.
		for _, permission := range AppPermissions(requirement.Permission) {
			concrete := Requirement{Permission: permission, Resource: requirement.Resource}
			// One missing concrete grant makes the unresolved identity unsafe.
			if !actor.Authorization.allows(concrete) {
				return nil, ErrPolicyDenied
			}
		}
		resolved[i].Permission = AppPermissions(requirement.Permission)[0]
	}
	return resolved, nil
}

// appScope resolves mixed-app discovery without widening a type-specific workspace grant.
func appScope(ctx context.Context, actor Actor, action Permission, resource ResourceType) (AuthorizedScope, error) {
	permissions := AppPermissions(action)
	all := true
	for _, permission := range permissions {
		scope := actor.Authorization.scope(permission, resource)
		// Read-only owner/team selectors need any explicit creation authority.
		if action == PermissionAppCreate && resource == ResourceWorkspace && scope.All {
			return AuthorizedScope{All: true}, nil
		}
		all = all && scope.All
	}
	// Uniform workspace grants need no database expansion; narrow grants do.
	if all {
		return AuthorizedScope{All: true}, nil
	}
	// No workspace creation scope or unavailable identity resolver must fail closed.
	if resource != ResourceApp {
		return AuthorizedScope{}, nil
	}
	// Without a store, only IDs authorized under every type are safe to expose.
	if actor.AppPermissions == nil {
		candidates := make(map[uuid.UUID]bool)
		for _, permission := range permissions {
			for _, id := range actor.Authorization.scope(permission, resource).IDs {
				candidates[id] = true
			}
		}
		scope := AuthorizedScope{}
		for id := range candidates {
			allowed := true
			for _, permission := range permissions {
				allowed = allowed && actor.Authorization.allows(Requirement{Permission: permission, Resource: ResourceRef{Type: resource, ID: id}})
			}
			// Keep only the intersection, never the union, without trusted type metadata.
			if allowed {
				scope.IDs = append(scope.IDs, id)
			}
		}
		return scope, nil
	}
	return actor.AppPermissions.ResolveAppPermissionScope(ctx, actor.AccountID, actor.Authorization.EffectiveGrants(actor.WorkspaceID), action)
}
