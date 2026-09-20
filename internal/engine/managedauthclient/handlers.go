package managedauthclient

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// MountRoutes exposes this installation's own managed-auth enrollment
// status/control surface (distinct from managedauthbroker's routes, which
// belong to whichever Engine is acting as the broker).
func MountRoutes(router chi.Router, service *Service) {
	router.Get("/workspace/managed-auth", statusHandler(service))
	router.Put("/workspace/managed-auth", enableHandler(service))
	router.Delete("/workspace/managed-auth", disableHandler(service))
}

type managedAuthStatusResponse struct {
	Status            string `json:"status"`
	RevocationPending bool   `json:"revocation_pending"`
}

// statusHandler exposes saved opt-out and pending remote cleanup without changing either.
func statusHandler(service *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeManagedAuthClientJSON(w, http.StatusOK, managedAuthState(r, service))
	}
}

// enableHandler drives the CLI/UI "Enable Fused Managed Auth" action. It
// blocks for one enrollment round trip rather than returning "pending" and
// polling, matching how first-boot opt-in is a rare, explicit action.
func enableHandler(service *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := service.Enroll(r.Context()); err != nil {
			writeManagedAuthClientError(w, http.StatusBadGateway, "enrollment is unavailable; retry shortly")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeManagedAuthClientJSON(w, http.StatusOK, managedAuthStatusResponse{Status: string(StatusReady)})
	}
}

func writeManagedAuthClientJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeManagedAuthClientError(w http.ResponseWriter, status int, message string) {
	writeManagedAuthClientJSON(w, status, struct {
		Error string `json:"error"`
	}{Error: message})
}

// managedAuthState distinguishes local disable from a broker acknowledgement still awaiting retry.
func managedAuthState(r *http.Request, service *Service) managedAuthStatusResponse {
	enabled, hasCredential, err := service.store.State(r.Context())
	// Failed reads must never falsely acknowledge completed revocation.
	if err != nil {
		return managedAuthStatusResponse{Status: string(StatusTemporarilyDown)}
	}
	return managedAuthStatusResponse{Status: string(service.Status(r.Context())), RevocationPending: !enabled && hasCredential}
}

// disableHandler acknowledges durable local opt-out and reports remote revocation separately.
func disableHandler(service *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// A failed local write means opt-out was not saved and cannot be acknowledged.
		if err := service.Disable(r.Context()); err != nil {
			writeManagedAuthClientError(w, http.StatusServiceUnavailable, "could not save managed-auth preference")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeManagedAuthClientJSON(w, http.StatusOK, managedAuthState(r, service))
	}
}
