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

func managedLoginStartHandler(service ManagedLoginService, cookies *browserauth.CookieManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setManagedLoginResponseHeaders(w)
		if service == nil || cookies == nil {
			writeManagedLoginError(w, http.StatusServiceUnavailable, "managed_login_unavailable")
			return
		}
		if !cookies.ValidateSameOrigin(r) {
			writeManagedLoginError(w, http.StatusForbidden, "origin_denied")
			return
		}
		if !validEmptyManagedLoginBody(w, r) {
			writeManagedLoginError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		result, err := service.Start(r.Context())
		if err != nil {
			writeManagedLoginServiceError(w, err)
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

func managedLoginPollHandler(service ManagedLoginService, cookies *browserauth.CookieManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setManagedLoginResponseHeaders(w)
		if service == nil || cookies == nil {
			writeManagedLoginError(w, http.StatusServiceUnavailable, "managed_login_unavailable")
			return
		}
		if !cookies.ValidateSameOrigin(r) {
			writeManagedLoginError(w, http.StatusForbidden, "origin_denied")
			return
		}
		request, id, err := decodeManagedLoginPollRequest(w, r)
		if err != nil {
			writeManagedLoginError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if !cookies.ValidateLoginBinding(r, id.String(), request.PollToken) {
			writeManagedLoginError(w, http.StatusForbidden, "managed_login_binding_denied")
			return
		}
		credential, err := service.Poll(r.Context(), id, request.PollToken)
		if errors.Is(err, store.ErrManagedLoginPending) {
			writeManagedLoginJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
			return
		}
		if err != nil {
			writeManagedLoginServiceError(w, err)
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

func writeManagedLoginServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrManagedIdentityDenied):
		writeManagedLoginError(w, http.StatusForbidden, "managed_login_denied")
	case errors.Is(err, store.ErrManagedLoginUnavailable):
		writeManagedLoginError(w, http.StatusServiceUnavailable, "managed_login_unavailable")
	default:
		writeManagedLoginError(w, http.StatusInternalServerError, "managed_login_failed")
	}
}

func writeManagedLoginError(w http.ResponseWriter, status int, code string) {
	writeManagedLoginJSON(w, status, map[string]string{"code": code})
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
