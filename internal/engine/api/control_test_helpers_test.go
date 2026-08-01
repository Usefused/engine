package api

import (
	"context"
	"net/http"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func newControlTestRouter(accountID uuid.UUID) chi.Router {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return withControlTestActor(accountID, next)
	})
	return router
}

func withControlTestActor(accountID uuid.UUID, next http.Handler) http.Handler {
	actor := controlTestOwnerActor(accountID)
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		ctx := accesscontrol.ContextWithActor(request.Context(), actor)
		next.ServeHTTP(w, request.WithContext(ContextWithAuthorizedPlanRevision(ctx, 1)))
	})
}

func controlTestRequest(request *http.Request, accountID uuid.UUID) *http.Request {
	return request.WithContext(controlTestContext(request.Context(), accountID))
}

func controlTestContext(ctx context.Context, accountID uuid.UUID) context.Context {
	ctx = accesscontrol.ContextWithActor(ctx, controlTestOwnerActor(accountID))
	return ContextWithAuthorizedPlanRevision(ctx, 1)
}

func controlTestOwnerActor(accountID uuid.UUID) accesscontrol.Actor {
	workspaceID := uuid.New()
	grants := make([]accesscontrol.Grant, 0, len(accesscontrol.AllPermissions()))
	for _, permission := range accesscontrol.AllPermissions() {
		grants = append(grants, accesscontrol.Grant{
			Permission: permission,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID},
		})
	}
	snapshot, err := accesscontrol.NewAuthorizationSnapshot(1, grants...)
	if err != nil {
		panic(err)
	}
	return accesscontrol.Actor{
		AccountID: accountID, WorkspaceID: workspaceID, SubjectID: uuid.New(), CredentialID: uuid.New(),
		Kind: accesscontrol.SubjectUser, Authorization: snapshot,
	}
}
