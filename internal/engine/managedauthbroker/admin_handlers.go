package managedauthbroker

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const maxManagedAuthAdminBodyBytes = 4 << 10

// MountAdminRoutes exposes catalogue registration for Fused's own operators.
// There is deliberately no customer-facing path to this: the managed-app
// catalogue is Fused's own OAuth application inventory, registered with each
// provider out of band, not something any Engine installation configures.
func MountAdminRoutes(router chi.Router, catalog *CatalogStore, masterKey []byte, adminKey string) {
	router.With(requireAdminKey(adminKey)).Put("/managed-auth/broker/admin/apps/{serviceID}/{authName}", upsertProviderAppHandler(catalog, masterKey))
}

// requireAdminKey fails closed: an unset adminKey rejects every request
// rather than admitting one, so forgetting to configure it disables the
// route instead of silently leaving it open.
func requireAdminKey(adminKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented := bearerToken(r)
			if adminKey == "" || presented == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(adminKey)) != 1 {
				writeBrokerError(w, http.StatusUnauthorized, "a valid managed-auth broker admin key is required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// upsertProviderAppHandler admits credential destinations only through the operator-authenticated registration route.
func upsertProviderAppHandler(catalog *CatalogStore, masterKey []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serviceID, err := uuid.Parse(chi.URLParam(r, "serviceID"))
		// Malformed service identities cannot select an operator registration.
		if err != nil {
			writeBrokerError(w, http.StatusBadRequest, "service id is invalid")
			return
		}
		authName := chi.URLParam(r, "authName")
		// A scheme is required to identify one exact provider app.
		if authName == "" {
			writeBrokerError(w, http.StatusBadRequest, "auth name is required")
			return
		}
		var req Registration
		// Strict decoding keeps credential values and duplicate provider policy out of the publication API.
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxManagedAuthAdminBodyBytes))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			writeBrokerError(w, http.StatusBadRequest, "a bucket and pinned service registration are required")
			return
		}
		// Trailing JSON must not be interpreted as a second hidden registration mutation.
		if decoder.Decode(new(any)) != io.EOF {
			writeBrokerError(w, http.StatusBadRequest, "invalid registration")
			return
		}
		// Publication is an explicit reference to existing operator-owned Engine material.
		if err := catalog.PublishRegistration(r.Context(), serviceID, authName, req, masterKey); err != nil {
			writeBrokerError(w, http.StatusBadRequest, "registration unavailable or already pinned differently")
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
