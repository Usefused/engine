package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/browserauth"
	"github.com/Usefused/engine/internal/engine/cliauth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
)

const maxCLILoginBodyBytes = 4 << 10

type CLILoginService interface {
	Start(context.Context, cliauth.StartInput) (cliauth.StartResult, error)
	Approve(context.Context, uuid.UUID, string, accesscontrol.Actor) error
	Poll(context.Context, uuid.UUID, string) (store.CLILoginCredential, error)
	Logout(context.Context, cliauth.LogoutInput) error
}

type cliWhoAmIResponse struct {
	Authenticated        bool                      `json:"authenticated"`
	AccountID            uuid.UUID                 `json:"account_id"`
	WorkspaceID          uuid.UUID                 `json:"workspace_id"`
	SubjectID            uuid.UUID                 `json:"subject_id"`
	SubjectKind          accesscontrol.SubjectKind `json:"subject_kind"`
	DisplayName          string                    `json:"display_name"`
	Email                string                    `json:"email,omitempty"`
	CredentialID         uuid.UUID                 `json:"credential_id"`
	CredentialSource     string                    `json:"credential_source"`
	AuthenticationMethod string                    `json:"authentication_method"`
	ExpiresAt            *time.Time                `json:"expires_at,omitempty"`
}

type cliLoginStartRequest struct {
	CredentialHash   string `json:"credential_hash"`
	CredentialPrefix string `json:"credential_prefix"`
}

type cliLoginCapabilityRequest struct {
	TransactionID string `json:"transaction_id"`
	Token         string `json:"token"`
}

func MountCLILoginRoutes(router chi.Router, service CLILoginService, browser BrowserSessionService) {
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(10, 100, time.Minute))).
		Post("/auth/cli/start", cliLoginStartHandler(service))
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(120, 1000, time.Minute))).
		Post("/auth/cli/poll", cliLoginPollHandler(service))
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(20, 200, time.Minute))).
		Post("/auth/cli/approve", cliLoginApproveHandler(service, browser))
	router.Get("/auth/whoami", cliWhoAmIHandler())
	router.Post("/auth/cli/logout", cliLogoutHandler(service))
}

// cliWhoAmIHandler returns the authenticated human identity without exposing credentials.
func cliWhoAmIHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setManagedLoginResponseHeaders(w)
		actor, ok := accesscontrol.ActorFromContext(r.Context())
		// An absent actor is an authentication failure, not an anonymous identity response.
		if !ok {
			writeCLILoginError(w, r.Context(), http.StatusUnauthorized, "authentication_required")
			return
		}
		writeManagedLoginJSON(w, http.StatusOK, cliWhoAmIResponse{
			Authenticated: true, AccountID: actor.AccountID, WorkspaceID: actor.WorkspaceID,
			SubjectID: actor.SubjectID, SubjectKind: actor.Kind, DisplayName: actor.DisplayName,
			Email: actor.Email, CredentialID: actor.CredentialID,
			CredentialSource: actor.CredentialSource, AuthenticationMethod: actor.AuthenticationMethod,
			ExpiresAt: actor.CredentialExpiresAt,
		})
	}
}

// cliLogoutHandler revokes only the credential represented by the authenticated actor.
func cliLogoutHandler(service CLILoginService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setManagedLoginResponseHeaders(w)
		// A missing service is a retryable Engine availability failure.
		if service == nil {
			writeCLILoginError(w, r.Context(), http.StatusServiceUnavailable, "cli_logout_unavailable")
			return
		}
		actor, ok := accesscontrol.ActorFromContext(r.Context())
		// Logout cannot act on caller-provided credential identifiers.
		if !ok {
			writeCLILoginError(w, r.Context(), http.StatusUnauthorized, "authentication_required")
			return
		}
		err := service.Logout(r.Context(), cliauth.LogoutInput{Actor: actor, RequestID: chimiddleware.GetReqID(r.Context())})
		// Store failures retain their stable denial or availability classification.
		if err != nil {
			writeCLILogoutServiceError(w, r.Context(), err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// cliLoginStartHandler creates one bounded device-style login transaction.
func cliLoginStartHandler(service CLILoginService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setManagedLoginResponseHeaders(w)
		// A missing service is reported before any credential material is parsed.
		if service == nil {
			writeCLILoginError(w, r.Context(), http.StatusServiceUnavailable, "cli_login_unavailable")
			return
		}
		request, err := decodeCLILoginStartRequest(w, r)
		// Malformed bounded JSON is caller-remediable and safe to classify precisely.
		if err != nil {
			writeCLILoginError(w, r.Context(), http.StatusBadRequest, "invalid_request")
			return
		}
		result, err := service.Start(r.Context(), cliauth.StartInput{
			CredentialHash: request.CredentialHash, CredentialPrefix: request.CredentialPrefix,
		})
		// Service errors are mapped without copying internal error prose.
		if err != nil {
			writeCLILoginServiceError(w, r.Context(), err)
			return
		}
		writeManagedLoginJSON(w, http.StatusCreated, map[string]string{
			"transaction_id": result.TransactionID.String(), "poll_token": result.PollToken,
			"browser_token": result.BrowserToken, "expires_at": result.ExpiresAt.Format(time.RFC3339),
		})
	}
}

// cliLoginPollHandler exchanges a valid transaction capability for CLI authentication state.
func cliLoginPollHandler(service CLILoginService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setManagedLoginResponseHeaders(w)
		// A missing service is a retryable Engine availability failure.
		if service == nil {
			writeCLILoginError(w, r.Context(), http.StatusServiceUnavailable, "cli_login_unavailable")
			return
		}
		request, id, err := decodeCLILoginCapabilityRequest(w, r)
		// Invalid capabilities are rejected before the login store is queried.
		if err != nil {
			writeCLILoginError(w, r.Context(), http.StatusBadRequest, "invalid_request")
			return
		}
		credential, err := service.Poll(r.Context(), id, request.Token)
		if errors.Is(err, store.ErrCLILoginPending) {
			writeManagedLoginJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
			return
		}
		// Non-pending service failures use the stable login error taxonomy.
		if err != nil {
			writeCLILoginServiceError(w, r.Context(), err)
			return
		}
		writeManagedLoginJSON(w, http.StatusOK, map[string]string{
			"status": "authenticated", "credential_id": credential.CredentialID.String(),
			"expires_at": credential.ExpiresAt.Format(time.RFC3339),
		})
	}
}

// cliLoginApproveHandler binds a browser-authenticated actor to one CLI login capability.
func cliLoginApproveHandler(service CLILoginService, browser BrowserSessionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setManagedLoginResponseHeaders(w)
		// Approval cannot proceed without the login service.
		if service == nil {
			writeCLILoginError(w, r.Context(), http.StatusServiceUnavailable, "cli_login_unavailable")
			return
		}
		actor, status, code, err := cliApprovalActor(r, browser)
		// Browser-origin and CSRF failures retain the classifier chosen by cliApprovalActor.
		if err != nil {
			writeCLILoginError(w, r.Context(), status, code)
			return
		}
		request, id, err := decodeCLILoginCapabilityRequest(w, r)
		// Invalid transaction capabilities never reach service approval.
		if err != nil {
			writeCLILoginError(w, r.Context(), http.StatusBadRequest, "invalid_request")
			return
		}
		// Approval failures are safe only after stable service classification.
		if err := service.Approve(r.Context(), id, request.Token, actor); err != nil {
			writeCLILoginServiceError(w, r.Context(), err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func cliApprovalActor(r *http.Request, browser BrowserSessionService) (accesscontrol.Actor, int, string, error) {
	if browser == nil || browser.Cookies() == nil {
		return accesscontrol.Actor{}, http.StatusServiceUnavailable, "cli_login_unavailable", errors.New("browser authentication unavailable")
	}
	cookies := browser.Cookies()
	if !cookies.ValidateSameOrigin(r) {
		return accesscontrol.Actor{}, http.StatusForbidden, "origin_denied", errors.New("origin denied")
	}
	credential, source, err := browserSessionCredential(r, browser)
	if err != nil || source != browserauth.CredentialSourceCookie || !cookies.ValidateCSRF(r, credential) {
		return accesscontrol.Actor{}, http.StatusUnauthorized, "browser_session_required", accesscontrol.ErrAuthenticationRequired
	}
	actor, err := browser.Session(r.Context(), credential)
	if err != nil {
		return accesscontrol.Actor{}, http.StatusUnauthorized, "browser_session_required", err
	}
	return actor, 0, "", nil
}

func decodeCLILoginStartRequest(w http.ResponseWriter, r *http.Request) (cliLoginStartRequest, error) {
	var request cliLoginStartRequest
	if err := decodeCLILoginJSON(w, r, &request); err != nil {
		return cliLoginStartRequest{}, err
	}
	return request, nil
}

func decodeCLILoginCapabilityRequest(w http.ResponseWriter, r *http.Request) (cliLoginCapabilityRequest, uuid.UUID, error) {
	var request cliLoginCapabilityRequest
	if err := decodeCLILoginJSON(w, r, &request); err != nil {
		return cliLoginCapabilityRequest{}, uuid.Nil, err
	}
	id, err := uuid.Parse(request.TransactionID)
	if err != nil || id == uuid.Nil || request.Token == "" || len(request.Token) > 128 {
		return cliLoginCapabilityRequest{}, uuid.Nil, errors.New("invalid request")
	}
	return request, id, nil
}

func decodeCLILoginJSON(w http.ResponseWriter, r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCLILoginBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invalid request")
	}
	return nil
}

// writeCLILoginServiceError maps login-store outcomes to stable public codes.
func writeCLILoginServiceError(w http.ResponseWriter, ctx context.Context, err error) {
	switch {
	// A denied capability is final and should not be retried unchanged.
	case errors.Is(err, store.ErrCLILoginDenied):
		writeCLILoginError(w, ctx, http.StatusForbidden, "cli_login_denied")
	// Pending is normal protocol state rather than an error envelope.
	case errors.Is(err, store.ErrCLILoginPending):
		writeManagedLoginJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
	// Unknown service failures are bounded as availability problems.
	default:
		writeCLILoginError(w, ctx, http.StatusServiceUnavailable, "cli_login_unavailable")
	}
}

// writeCLILoginError preserves CLI-specific codes in the shared identity envelope.
func writeCLILoginError(w http.ResponseWriter, ctx context.Context, status int, code string) {
	writeManagedLoginError(w, status, code, ctx)
}

// writeCLILogoutServiceError distinguishes an explicit denial from a retryable outage.
func writeCLILogoutServiceError(w http.ResponseWriter, ctx context.Context, err error) {
	// Actor and credential mismatches are caller-visible policy denials.
	if errors.Is(err, store.ErrCLILogoutDenied) || errors.Is(err, store.ErrInvalidMutationActor) {
		writeCLILoginError(w, ctx, http.StatusForbidden, "cli_logout_denied")
		return
	}
	writeCLILoginError(w, ctx, http.StatusServiceUnavailable, "cli_logout_unavailable")
}

var _ CLILoginService = (*cliauth.Service)(nil)
