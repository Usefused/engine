package api

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

var errMissingControlActor = errors.New("authenticated control actor is required")

// controlActorAccount reads the identity resolved once by the Engine's
// top-level control middleware. Runtime SDK/MCP credentials never create this
// Actor and there is deliberately no handler-local credential fallback.
func controlActorAccount(ctx context.Context) (uuid.UUID, error) {
	if actor, ok := accesscontrol.ActorFromContext(ctx); ok {
		return actor.AccountID, nil
	}
	return uuid.Nil, errMissingControlActor
}

func verifyWorkspaceActor(ctx context.Context, accountID uuid.UUID) error {
	actor, ok := accesscontrol.ActorFromContext(ctx)
	if !ok {
		return errMissingControlActor
	}
	// Authentication already joined the singleton workspace and active local
	// subject. Re-querying ownership would defeat the permission cache and
	// reintroduce an identity path outside local access control.
	if actor.AccountID != accountID || actor.WorkspaceID == uuid.Nil {
		return errors.New("authenticated actor workspace mismatch")
	}
	return nil
}
