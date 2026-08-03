package accesscontrol

import (
	"errors"
	"fmt"
	"strings"
)

const (
	RoleOwner           = "owner"
	RoleAdmin           = "admin"
	RoleBuilder         = "builder"
	RoleViewer          = "viewer"
	RoleServiceUser     = "service-user"
	RoleServiceManager  = "service-manager"
	RoleBucketUser      = "bucket-user"
	RoleBucketManager   = "bucket-manager"
	RoleArtifactReader  = "artifact-reader"
	RoleArtifactUser    = "artifact-user"
	RoleArtifactManager = "artifact-manager"
)

var ErrInvalidRoleDefinition = errors.New("invalid role definition")

type RoleDefinition struct {
	Slug        string
	DisplayName string
	ScopeType   ResourceType
	Permissions []Permission
}

var builtInRoles = []RoleDefinition{
	{
		Slug:        RoleOwner,
		DisplayName: "Owner",
		ScopeType:   ResourceWorkspace,
		Permissions: allPermissions,
	},
	{
		Slug:        RoleAdmin,
		DisplayName: "Admin",
		ScopeType:   ResourceWorkspace,
		Permissions: []Permission{
			PermissionWorkspaceRead,
			PermissionWorkspaceUpdate,
			PermissionServiceRead,
			PermissionServiceConsume,
			PermissionServiceManage,
			PermissionBucketRead,
			PermissionBucketValuesRead,
			PermissionBucketUse,
			PermissionBucketManage,
			PermissionCredentialsMetadataRead,
			PermissionCredentialsManage,
			PermissionConnectionRead,
			PermissionConnectionManage,
			PermissionArtifactRead,
			PermissionArtifactUse,
			PermissionArtifactCreate,
			PermissionArtifactManage,
			PermissionArtifactTokensManage,
			PermissionCatalogueRead,
			PermissionCatalogueImport,
			PermissionCatalogueManage,
			PermissionAccountRead,
			PermissionBillingRead,
			PermissionNotificationUpdate,
			PermissionAuditRead,
			PermissionAccessRead,
			PermissionAccessManage,
		},
	},
	{
		Slug:        RoleBuilder,
		DisplayName: "Builder",
		ScopeType:   ResourceWorkspace,
		Permissions: []Permission{
			PermissionWorkspaceRead,
			PermissionArtifactCreate,
			PermissionCatalogueRead,
			PermissionAccountRead,
			PermissionBillingRead,
			PermissionNotificationUpdate,
		},
	},
	{
		Slug:        RoleViewer,
		DisplayName: "Viewer",
		ScopeType:   ResourceWorkspace,
		Permissions: []Permission{
			PermissionWorkspaceRead,
			PermissionCatalogueRead,
			PermissionAccountRead,
			PermissionBillingRead,
		},
	},
	{
		Slug:        RoleServiceUser,
		DisplayName: "Service user",
		ScopeType:   ResourceService,
		Permissions: []Permission{
			PermissionServiceRead,
			PermissionServiceConsume,
		},
	},
	{
		Slug:        RoleServiceManager,
		DisplayName: "Service manager",
		ScopeType:   ResourceService,
		Permissions: []Permission{
			PermissionServiceRead,
			PermissionServiceConsume,
			PermissionServiceManage,
		},
	},
	{
		Slug:        RoleBucketUser,
		DisplayName: "Bucket user",
		ScopeType:   ResourceBucket,
		Permissions: []Permission{
			PermissionBucketRead,
			PermissionBucketUse,
		},
	},
	{
		Slug:        RoleBucketManager,
		DisplayName: "Bucket manager",
		ScopeType:   ResourceBucket,
		Permissions: []Permission{
			PermissionBucketRead,
			PermissionBucketValuesRead,
			PermissionBucketUse,
			PermissionBucketManage,
			PermissionCredentialsMetadataRead,
			PermissionCredentialsManage,
			PermissionConnectionRead,
			PermissionConnectionManage,
		},
	},
	{
		Slug:        RoleArtifactReader,
		DisplayName: "Artifact reader",
		ScopeType:   ResourceArtifact,
		Permissions: []Permission{PermissionArtifactRead},
	},
	{
		Slug:        RoleArtifactUser,
		DisplayName: "Artifact user",
		ScopeType:   ResourceArtifact,
		Permissions: []Permission{
			PermissionArtifactRead,
			PermissionArtifactUse,
		},
	},
	{
		Slug:        RoleArtifactManager,
		DisplayName: "Artifact manager",
		ScopeType:   ResourceArtifact,
		Permissions: []Permission{
			PermissionArtifactRead,
			PermissionArtifactUse,
			PermissionArtifactManage,
			PermissionArtifactTokensManage,
		},
	},
}

// BuiltInRoles returns deep copies because these definitions are later used as
// database seeds and must remain stable if a caller sorts or edits a slice.
func BuiltInRoles() []RoleDefinition {
	roles := make([]RoleDefinition, len(builtInRoles))
	for i, role := range builtInRoles {
		roles[i] = role
		roles[i].Permissions = append([]Permission(nil), role.Permissions...)
	}
	return roles
}

func ValidateRoleDefinition(role RoleDefinition) error {
	if role.Slug == "" || role.DisplayName == "" {
		return fmt.Errorf("%w: slug and display name are required", ErrInvalidRoleDefinition)
	}
	if err := ValidateResourceType(role.ScopeType); err != nil {
		return fmt.Errorf("%w: role %q: %v", ErrInvalidRoleDefinition, role.Slug, err)
	}
	if len(role.Permissions) == 0 {
		return fmt.Errorf("%w: role %q has no permissions", ErrInvalidRoleDefinition, role.Slug)
	}
	return validateRolePermissions(role)
}

func validateRolePermissions(role RoleDefinition) error {
	seen := make(map[Permission]struct{}, len(role.Permissions))
	for _, permission := range role.Permissions {
		if err := ValidatePermission(permission); err != nil {
			return fmt.Errorf("%w: role %q: %v", ErrInvalidRoleDefinition, role.Slug, err)
		}
		if _, duplicate := seen[permission]; duplicate {
			return fmt.Errorf("%w: role %q repeats %q", ErrInvalidRoleDefinition, role.Slug, permission)
		}
		if !permissionSupportsRoleScope(permission, role.ScopeType) {
			return fmt.Errorf("%w: permission %q cannot be granted by %s role %q", ErrInvalidRoleDefinition, permission, role.ScopeType, role.Slug)
		}
		seen[permission] = struct{}{}
	}
	return nil
}

func permissionSupportsRoleScope(permission Permission, scope ResourceType) bool {
	if scope == ResourceWorkspace {
		return true
	}
	prefix, _, _ := strings.Cut(string(permission), ".")
	switch prefix {
	case "service":
		return scope == ResourceService
	case "bucket", "credentials", "connection":
		return scope == ResourceBucket
	case "artifact":
		return scope == ResourceArtifact
	default:
		return false
	}
}
