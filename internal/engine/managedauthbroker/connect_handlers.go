package managedauthbroker

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/shared/fusedobject"
)

const maxManagedAuthConnectBodyBytes = 64 << 10

// MountConnectRoutes exposes the connect-time proxy: an enrolled
// installation calls these instead of talking to the provider directly with
// a locally-held secret, because for a Fused Managed App there is no local
// secret to hold. Every route requires the installation's own
// managed_auth:connect access token, resolved fresh on each request so a
// revoked or expired installation loses access immediately.
func MountConnectRoutes(router chi.Router, connect *ConnectService, installations *Store) {
	router.Route("/managed-auth/broker/connect/{serviceID}/{authName}", func(r chi.Router) {
		r.Use(requireInstallationAccessToken(installations))
		r.Get("/client-id", clientIDHandler(connect))
		r.Post("/exchange", exchangeHandler(connect))
		r.Post("/refresh", connectRefreshHandler(connect))
	})
}

type installationContextKey struct{}

// requireInstallationAccessToken resolves the live installation for the
// caller's bearer access token, the same way any other Engine control
// credential is resolved at the edge, before any connect-proxy handler runs.
func requireInstallationAccessToken(installations *Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				writeBrokerError(w, http.StatusUnauthorized, "a managed-auth installation access token is required")
				return
			}
			installation, err := installations.GetByAccessToken(r.Context(), hashCredential(token))
			if err != nil {
				writeBrokerError(w, http.StatusUnauthorized, "installation access token is invalid, expired, or revoked")
				return
			}
			ctx := context.WithValue(r.Context(), installationContextKey{}, installation)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(header, prefix))
}

func connectRouteIdentity(r *http.Request) (serviceID uuid.UUID, authName string, ok bool) {
	serviceID, err := uuid.Parse(chi.URLParam(r, "serviceID"))
	if err != nil {
		return uuid.Nil, "", false
	}
	authName = chi.URLParam(r, "authName")
	if authName == "" {
		return uuid.Nil, "", false
	}
	return serviceID, authName, true
}

type clientIDResponse struct {
	ClientID string `json:"client_id"`
}

func clientIDHandler(connect *ConnectService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serviceID, authName, ok := connectRouteIdentity(r)
		if !ok {
			writeBrokerError(w, http.StatusBadRequest, "service id and auth name are required")
			return
		}
		clientID, err := connect.ClientID(r.Context(), serviceID, authName)
		if err != nil {
			writeBrokerError(w, http.StatusNotFound, "no Fused Managed App is registered for this service")
			return
		}
		writeBrokerJSON(w, http.StatusOK, clientIDResponse{ClientID: clientID})
	}
}

// connectExchangeRequest accepts legacy auth/flow fields for wire compatibility only.
// The service ignores them and resolves all token routing and credential placement from its operator catalogue.
type connectExchangeRequest struct {
	RedirectURI string                         `json:"redirect_uri"`
	Auth        fusedobject.AuthConfig         `json:"auth"`
	Flow        fusedobject.OAuth2FlowContract `json:"flow"`
	Code        string                         `json:"code"`
	Verifier    string                         `json:"verifier"`
}

func exchangeHandler(connect *ConnectService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serviceID, authName, ok := connectRouteIdentity(r)
		if !ok {
			writeBrokerError(w, http.StatusBadRequest, "service id and auth name are required")
			return
		}
		var req connectExchangeRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxManagedAuthConnectBodyBytes)).Decode(&req); err != nil || req.Code == "" {
			writeBrokerError(w, http.StatusBadRequest, "code is required")
			return
		}
		token, err := connect.Exchange(r.Context(), serviceID, authName, req.RedirectURI, req.Auth, req.Flow, req.Code, req.Verifier)
		if err != nil {
			writeBrokerError(w, http.StatusBadGateway, "provider token exchange failed")
			return
		}
		writeBrokerJSON(w, http.StatusOK, token)
	}
}

type connectRefreshRequest struct {
	RedirectURI  string                         `json:"redirect_uri"`
	Auth         fusedobject.AuthConfig         `json:"auth"`
	Flow         fusedobject.OAuth2FlowContract `json:"flow"`
	RefreshToken string                         `json:"refresh_token"`
}

// connectRefreshHandler avoids colliding with the installation-credential
// refreshHandler already defined in handlers.go -- the two refresh
// different things (the installation's own broker credential vs. the
// end-user's provider tokens) and deliberately stay separate endpoints.
func connectRefreshHandler(connect *ConnectService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serviceID, authName, ok := connectRouteIdentity(r)
		if !ok {
			writeBrokerError(w, http.StatusBadRequest, "service id and auth name are required")
			return
		}
		var req connectRefreshRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxManagedAuthConnectBodyBytes)).Decode(&req); err != nil || req.RefreshToken == "" {
			writeBrokerError(w, http.StatusBadRequest, "refresh_token is required")
			return
		}
		token, err := connect.Refresh(r.Context(), serviceID, authName, req.RedirectURI, req.Auth, req.Flow, req.RefreshToken)
		if err != nil {
			writeBrokerError(w, http.StatusBadGateway, "provider token refresh failed")
			return
		}
		writeBrokerJSON(w, http.StatusOK, token)
	}
}
