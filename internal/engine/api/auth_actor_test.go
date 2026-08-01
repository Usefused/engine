package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

type actorFastPathStore struct {
	store.Store
}

func (actorFastPathStore) GetAccountByAPIKey(context.Context, string) (uuid.UUID, error) {
	panic("cached Actor must avoid credential lookup")
}

func (actorFastPathStore) VerifyWorkspaceOwner(context.Context, uuid.UUID) error {
	panic("RBAC actor must avoid legacy ownership lookup")
}

func TestResolveWorkspaceActorUsesCachedRBACActorWithoutStoreQueries(t *testing.T) {
	actor := accesscontrol.Actor{
		AccountID:   uuid.New(),
		WorkspaceID: uuid.New(),
		SubjectID:   uuid.New(),
		Kind:        accesscontrol.SubjectUser,
	}
	request := httptest.NewRequest(http.MethodGet, "/workspace/config", nil)
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))

	accountID, err := resolveWorkspaceActor(request.Context())
	if err != nil || accountID != actor.AccountID {
		t.Fatalf("account/error = %s/%v", accountID, err)
	}
	if err := verifyWorkspaceActor(request.Context(), actor.AccountID); err != nil {
		t.Fatalf("verifyWorkspaceActor: %v", err)
	}
}

func TestVerifyWorkspaceActorFailsClosedWithoutActor(t *testing.T) {
	if err := verifyWorkspaceActor(context.Background(), uuid.New()); !errors.Is(err, errMissingControlActor) {
		t.Fatalf("expected errMissingControlActor, got %v", err)
	}
}
