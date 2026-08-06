package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/browserauth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
)

const maxBrowserSessionBodyBytes = 4 << 10

type BrowserSessionService interface {
	ExchangeLicenseKey(context.Context, string) (store.BrowserSessionCredential, error)
	Session(context.Context, string) (accesscontrol.Actor, error)
	Logout(context.Context, string) error
	Cookies() *browserauth.CookieManager
}

type licenseExchangeRequest struct {
	LicenseKey string `json:"license_key"`
}

func MountBrowserSessionRoutes(router chi.Router, service BrowserSessionService) {
	router.Get("/auth/session", browserSessionStatusHandler(service))
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(10, 100, time.Minute))).
		Post("/auth/license/exchange", browserLicenseExchangeHandler(service))
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(10, 100, time.Minute))).
		Post("/auth/api-key/exchange", browserLicenseExchangeHandler(service))
	router.Post("/auth/logout", browserSessionLogoutHandler(service))
}

func browserSessionStatusHandler(service BrowserSessionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setManagedLoginResponseHeaders(w)
		credential, source, _ := browserSessionCredential(r, service)
		if source != browserauth.CredentialSourceCookie {
			writeManagedLoginJSON(w, http.StatusOK, map[string]any{"authenticated": false})
			return
		}
		actor, err := service.Session(r.Context(), credential)
		if err != nil {
			service.Cookies().ClearSession(w)
			writeManagedLoginJSON(w, http.StatusOK, map[string]any{"authenticated": false})
			return
		}
		service.Cookies().RefreshCSRF(w, credential)
		writeManagedLoginJSON(w, http.StatusOK, map[string]any{
			"authenticated": true, "subject_kind": actor.Kind,
		})
	}
}

func browserLicenseExchangeHandler(service BrowserSessionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setManagedLoginResponseHeaders(w)
		if service == nil {
			writeManagedLoginError(w, http.StatusServiceUnavailable, "browser_auth_unavailable")
			return
		}
		if !service.Cookies().ValidateSameOrigin(r) {
			writeManagedLoginError(w, http.StatusForbidden, "origin_denied")
			return
		}
		licenseKey, err := decodeLicenseExchangeRequest(w, r)
		if err != nil {
			writeManagedLoginError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		credential, err := service.ExchangeLicenseKey(r.Context(), licenseKey)
		if errors.Is(err, browserauth.ErrAPIKeyRequired) {
			writeManagedLoginError(w, http.StatusUnauthorized, "api_key_denied")
			return
		}
		if err != nil {
			writeManagedLoginError(w, http.StatusInternalServerError, "browser_auth_failed")
			return
		}
		service.Cookies().SetSession(w, credential.RawKey, credential.ExpiresAt)
		writeManagedLoginJSON(w, http.StatusOK, map[string]string{"status": "authenticated"})
	}
}

func browserSessionLogoutHandler(service BrowserSessionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setManagedLoginResponseHeaders(w)
		credential, source, err := browserSessionCredential(r, service)
		if err != nil || source != browserauth.CredentialSourceCookie {
			writeManagedLoginError(w, http.StatusUnauthorized, "session_required")
			return
		}
		if !service.Cookies().ValidateCSRF(r, credential) {
			writeManagedLoginError(w, http.StatusForbidden, "csrf_denied")
			return
		}
		err = service.Logout(r.Context(), credential)
		if err != nil && !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
			writeManagedLoginError(w, http.StatusInternalServerError, "logout_failed")
			return
		}
		// Keep the cookie on transient persistence failures so the browser can
		// repair CSRF/retry; invalid or revoked sessions are safe to clear.
		service.Cookies().ClearSession(w)
		w.WriteHeader(http.StatusNoContent)
	}
}

func browserSessionCredential(r *http.Request, service BrowserSessionService) (string, browserauth.CredentialSource, error) {
	if service == nil || service.Cookies() == nil {
		return "", browserauth.CredentialSourceNone, errors.New("browser auth unavailable")
	}
	return browserauth.CredentialFromRequest(r, service.Cookies())
}

func decodeLicenseExchangeRequest(w http.ResponseWriter, r *http.Request) (string, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBrowserSessionBodyBytes))
	decoder.DisallowUnknownFields()
	var request licenseExchangeRequest
	if err := decoder.Decode(&request); err != nil {
		return "", err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", errors.New("invalid request")
	}
	if request.LicenseKey == "" || len(request.LicenseKey) > 1024 || strings.TrimSpace(request.LicenseKey) != request.LicenseKey {
		return "", errors.New("invalid request")
	}
	return request.LicenseKey, nil
}

var _ BrowserSessionService = (*browserauth.Service)(nil)
