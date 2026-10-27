package api

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// validateBridgeReferences requires portable aliases and explicit immutable local-provider identities.
func validateBridgeReferences(doc sdkConfigDocument) error {
	// Bounds apply independently of ordinary service selections.
	if len(doc.Bridges) > 100 {
		return errors.New("at most 100 Bridge capabilities may be attached")
	}
	for alias, ref := range doc.Bridges {
		// Python sync companions must not shadow another selected operation.
		if _, exists := doc.Bridges[alias+"_sync"]; exists {
			return errors.New("conflicting Bridge aliases")
		}
		// Each reviewed alias has independent immutable routing authority.
		if err := validateBridgeReference(alias, ref); err != nil {
			return err
		}

	}
	return nil
}

// resolveBridgeAttachments copies only stored manifest contracts after explicit workspace authority.
func resolveBridgeAttachments(ctx context.Context, s store.Store, account uuid.UUID, doc sdkConfigDocument) ([]models.BridgeBinding, error) {
	// Existing apps have no dependency on Bridge storage or permissions.
	if len(doc.Bridges) == 0 {
		return nil, nil
	}
	actor, ok := accesscontrol.ActorFromContext(ctx)
	if !ok || actor.AccountID != account {
		return nil, errors.New("Bridge authorization unavailable")
	}
	err := (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, actor, accesscontrol.Requirement{Permission: accesscontrol.PermissionBridgeManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID}})
	// Delegating a local capability requires the same authority as sharing it.
	if err != nil {
		return nil, err
	}
	repository, ok := s.(store.BridgeBindingStore)
	if !ok {
		return nil, errors.New("Bridge binding storage unavailable")
	}
	bindings, err := repository.ResolveBridgeBindings(ctx, account, doc.Bridges)
	// Resolution cannot be replaced by a caller-provided contract.
	if err != nil {
		return nil, err
	}
	// A grant on one workspace cannot delegate a pairing owned by a sibling workspace.
	if err := validateBridgeWorkspace(actor.WorkspaceID, bindings); err != nil {
		return nil, err
	}
	return validateBridgeAdapterKinds(doc.Kind, bindings)
}

// verifyBridgeAttachments prevents a delayed apply from changing its reviewed schema or destination.
func verifyBridgeAttachments(ctx context.Context, s store.Store, doc sdkConfigDocument, payload appResolvedPayload) error {
	// Fast-path empty scopes without demanding a control actor in legacy callers.
	if len(doc.Bridges) == 0 && len(payload.Bridges) == 0 {
		return nil
	}
	actor, ok := accesscontrol.ActorFromContext(ctx)
	if !ok {
		return errors.New("Bridge authorization unavailable")
	}
	fresh, err := resolveBridgeAttachments(ctx, s, actor.AccountID, doc)
	if err != nil {
		return err
	}
	a, _ := json.Marshal(fresh)
	b, _ := json.Marshal(payload.Bridges)
	var left, right any
	_ = json.Unmarshal(a, &left)
	_ = json.Unmarshal(b, &right)
	// Raw schemas compare structurally after serialization while exact hashes remain pinned.
	if !reflect.DeepEqual(left, right) {
		return errors.New("Bridge attachments changed; plan again")
	}
	return nil
}

// validateBridgeReference prevents generated names and routing fields from widening one selected capability.
func validateBridgeReference(alias string, ref models.BridgeReference) error {
	// Reuse the portable alias grammar so generated languages expose identical names.
	if !unifiedAppAlias.MatchString(alias) || strings.Contains(" false true null none self cls async await and as assert break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield constructor then execute __proto__ ", " "+alias+" ") {
		return errors.New("invalid Bridge alias")
	}
	if ref.Grouped() {
		return models.ValidateBridgeGroup(ref)
	}
	if _, err := uuid.Parse(ref.PairingID); err != nil {
		return errors.New("Bridge pairing_id must be a UUID")
	}
	if len(ref.ManifestHash) != 71 || !strings.HasPrefix(ref.ManifestHash, "sha256:") || ref.ProfileID == "" || ref.ConnectionID == "" || ref.OperationID == "" {
		return errors.New("Bridge requires exact manifest and operation identities")
	}
	return nil
}

// validateBridgeAdapterKinds refuses silent event selections in adapters without durable receiver support.
func validateBridgeAdapterKinds(kind string, bindings []models.BridgeBinding) ([]models.BridgeBinding, error) {
	for _, binding := range bindings {
		// The first event adapter is the existing SDK receiver; MCP and authored apps expose unary calls.
		if binding.Kind == "event" && kind != "sdk" {
			return nil, errors.New("Bridge event subscriptions require kind sdk")
		}
	}
	return bindings, nil
}

// validateBridgeWorkspace keeps pairing ownership aligned with the control actor's exact workspace.
func validateBridgeWorkspace(workspace uuid.UUID, bindings []models.BridgeBinding) error {
	for _, binding := range bindings {
		// Account membership alone never authorizes a different workspace's local devices.
		if binding.WorkspaceID != workspace.String() {
			return errors.New("Bridge workspace authorization unavailable")
		}
	}
	return nil
}
