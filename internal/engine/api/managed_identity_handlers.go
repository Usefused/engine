package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/Usefused/engine/internal/engine/browserauth"
	"github.com/Usefused/engine/internal/engine/managedauth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const maxManagedLoginBodyBytes = 4 << 10

type ManagedLoginService interface {
	Start(context.Context) (managedauth.StartResult, error)
	Poll(context.Context, uuid.UUID, string) (store.ManagedSessionCredential, error)
}

type managedLoginPollRequest struct {
	TransactionID string `json:"transaction_id"`
	PollToken     string `json:"poll_token"`
}

func MountManagedIdentityRoutes(router chi.Router, service ManagedLoginService, cookies *browserauth.CookieManager) {
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(10, 100, time.Minute))).
		Post("/auth/managed/start", managedLoginStartHandler(service, cookies))
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(120, 1000, time.Minute))).
		Post("/auth/managed/poll", managedLoginPollHandler(service, cookies))
}

// managedLoginStartHandler creates a browser-bound managed-login transaction.
func managedLoginStartHandler(service ManagedLoginService, cookies *browserauth.CookieManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setManagedLoginResponseHeaders(w)
		// Missing dependencies make the managed-login capability unavailable.
		if service == nil || cookies == nil {
			writeManagedLoginError(w, http.StatusServiceUnavailable, "managed_login_unavailable", r.Context())
			return
		}
		// Browser transactions must originate from the configured Engine origin.
		if !cookies.ValidateSameOrigin(r) {
			writeManagedLoginError(w, http.StatusForbidden, "origin_denied", r.Context())
			return
		}
		// The start route deliberately accepts no caller-controlled fields.
		if !validEmptyManagedLoginBody(w, r) {
			writeManagedLoginError(w, http.StatusBadRequest, "invalid_request", r.Context())
			return
		}
		result, err := service.Start(r.Context())
		// Service failures are classified without exposing identity-provider prose.
		if err != nil {
			writeManagedLoginServiceError(w, r.Context(), err)
			return
		}
		cookies.SetLoginBinding(w, result.TransactionID.String(), result.PollToken, result.ExpiresAt)
		writeManagedLoginJSON(w, http.StatusCreated, map[string]string{
			"transaction_id": result.TransactionID.String(),
			"poll_token":     result.PollToken, "verification_url": result.VerificationURL,
			"expires_at": result.ExpiresAt.Format(time.RFC3339),
		})
	}
}

// managedLoginPollHandler validates the browser binding before polling managed identity state.
func managedLoginPollHandler(service ManagedLoginService, cookies *browserauth.CookieManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setManagedLoginResponseHeaders(w)
		// Missing dependencies make the managed-login capability unavailable.
		if service == nil || cookies == nil {
			writeManagedLoginError(w, http.StatusServiceUnavailable, "managed_login_unavailable", r.Context())
			return
		}
		// Cross-origin polling cannot consume a browser-bound capability.
		if !cookies.ValidateSameOrigin(r) {
			writeManagedLoginError(w, http.StatusForbidden, "origin_denied", r.Context())
			return
		}
		request, id, err := decodeManagedLoginPollRequest(w, r)
		// Malformed capabilities are rejected before any identity-provider call.
		if err != nil {
			writeManagedLoginError(w, http.StatusBadRequest, "invalid_request", r.Context())
			return
		}
		// The transaction ID and poll token must match the HttpOnly browser binding.
		if !cookies.ValidateLoginBinding(r, id.String(), request.PollToken) {
			writeManagedLoginError(w, http.StatusForbidden, "managed_login_binding_denied", r.Context())
			return
		}
		credential, err := service.Poll(r.Context(), id, request.PollToken)
		if errors.Is(err, store.ErrManagedLoginPending) {
			writeManagedLoginJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
			return
		}
		// Non-pending failures are mapped through the stable managed-login taxonomy.
		if err != nil {
			writeManagedLoginServiceError(w, r.Context(), err)
			return
		}
		cookies.SetSession(w, credential.RawKey, credential.ExpiresAt)
		cookies.ClearLoginBinding(w)
		writeManagedLoginJSON(w, http.StatusOK, map[string]string{
			"status": "authenticated", "expires_at": credential.ExpiresAt.Format(time.RFC3339),
		})
	}
}

func validEmptyManagedLoginBody(w http.ResponseWriter, r *http.Request) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxManagedLoginBodyBytes))
	decoder.DisallowUnknownFields()
	var body struct{}
	err := decoder.Decode(&body)
	if errors.Is(err, io.EOF) {
		return true
	}
	if err != nil {
		return false
	}
	return errors.Is(decoder.Decode(&struct{}{}), io.EOF)
}

func decodeManagedLoginPollRequest(w http.ResponseWriter, r *http.Request) (managedLoginPollRequest, uuid.UUID, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxManagedLoginBodyBytes))
	decoder.DisallowUnknownFields()
	var request managedLoginPollRequest
	if err := decoder.Decode(&request); err != nil {
		return managedLoginPollRequest{}, uuid.Nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return managedLoginPollRequest{}, uuid.Nil, errors.New("invalid request")
	}
	id, err := uuid.Parse(request.TransactionID)
	if err != nil || id == uuid.Nil || request.PollToken == "" || len(request.PollToken) > 128 {
		return managedLoginPollRequest{}, uuid.Nil, errors.New("invalid request")
	}
	return request, id, nil
}

// writeManagedLoginServiceError maps provider/store failures to stable safe codes.
func writeManagedLoginServiceError(w http.ResponseWriter, ctx context.Context, err error) {
	switch {
	// Explicit identity denial is not a retryable Engine outage.
	case errors.Is(err, store.ErrManagedIdentityDenied):
		writeManagedLoginError(w, http.StatusForbidden, "managed_login_denied", ctx)
	// A disabled or unreachable managed-login service is retryable.
	case errors.Is(err, store.ErrManagedLoginUnavailable):
		writeManagedLoginError(w, http.StatusServiceUnavailable, "managed_login_unavailable", ctx)
	// Unknown provider failures remain internal and never echo their cause.
	default:
		writeManagedLoginError(w, http.StatusInternalServerError, "managed_login_failed", ctx)
	}
}

// writeManagedLoginError converges browser and CLI identity failures on the
// shared Engine envelope while preserving their existing stable codes.
func writeManagedLoginError(w http.ResponseWriter, status int, code string, contexts ...context.Context) {
	ctx := context.Background()
	// A handler-supplied request context enables standard request/trace correlation.
	if len(contexts) > 0 && contexts[0] != nil {
		ctx = contexts[0]
	}
	message, remediation := managedLoginErrorGuidance(code)
	writeControlAPIError(w, ctx, status, code, message, remediation)
}

// managedLoginErrorGuidance maps machine codes to bounded operator-facing
// guidance without reflecting credentials, origins, or service error prose.
func managedLoginErrorGuidance(code string) (string, string) {
	switch code {
	// Authentication failures require a fresh credential or browser session.
	case "authentication_required", "api_key_denied", "browser_session_required", "session_required":
		return "Authentication is required for this request.", "Sign in again, then retry the command."
	// Origin and CSRF failures must be repaired by restarting the trusted flow.
	case "origin_denied", "csrf_denied", "managed_login_binding_denied":
		return "The authentication request could not be verified.", "Restart sign-in from the Fused Engine and retry."
	// Explicit user or policy denials are not transient service failures.
	case "cli_login_denied", "cli_logout_denied", "managed_login_denied":
		return "The authentication request was denied.", "Confirm the account and authorization, then retry if appropriate."
	// Rate limiting has an authoritative Retry-After header from the middleware.
	case "rate_limited":
		return "Too many authentication requests were received.", "Wait for the Retry-After interval before retrying."
	// Invalid requests should be corrected locally instead of retried unchanged.
	case "invalid_request":
		return "The authentication request is invalid.", "Review the request fields and retry."
	// Availability and internal failures share safe retry guidance.
	default:
		return "The Engine could not complete the authentication request.", "Retry and check Engine logs if the problem continues."
	}
}

func writeManagedLoginJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func setManagedLoginResponseHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

var _ ManagedLoginService = (*managedauth.Service)(nil)
