package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/canonical"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// resolveRESTAppReference preserves UUID routes and binds Unified App names to the supplied execution token's family.
func (s *EngineGRPCServer) resolveRESTAppReference(request *http.Request) (uuid.UUID, *restExecutionError) {
	raw := chi.URLParam(request, "app_id")
	// Chi routes against RawPath when present; decode only that representation, once, so literal percent signs survive.
	if request.URL.RawPath != "" {
		decoded, err := url.PathUnescape(raw)
		// Malformed escapes cannot produce a valid authored identity.
		if err != nil {
			return uuid.Nil, newRESTExecutionError(http.StatusBadRequest, "invalid_request", "invalid app name encoding")
		}
		raw = decoded
	}
	raw = strings.TrimSpace(raw)
	// UUID-shaped references always retain existing ID semantics instead of falling back to a similarly named app.
	if _, err := uuid.Parse(raw); err == nil {
		return parseRESTAppID(raw)
	}
	// Empty references cannot match any authored app identity.
	if raw == "" {
		return uuid.Nil, newRESTExecutionError(http.StatusBadRequest, "invalid_request", "an app ID or Unified App name is required")
	}
	name, _, err := canonical.AppName(raw)
	// URL references must satisfy the same identity rules as authored app names.
	if err != nil {
		return uuid.Nil, newRESTExecutionError(http.StatusBadRequest, "invalid_request", "invalid Unified App name")
	}
	return s.resolveRESTAppName(request, name)
}

// resolveRESTAppName performs scoped discovery only; the existing runtime validator still authorizes the selected version.
func (s *EngineGRPCServer) resolveRESTAppName(request *http.Request, name string) (uuid.UUID, *restExecutionError) {
	token, err := restBearerToken(request)
	// Name lookup never exposes deployment state to an unauthenticated caller.
	if err != nil {
		return uuid.Nil, newRESTExecutionError(http.StatusUnauthorized, "authentication_required", "a Bearer app token is required")
	}
	resolver, ok := s.store.(store.UnifiedAppNameResolver)
	// A missing resolver cannot fall back to a global name search.
	if !ok {
		return uuid.Nil, newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "app name lookup is unavailable")
	}
	digest := sha256.Sum256([]byte(token))
	familyID, err := resolver.ResolveUnifiedAppName(request.Context(), name, hex.EncodeToString(digest[:]))
	// A wrong name and a wrong token reveal the same result without disclosing whether an app exists.
	if errors.Is(err, store.ErrAppFamilyNotFound) {
		return uuid.Nil, newRESTExecutionError(http.StatusUnauthorized, "authentication_failed", "app authentication failed")
	}
	// Storage errors and invalid identities cannot proceed into runtime admission.
	if err != nil || familyID == uuid.Nil {
		return uuid.Nil, newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "app name lookup is unavailable")
	}
	return familyID, nil
}
