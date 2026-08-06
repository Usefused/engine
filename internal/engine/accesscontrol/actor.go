package accesscontrol

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type SubjectKind string

const (
	SubjectBootstrap      SubjectKind = "bootstrap"
	SubjectUser           SubjectKind = "user"
	SubjectServiceAccount SubjectKind = "service_account"
	SubjectApp            SubjectKind = "app"
)

type Actor struct {
	AccountID            uuid.UUID
	WorkspaceID          uuid.UUID
	SubjectID            uuid.UUID
	DisplayName          string
	Email                string
	CredentialID         uuid.UUID
	CredentialSource     string
	AuthenticationMethod string
	CredentialExpiresAt  *time.Time
	Kind                 SubjectKind
	Authorization        AuthorizationSnapshot
}

type actorContextKey struct{}

func ContextWithActor(ctx context.Context, actor Actor) context.Context {
	return context.WithValue(ctx, actorContextKey{}, actor)
}

func ActorFromContext(ctx context.Context) (Actor, bool) {
	actor, ok := ctx.Value(actorContextKey{}).(Actor)
	return actor, ok
}
