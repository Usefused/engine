package store

// AppKind identifies which runtime adapter an app family uses. SDK and MCP
// share lifecycle and authorization; the kind controls only adapter-specific
// behavior such as package generation.
type AppKind string

// AppDeliveryMode distinguishes generated SDK packages from package-free REST APIs within the shared SDK adapter kind.
type AppDeliveryMode string

const (
	AppKindSDK         AppKind         = "sdk"
	AppKindMCP         AppKind         = "mcp"
	AppDeliveryModeSDK AppDeliveryMode = "sdk"
	AppDeliveryModeAPI AppDeliveryMode = "api"
)

func (kind AppKind) Valid() bool {
	return kind == AppKindSDK || kind == AppKindMCP
}

func (kind AppKind) String() string {
	return string(kind)
}

// AppStatus is the persisted state of an exact app version. Deactivation is
// deliberately absent: a deactivated version is deleted and represented by a
// tombstone, not by a runnable app row with another status.
type AppStatus string

const (
	AppStatusBuilding   AppStatus = "building"
	AppStatusActive     AppStatus = "active"
	AppStatusDeprecated AppStatus = "deprecated"
)

func (status AppStatus) Valid() bool {
	switch status {
	case AppStatusBuilding, AppStatusActive, AppStatusDeprecated:
		return true
	default:
		return false
	}
}

func (status AppStatus) Runnable() bool {
	return status == AppStatusActive || status == AppStatusDeprecated
}

func (status AppStatus) String() string {
	return string(status)
}

// AppFamilyQuotaUsage is the single-row entitlement projection for one
// account, adapter, and target family identity.
type AppFamilyQuotaUsage struct {
	CurrentInvokable int
	TargetInvokable  bool
}

// HasSameBinding compares the immutable language and owner choices attached to
// a logical family. Display-name changes are intentionally excluded because
// they do not change execution authority or generated-package identity.
func (family AppFamily) HasSameBinding(other AppFamily) bool {
	// An empty requested mode is a pre-publication reservation, while every concrete apply must match the durable family mode.
	deliveryMatches := other.DeliveryMode == "" || family.DeliveryMode == other.DeliveryMode
	return deliveryMatches && family.TargetLanguage == other.TargetLanguage &&
		family.OwnerSubjectID == other.OwnerSubjectID &&
		family.OwnerTeamID == other.OwnerTeamID
}
