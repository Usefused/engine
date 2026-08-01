package accesscontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

var (
	ErrAuthenticationRequired = errors.New("authentication required")
	ErrPolicyDenied           = errors.New("authorization policy denied request")
)

type denialResponse struct {
	Error   string               `json:"error"`
	Missing []missingRequirement `json:"missing,omitempty"`
}

type missingRequirement struct {
	Permission   Permission   `json:"permission"`
	ResourceType ResourceType `json:"resource_type"`
	ResourceID   string       `json:"resource_id"`
	DisplayName  string       `json:"display_name,omitempty"`
}

func RequireAll(authorizer Authorizer, requirements ...Requirement) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			err := AuthorizeAll(r.Context(), authorizer, requirements...)
			w.Header().Add("Server-Timing", fmt.Sprintf("engine_authz;dur=%.3f", float64(time.Since(started).Microseconds())/1000))
			if err != nil {
				WriteAuthorizationError(w, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func AuthorizeAll(ctx context.Context, authorizer Authorizer, requirements ...Requirement) error {
	actor, ok := ActorFromContext(ctx)
	if !ok {
		return ErrAuthenticationRequired
	}
	return authorizer.CheckAll(ctx, actor, requirements...)
}

func WriteAuthorizationError(w http.ResponseWriter, err error) {
	status, response := authorizationErrorResponse(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

func authorizationErrorResponse(err error) (int, denialResponse) {
	if errors.Is(err, ErrAuthenticationRequired) {
		return http.StatusUnauthorized, denialResponse{Error: "authentication_required"}
	}
	if errors.Is(err, ErrPolicyDenied) {
		return http.StatusForbidden, denialResponse{Error: "permission_denied"}
	}

	var denied *PermissionDeniedError
	if errors.As(err, &denied) {
		missing := make([]missingRequirement, 0, len(denied.Missing))
		for _, requirement := range denied.Missing {
			missing = append(missing, missingRequirement{
				Permission:   requirement.Permission,
				ResourceType: requirement.Resource.Type,
				ResourceID:   requirement.Resource.ID.String(),
				DisplayName:  denied.DisplayNames[requirement.Resource],
			})
		}
		return http.StatusForbidden, denialResponse{Error: "permission_denied", Missing: missing}
	}
	// Invalid server-owned permission declarations fail closed and are not
	// misreported as a caller authorization failure.
	return http.StatusInternalServerError, denialResponse{Error: "authorization_policy_invalid"}
}
