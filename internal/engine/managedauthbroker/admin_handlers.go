package managedauthbroker

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Bound operator metadata while admitting up to 64 explicit account/installation grants.
const maxManagedAuthAdminBodyBytes = 16 << 10

// MountAdminRoutes restricts publication management to broker operators;
// enrolled consumer Engines cannot register or alter provider applications.
func MountAdminRoutes(router chi.Router, catalog *CatalogStore, masterKey []byte, adminKey string) {
	admin := router.With(requireAdminKey(adminKey))
	const path = "/managed-auth/broker/admin/apps/{serviceID}/{authName}"
	admin.Put(path, upsertProviderAppHandler(catalog, masterKey))
	admin.Delete(path, revokeProviderAppHandler(catalog))
	admin.Delete(path+"/applications/{applicationID}", revokeProviderAppHandler(catalog))
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

// revokeProviderAppHandler permits only operators to withdraw access while keeping publication identity permanently reserved.
func revokeProviderAppHandler(catalog *CatalogStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		service, auth, ok := connectRouteIdentity(r)
		// Missing or malformed routing must never widen withdrawal to another application.
		if !ok {
			writeBrokerError(w, http.StatusBadRequest, "invalid application identity")
			return
		}
		// Withdrawal remains available even when the source bucket or provider contract is unavailable.
		if err := catalog.RevokeRegistrationAccess(r.Context(), service, auth, chi.URLParam(r, "applicationID")); err != nil {
			writeBrokerError(w, http.StatusNotFound, "publication unavailable")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
