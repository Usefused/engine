package accesscontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/otel/trace"
)

var (
	ErrAuthenticationRequired = errors.New("authentication required")
	ErrPolicyDenied           = errors.New("authorization policy denied request")
)

type denialResponse struct {
	Error denialError `json:"error"`
}

type denialError struct {
	Code        string         `json:"code"`
	Message     string         `json:"message"`
	Category    string         `json:"category"`
	Retryable   bool           `json:"retryable"`
	Details     *denialDetails `json:"details,omitempty"`
	Phase       string         `json:"phase,omitempty"`
	OperationID string         `json:"operation_id,omitempty"`
	RequestID   string         `json:"request_id,omitempty"`
	CommitState string         `json:"commit_state,omitempty"`
	TraceID     string         `json:"trace_id,omitempty"`
}

type denialDetails struct {
	Missing []missingRequirement `json:"missing,omitempty"`
}

type missingRequirement struct {
	Permission   Permission   `json:"permission"`
	ResourceType ResourceType `json:"resource_type"`
	ResourceID   string       `json:"resource_id"`
	DisplayName  string       `json:"display_name,omitempty"`
}

// RequireAll enforces the complete route policy and writes the shared bounded
// Engine error envelope when authorization fails.
func RequireAll(authorizer Authorizer, requirements ...Requirement) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			err := AuthorizeAll(r.Context(), authorizer, requirements...)
			w.Header().Add("Server-Timing", fmt.Sprintf("engine_authz;dur=%.3f", float64(time.Since(started).Microseconds())/1000))
			// Denials terminate before the protected handler and retain request correlation.
			if err != nil {
				// Unsafe methods cannot commit after authorization stops the handler.
				if authorizationMutationMethod(r.Method) {
					WriteAuthorizationMutationError(w, err, "authorization", "", "not_committed", r.Context())
				} else {
					WriteAuthorizationError(w, err, r.Context())
				}
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

// WriteAuthorizationError emits the shared bounded authorization response.
func WriteAuthorizationError(w http.ResponseWriter, err error, contexts ...context.Context) {
	status, response := authorizationErrorResponse(err)
	writeAuthorizationResponse(w, status, response, contexts)
}

// WriteAuthorizationMutationError preserves authorization diagnostics while
// adding only caller-proven mutation state to the shared error envelope.
func WriteAuthorizationMutationError(w http.ResponseWriter, err error, phase, operationID, commitState string, contexts ...context.Context) {
	status, response := authorizationErrorResponse(err)
	response.Error.Phase = phase
	response.Error.OperationID = operationID
	response.Error.CommitState = commitState
	writeAuthorizationResponse(w, status, response, contexts)
}

// writeAuthorizationResponse centralizes correlation and response hardening for
// ordinary denials and pre-commit mutation denials.
func writeAuthorizationResponse(w http.ResponseWriter, status int, response denialResponse, contexts []context.Context) {
	// The nearest request context owns both correlation identifiers.
	if len(contexts) > 0 && contexts[0] != nil {
		response.Error.RequestID = chimiddleware.GetReqID(contexts[0])
		spanContext := trace.SpanContextFromContext(contexts[0])
		// Invalid span contexts are omitted instead of exposing an invented trace ID.
		if spanContext.IsValid() {
			response.Error.TraceID = spanContext.TraceID().String()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Correlation headers mirror the exact bounded values carried in the envelope.
	if response.Error.RequestID != "" {
		w.Header().Set("X-Request-ID", response.Error.RequestID)
	}
	if response.Error.TraceID != "" {
		w.Header().Set("X-Trace-ID", response.Error.TraceID)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

// authorizationMutationMethod identifies methods whose protected handler may
// change state; authorization denial proves those writes never began.
func authorizationMutationMethod(method string) bool {
	// Read-only and discovery methods do not need mutation recovery metadata.
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

// AuthorizationErrorStatusCode exposes the existing stable classification so
// route-specific safe envelopes can reuse it without duplicating RBAC policy.
func AuthorizationErrorStatusCode(err error) (int, string) {
	status, response := authorizationErrorResponse(err)
	return status, response.Error.Code
}

// authorizationErrorResponse classifies authorization failures without
// exposing policy internals or caller-controlled credential material.
func authorizationErrorResponse(err error) (int, denialResponse) {
	// Missing authentication is distinct from an authenticated policy denial.
	if errors.Is(err, ErrAuthenticationRequired) {
		return http.StatusUnauthorized, newDenialResponse("authentication_required", "Authentication is required for this request.", "authentication", false, nil)
	}
	// A stable policy denial tells callers that retrying unchanged will not help.
	if errors.Is(err, ErrPolicyDenied) {
		return http.StatusForbidden, newDenialResponse("permission_denied", "The authenticated identity is not permitted to perform this request.", "authorization", false, nil)
	}

	var denied *PermissionDeniedError
	// Reviewed missing requirements are safe typed details for interactive remediation.
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
		return http.StatusForbidden, newDenialResponse("permission_denied", "The authenticated identity is missing required permissions.", "authorization", false, missing)
	}
	// Invalid server-owned permission declarations fail closed and are not
	// misreported as a caller authorization failure.
	return http.StatusInternalServerError, newDenialResponse("authorization_policy_invalid", "The Engine could not validate the authorization policy.", "internal", true, nil)
}

// newDenialResponse builds the common envelope while omitting an empty details object.
func newDenialResponse(code, message, category string, retryable bool, missing []missingRequirement) denialResponse {
	response := denialResponse{Error: denialError{Code: code, Message: message, Category: category, Retryable: retryable}}
	// Details are emitted only when the authorizer supplied reviewed requirements.
	if len(missing) > 0 {
		response.Error.Details = &denialDetails{Missing: missing}
	}
	return response
}
