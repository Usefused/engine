package accesscontrol

import (
	"context"

	"github.com/google/uuid"
)

type SubjectKind string

const (
	SubjectBootstrap      SubjectKind = "bootstrap"
	SubjectUser           SubjectKind = "user"
	SubjectServiceAccount SubjectKind = "service_account"
	SubjectArtifact       SubjectKind = "artifact"
)

type Actor struct {
	AccountID     uuid.UUID
	WorkspaceID   uuid.UUID
	SubjectID     uuid.UUID
	CredentialID  uuid.UUID
	Kind          SubjectKind
	Authorization AuthorizationSnapshot
}

type actorContextKey struct{}

func ContextWithActor(ctx context.Context, actor Actor) context.Context {
	return context.WithValue(ctx, actorContextKey{}, actor)
}

func ActorFromContext(ctx context.Context) (Actor, bool) {
	actor, ok := ctx.Value(actorContextKey{}).(Actor)
	return actor, ok
}
