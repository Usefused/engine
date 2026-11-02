package api

import (
	"context"
	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/canonical"
	"github.com/google/uuid"
)

// deploymentPlanAuthorization separates an existing family's authority from a new version's absent config state.
func deploymentPlanAuthorization(ctx context.Context, s store.Store, call sdkPlanCall, current *store.ConfigState, appID uuid.UUID) (*store.ConfigState, uuid.UUID, error) {
	// Ordinary planners retain their existing state-driven permission behavior.
	if call.authorizationAppID == uuid.Nil {
		return current, appID, nil
	}
	anchor, err := s.GetApp(ctx, call.authorizationAppID)
	// The internal reference is accepted only for a concrete app owned by this account.
	if err != nil || anchor == nil || anchor.AccountID != call.accountID || anchor.AppFamilyID == uuid.Nil {
		return nil, uuid.Nil, accesscontrol.ErrPolicyDenied
	}
	name, _, err := canonical.AppName(call.document.Name)
	// Caller-visible names must resolve back to the exact original family, not a replacement using its label.
	if err != nil {
		return nil, uuid.Nil, accesscontrol.ErrPolicyDenied
	}
	family, err := s.GetAppFamilyByIdentity(ctx, call.accountID, appKindFromDocument(call.document).String(), name)
	// Deployment authorization cannot cross app type, packaging mode or immutable family identity.
	if err != nil || family == nil || family.AppFamilyID != anchor.AppFamilyID || family.DeliveryMode != sdkConfigDeliveryMode(call.document) || family.ArchivedAt != nil {
		return nil, uuid.Nil, accesscontrol.ErrPolicyDenied
	}
	// An existing target version must belong to the same family as the approved anchor.
	if appID != uuid.Nil {
		target, err := s.GetApp(ctx, appID)
		if err != nil || target == nil || target.AppFamilyID != anchor.AppFamilyID {
			return nil, uuid.Nil, accesscontrol.ErrPolicyDenied
		}
	}
	state := &store.ConfigState{LatestResourceID: &anchor.AppID}
	// Ownership is copied from the actual family; a managing subject cannot claim the release personally.
	if family.OwnerSubjectID != uuid.Nil {
		state.OwnerSubjectID = &family.OwnerSubjectID
	}
	if family.OwnerTeamID != uuid.Nil {
		state.OwnerTeamID = &family.OwnerTeamID
	}
	return state, anchor.AppID, nil
}
