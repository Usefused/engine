package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"regexp"
	"strings"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

var unifiedAppAlias = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// validateAttachedAppServices accepts hosted capabilities alongside ordinary physical selections.
func validateAttachedAppServices(doc sdkConfigDocument) error {
	// Hosted apps cannot recursively delegate into other apps or create dependency cycles.
	if len(doc.UnifiedApps) > 0 && doc.Kind != "sdk" && doc.Kind != "mcp" {
		return errors.New("unified_apps is supported only by SDK and MCP configs")
	}
	if len(doc.UnifiedApps) > 16 {
		return errors.New("at most 16 Unified Apps may be attached")
	}
	for alias, ref := range doc.UnifiedApps {
		// Python exposes a synchronous companion, so aliases cannot shadow each other.
		if _, collision := doc.UnifiedApps[alias+"_sync"]; collision {
			return errors.New("Unified App aliases conflict with a synchronous companion method")
		}
		// Stable identifier aliases become callable API names in every delivery adapter.
		if (!unifiedAppAlias.MatchString(alias) || strings.Contains(" false true null none self cls async await and as assert break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield constructor then execute __proto__ ", " "+alias+" ")) || strings.TrimSpace(ref.Name) == "" || !validAppVersion(ref.Version) {
			return errors.New("unified_apps requires an identifier alias, app name, and exact SemVer version")
		}
	}
	// A reference-only consumer is valid; a completely empty app remains invalid.
	if len(doc.Services) == 0 && len(doc.UnifiedApps) > 0 {
		return nil
	}
	return validateAppServiceDocs(doc.Services)
}

// resolveUnifiedAppAttachments freezes exact source identity before a plan grants consumer execution authority.
func resolveUnifiedAppAttachments(ctx context.Context, s store.Store, accountID uuid.UUID, doc sdkConfigDocument) ([]models.UnifiedAppBinding, error) {
	// Existing configs keep their original planning and test-store requirements.
	if len(doc.UnifiedApps) == 0 {
		return nil, nil
	}
	repository, ok := s.(store.UnifiedAppAttachmentStore)
	if !ok {
		return nil, errors.New("Unified App attachment storage unavailable")
	}
	bindings, err := repository.ResolveUnifiedAppBindings(ctx, accountID, doc.UnifiedApps)
	// Identity failures never create partial attachments or fall back to latest.
	if err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "referenced Unified App version is unavailable"}
	}
	return bindings, nil
}

// attachmentPermissions adds typed use authority to the ordinary owner and actor preflight.
func attachmentPermissions(raw json.RawMessage, bindings []models.UnifiedAppBinding) (json.RawMessage, int, error) {
	requirements, err := accesscontrol.UnmarshalRequiredPermissions(raw)
	// Persisted permission metadata must remain authoritative for apply.
	if err != nil {
		return nil, 0, err
	}
	names := map[accesscontrol.ResourceRef]string{}
	var persisted []accesscontrol.RequiredPermission
	// Existing provider labels stay readable when hosted permissions are merged.
	if len(raw) > 0 && json.Unmarshal(raw, &persisted) != nil {
		return nil, 0, errors.New("invalid required permissions")
	}
	for _, requirement := range persisted {
		names[accesscontrol.ResourceRef{Type: requirement.ResourceType, ID: requirement.ResourceID}] = requirement.DisplayName
	}
	for _, binding := range bindings {
		ref := accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: binding.AppFamilyID}
		requirements = append(requirements, accesscontrol.Requirement{Permission: accesscontrol.PermissionAppUnifiedAppUse, Resource: ref})
		names[ref] = binding.Name
	}
	// Preserve existing display names when there is no hosted dependency.
	if len(bindings) == 0 {
		return raw, len(requirements), nil
	}
	return marshalPlanRequiredPermissions(deduplicateConfigRequirements(requirements), names, nil)
}

// verifyUnifiedAppAttachments rejects deletion or changed provenance between plan and apply.
func verifyUnifiedAppAttachments(ctx context.Context, s store.Store, doc sdkConfigDocument, payload appResolvedPayload) error {
	// Both sides must agree even when a stored payload has been damaged.
	if len(doc.UnifiedApps) == 0 && len(payload.UnifiedApps) == 0 {
		return nil
	}
	actor, ok := accesscontrol.ActorFromContext(ctx)
	if !ok {
		return errors.New("attachment authorization unavailable")
	}
	current, err := resolveUnifiedAppAttachments(ctx, s, actor.AccountID, doc)
	// JSON schema bytes compare canonically through the decoded descriptor structure below.
	if err != nil {
		return err
	}
	planned, _ := json.Marshal(payload.UnifiedApps)
	fresh, _ := json.Marshal(current)
	var a, b any
	if json.Unmarshal(planned, &a) != nil || json.Unmarshal(fresh, &b) != nil || !reflect.DeepEqual(a, b) {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "Unified App attachment changed; plan again"}
	}
	return nil
}
