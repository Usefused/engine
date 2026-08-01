package accesscontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/google/uuid"
)

type RequiredPermission struct {
	Permission   Permission   `json:"permission"`
	ResourceType ResourceType `json:"resource_type"`
	ResourceID   uuid.UUID    `json:"resource_id"`
	DisplayName  string       `json:"display_name,omitempty"`
}

type requiredPermissionsContextKey struct{}
type requiredPermissionsCaptureContextKey struct{}

// RequiredPermissionsCapture lets an outer HTTP middleware observe the exact
// requirements resolved by a downstream authorization layer. Context values
// themselves are immutable, so replacing a downstream request context cannot
// communicate the resolved plan back to an already-running outer middleware.
type RequiredPermissionsCapture struct {
	mu           sync.RWMutex
	requirements []Requirement
	missing      []Requirement
	set          bool
	missingSet   bool
}

func CaptureMissingPermissions(ctx context.Context, requirements []Requirement) bool {
	capture, ok := ctx.Value(requiredPermissionsCaptureContextKey{}).(*RequiredPermissionsCapture)
	if !ok || capture == nil {
		return false
	}
	capture.mu.Lock()
	capture.missing = append([]Requirement(nil), requirements...)
	capture.missingSet = true
	capture.mu.Unlock()
	return true
}

func ContextWithRequiredPermissions(ctx context.Context, requirements []Requirement) context.Context {
	return context.WithValue(ctx, requiredPermissionsContextKey{}, append([]Requirement(nil), requirements...))
}

func ContextWithRequiredPermissionsCapture(ctx context.Context) (context.Context, *RequiredPermissionsCapture) {
	capture := &RequiredPermissionsCapture{}
	return context.WithValue(ctx, requiredPermissionsCaptureContextKey{}, capture), capture
}

func CaptureRequiredPermissions(ctx context.Context, requirements []Requirement) bool {
	capture, ok := ctx.Value(requiredPermissionsCaptureContextKey{}).(*RequiredPermissionsCapture)
	if !ok || capture == nil {
		return false
	}
	capture.mu.Lock()
	capture.requirements = append([]Requirement(nil), requirements...)
	capture.set = true
	capture.mu.Unlock()
	return true
}

func (capture *RequiredPermissionsCapture) RequiredPermissions() ([]Requirement, bool) {
	if capture == nil {
		return nil, false
	}
	capture.mu.RLock()
	requirements := append([]Requirement(nil), capture.requirements...)
	set := capture.set
	capture.mu.RUnlock()
	return requirements, set
}

func RequiredPermissionsFromContext(ctx context.Context) ([]Requirement, bool) {
	if requirements, ok := ctx.Value(requiredPermissionsContextKey{}).([]Requirement); ok {
		return append([]Requirement(nil), requirements...), true
	}
	capture, _ := ctx.Value(requiredPermissionsCaptureContextKey{}).(*RequiredPermissionsCapture)
	return capture.RequiredPermissions()
}

func MissingPermissionsFromContext(ctx context.Context) ([]Requirement, bool) {
	capture, _ := ctx.Value(requiredPermissionsCaptureContextKey{}).(*RequiredPermissionsCapture)
	if capture == nil {
		return nil, false
	}
	capture.mu.RLock()
	missing := append([]Requirement(nil), capture.missing...)
	set := capture.missingSet
	capture.mu.RUnlock()
	return missing, set
}

func MarshalRequiredPermissions(requirements []Requirement) (json.RawMessage, error) {
	return MarshalRequiredPermissionsWithDisplayNames(requirements, nil)
}

func MarshalRequiredPermissionsWithDisplayNames(requirements []Requirement, displayNames map[ResourceRef]string) (json.RawMessage, error) {
	canonical, err := canonicalRequiredPermissions(requirements, displayNames)
	if err != nil {
		return nil, err
	}
	return json.Marshal(canonical)
}

func UnmarshalRequiredPermissions(raw json.RawMessage) ([]Requirement, error) {
	_, requirements, err := NormalizeRequiredPermissions(raw)
	return requirements, err
}

func NormalizeRequiredPermissions(raw json.RawMessage) (json.RawMessage, []Requirement, error) {
	var serialized []RequiredPermission
	if err := json.Unmarshal(raw, &serialized); err != nil {
		return nil, nil, err
	}
	requirements := make([]Requirement, 0, len(serialized))
	displayNames := make(map[ResourceRef]string, len(serialized))
	for _, item := range serialized {
		requirement := Requirement{
			Permission: item.Permission,
			Resource:   ResourceRef{Type: item.ResourceType, ID: item.ResourceID},
		}
		if err := validateRequirement(requirement); err != nil {
			return nil, nil, err
		}
		if err := validateDisplayName(item.DisplayName); err != nil {
			return nil, nil, err
		}
		requirements = append(requirements, requirement)
		if item.DisplayName != "" {
			displayNames[requirement.Resource] = item.DisplayName
		}
	}
	canonical, err := MarshalRequiredPermissionsWithDisplayNames(requirements, displayNames)
	return canonical, requirements, err
}

func canonicalRequiredPermissions(requirements []Requirement, displayNames map[ResourceRef]string) ([]RequiredPermission, error) {
	unique, err := uniqueRequirements(requirements)
	if err != nil {
		return nil, err
	}
	serialized := make([]RequiredPermission, 0, len(unique))
	for _, requirement := range unique {
		displayName := displayNames[requirement.Resource]
		if err := validateDisplayName(displayName); err != nil {
			return nil, err
		}
		serialized = append(serialized, RequiredPermission{
			Permission: requirement.Permission, ResourceType: requirement.Resource.Type,
			ResourceID: requirement.Resource.ID, DisplayName: displayName,
		})
	}
	sort.Slice(serialized, func(i, j int) bool {
		if serialized[i].Permission != serialized[j].Permission {
			return serialized[i].Permission < serialized[j].Permission
		}
		if serialized[i].ResourceType != serialized[j].ResourceType {
			return serialized[i].ResourceType < serialized[j].ResourceType
		}
		return serialized[i].ResourceID.String() < serialized[j].ResourceID.String()
	})
	return serialized, nil
}

func validateDisplayName(name string) error {
	if name == "" {
		return nil
	}
	if strings.TrimSpace(name) != name || len(name) > 128 {
		return fmt.Errorf("invalid required permission display name")
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			return fmt.Errorf("invalid required permission display name")
		}
	}
	return nil
}
