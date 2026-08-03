package accesscontrol

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

type Permission string

const (
	PermissionWorkspaceRead           Permission = "workspace.read"
	PermissionWorkspaceUpdate         Permission = "workspace.update"
	PermissionServiceRead             Permission = "service.read"
	PermissionServiceConsume          Permission = "service.consume"
	PermissionServiceManage           Permission = "service.manage"
	PermissionBucketRead              Permission = "bucket.read"
	PermissionBucketValuesRead        Permission = "bucket.values.read"
	PermissionBucketUse               Permission = "bucket.use"
	PermissionBucketManage            Permission = "bucket.manage"
	PermissionCredentialsMetadataRead Permission = "credentials.metadata.read"
	PermissionCredentialsManage       Permission = "credentials.manage"
	PermissionConnectionRead          Permission = "connection.read"
	PermissionConnectionManage        Permission = "connection.manage"
	PermissionArtifactRead            Permission = "artifact.read"
	PermissionArtifactUse             Permission = "artifact.use"
	PermissionArtifactCreate          Permission = "artifact.create"
	PermissionArtifactManage          Permission = "artifact.manage"
	PermissionArtifactTokensManage    Permission = "artifact.tokens.manage"
	PermissionCatalogueRead           Permission = "catalogue.read"
	PermissionCatalogueImport         Permission = "catalogue.import"
	PermissionCatalogueManage         Permission = "catalogue.manage"
	PermissionAccountRead             Permission = "account.read"
	PermissionAccountManage           Permission = "account.manage"
	PermissionBillingRead             Permission = "billing.read"
	PermissionBillingManage           Permission = "billing.manage"
	PermissionNotificationUpdate      Permission = "notification.update"
	PermissionAuditRead               Permission = "audit.read"
	PermissionAccessRead              Permission = "access.read"
	PermissionAccessManage            Permission = "access.manage"
)

var (
	ErrInvalidPermission = errors.New("invalid permission")

	allPermissions = []Permission{
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
		PermissionAccountManage,
		PermissionBillingRead,
		PermissionBillingManage,
		PermissionNotificationUpdate,
		PermissionAuditRead,
		PermissionAccessRead,
		PermissionAccessManage,
	}
	permissionSet = newPermissionSet(allPermissions)
)

type ResourceType string

const (
	ResourceWorkspace ResourceType = "workspace"
	ResourceService   ResourceType = "service"
	ResourceBucket    ResourceType = "bucket"
	ResourceArtifact  ResourceType = "artifact"
)

var validResourceTypes = map[ResourceType]struct{}{
	ResourceWorkspace: {},
	ResourceService:   {},
	ResourceBucket:    {},
	ResourceArtifact:  {},
}

type ResourceRef struct {
	Type ResourceType
	ID   uuid.UUID
}

type Requirement struct {
	Permission Permission
	Resource   ResourceRef
}

// AllPermissions returns a copy so callers cannot mutate the process-wide
// catalogue used to reject unknown database role permissions.
func AllPermissions() []Permission {
	return append([]Permission(nil), allPermissions...)
}

func ValidatePermission(permission Permission) error {
	if _, ok := permissionSet[permission]; ok {
		return nil
	}
	return fmt.Errorf("%w: %q", ErrInvalidPermission, permission)
}

func ValidateResourceType(resourceType ResourceType) error {
	if _, ok := validResourceTypes[resourceType]; ok {
		return nil
	}
	return fmt.Errorf("invalid resource type: %q", resourceType)
}

func newPermissionSet(permissions []Permission) map[Permission]struct{} {
	set := make(map[Permission]struct{}, len(permissions))
	for _, permission := range permissions {
		set[permission] = struct{}{}
	}
	return set
}
