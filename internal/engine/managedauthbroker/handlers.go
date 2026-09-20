package managedauthbroker

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
)

const maxManagedAuthBrokerBodyBytes = 4 << 10

// MountRoutes exposes the broker's enrollment/refresh surface. The caller
// decides whether to mount this at all: only the one Engine instance
// designated as Fused's managed-auth broker should ever expose it, gated by
// config (see cmd/engine/cmd/start.go), because any customer Engine that
// mounted it would be offering to vouch for other customers' installations.
func MountRoutes(router chi.Router, service *Service) {
	router.Post("/managed-auth/broker/enroll", enrollHandler(service))
	router.Post("/managed-auth/broker/refresh", refreshHandler(service))
	router.Post("/managed-auth/broker/revoke", revokeHandler(service))
}

type tokenResponse struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	TokenType    string   `json:"token_type"`
	ExpiresIn    int64    `json:"expires_in"`
	Scope        []string `json:"scope"`
}

func writeTokenResponse(w http.ResponseWriter, result TokenResponse) {
	w.Header().Set("Cache-Control", "no-store")
	writeBrokerJSON(w, http.StatusOK, tokenResponse{
		AccessToken: result.AccessToken, RefreshToken: result.RefreshToken,
		TokenType: "Bearer", ExpiresIn: result.ExpiresIn, Scope: result.Scope,
	})
}

type enrollRequest struct {
	Ticket string `json:"ticket"`
}

// enrollHandler exchanges a single-use Registry proof without exposing internal persistence failures.
func enrollHandler(service *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req enrollRequest
		// Reject missing or oversized bearer requests before invoking the service.
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxManagedAuthBrokerBodyBytes)).Decode(&req); err != nil || req.Ticket == "" {
			writeBrokerError(w, http.StatusBadRequest, "ticket is required")
			return
		}
		result, err := service.Enroll(r.Context(), req.Ticket)
		// Only explicit credential rejection warrants authentication recovery on the consumer.
		if errors.Is(err, ErrInvalidTicket) {
			writeBrokerError(w, http.StatusUnauthorized, "ticket is invalid, expired, or already used")
			return
		}
		// Transient storage failure must not be confused with consumed credentials.
		if err != nil {
			writeBrokerError(w, http.StatusServiceUnavailable, "managed-auth broker temporarily unavailable")
			return
		}
		writeTokenResponse(w, result)
	}
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
	Ticket       string `json:"ticket"`
}

// refreshHandler distinguishes an invalid bearer from a retryable broker outage.
func refreshHandler(service *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req refreshRequest
		// Reject missing or oversized bearer requests before invoking the service.
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxManagedAuthBrokerBodyBytes)).Decode(&req); err != nil || req.RefreshToken == "" || req.Ticket == "" {
			writeBrokerError(w, http.StatusBadRequest, "refresh_token and ticket are required")
			return
		}
		result, err := service.Refresh(r.Context(), req.RefreshToken, req.Ticket)
		// Only explicit credential rejection warrants authentication recovery on the consumer.
		if errors.Is(err, ErrInvalidTicket) {
			writeBrokerError(w, http.StatusUnauthorized, "refresh_token is invalid, expired, or already used")
			return
		}
		// Transient storage failure must not be confused with consumed credentials.
		if err != nil {
			writeBrokerError(w, http.StatusServiceUnavailable, "managed-auth broker temporarily unavailable")
			return
		}
		writeTokenResponse(w, result)
	}
}

func writeBrokerJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeBrokerError(w http.ResponseWriter, status int, message string) {
	writeBrokerJSON(w, status, struct {
		Error string `json:"error"`
	}{Error: message})
}

// revokeHandler makes revocation retryable even when an earlier acknowledgement was lost.
func revokeHandler(service *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req refreshRequest
		// Bounded credential input is required; Registry authority is deliberately unnecessary for withdrawal.
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxManagedAuthBrokerBodyBytes)).Decode(&req); err != nil || req.RefreshToken == "" {
			writeBrokerError(w, http.StatusBadRequest, "refresh_token is required")
			return
		}
		// A failed write leaves revocation pending instead of falsely acknowledging it.
		if err := service.Revoke(r.Context(), req.RefreshToken); err != nil {
			writeBrokerError(w, http.StatusServiceUnavailable, "revocation temporarily unavailable")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeBrokerJSON(w, http.StatusOK, struct{}{})
	}
}
