package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/oauthprovider"
	"github.com/Usefused/engine/internal/engine/store"
	"net/http"
	"strings"
)

// deploymentManifestWithin rejects other apps, provider-version changes and additional operations before planning.
func deploymentManifestWithin(next, ceiling runtimeAccessManifest) bool {
	// Semantic app versions may advance; app identity and selected provider versions remain pinned.
	if next.Name != ceiling.Name || next.Bucket != ceiling.Bucket || len(next.Services) == 0 {
		return false
	}
	for name, service := range next.Services {
		approved, ok := ceiling.Services[name]
		// New providers or provider versions require a new browser consent ceremony.
		if !ok || service.Version != approved.Version {
			return false
		}
		allowed := map[string]bool{}
		for _, operation := range approved.Operations {
			allowed[operation] = true
		}
		for _, operation := range service.Operations {
			// A previously unused operation still broadens authority and must be reviewed.
			if !allowed[operation] {
				return false
			}
		}
	}
	return true
}

// deploy accepts only a dedicated app-family grant and returns a separate version-pinned execution credential.
func (h *runtimeOAuthHandler) deploy(w http.ResponseWriter, r *http.Request) {
	setOAuthResponseHeaders(w)
	repository, ok := h.store.(store.OAuthDeploymentStore)
	// No fallback to a broad management token exists for older Engine stores.
	if !ok {
		writeOAuthTokenError(w, 503, "temporarily_unavailable")
		return
	}
	authorization := r.Header.Get("Authorization")
	// Bearer proof is accepted only in a header, never a URL or browser cookie.
	if !strings.HasPrefix(authorization, "Bearer ") || len(authorization) > 8192 {
		writeOAuthTokenError(w, 401, "invalid_token")
		return
	}
	hash := accesscontrol.HashControlCredential(strings.TrimPrefix(authorization, "Bearer "))
	actor, ceiling, raw, err := repository.LoadOAuthDeployment(r.Context(), hash)
	// Expired/revoked credentials cannot inspect a manifest or publish a release.
	if err != nil {
		writeOAuthTokenError(w, 401, "invalid_token")
		return
	}
	var input struct {
		Manifest string `json:"manifest"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	// Unknown config fields must not bypass the narrow deployment declaration.
	if decodeOneStrictJSON(r.Body, &input) != nil {
		writeOAuthTokenError(w, 400, "invalid_request")
		return
	}
	next, err := parseRuntimeManifest([]byte(input.Manifest))
	approved, approvedErr := parseRuntimeManifest(raw)
	// The unchanged original ceiling controls every release, not merely the preceding one.
	if err != nil || approvedErr != nil || !deploymentManifestWithin(next, approved) {
		writeOAuthTokenError(w, 403, "consent_required")
		return
	}
	buckets := make([]store.Bucket, 0, len(ceiling.Buckets))
	// Resolve stable approved IDs rather than trusting recycled bucket names.
	for _, choice := range ceiling.Buckets {
		bucket, err := h.store.GetBucket(r.Context(), choice.ID)
		// Missing or changed credential routes cannot become implicit defaults.
		if err != nil || bucket == nil {
			writeOAuthTokenError(w, 403, "consent_required")
			return
		}
		// Runtime publish expects the selected default first.
		if choice.ID == ceiling.DefaultBucketID {
			buckets = append([]store.Bucket{*bucket}, buckets...)
		} else {
			buckets = append(buckets, *bucket)
		}
	}
	// Only the one existing family is grantable; there is deliberately no app.create permission.
	grant, err := h.publishWithCeiling(r.Context(), actor, next, store.RuntimeManifestDigest([]byte(input.Manifest)), buckets, &ceiling)
	if err != nil {
		writeOAuthTokenError(w, 403, "deployment_denied")
		return
	}
	// Defense in depth: even a planner bug cannot issue another family's runtime credential.
	if grant.AppFamilyID != ceiling.AppFamilyID {
		writeOAuthTokenError(w, 403, "deployment_denied")
		return
	}
	entropy := make([]byte, 32)
	// Entropy failure cannot create a predictable deployment credential.
	if _, err := rand.Read(entropy); err != nil {
		writeOAuthTokenError(w, 500, "server_error")
		return
	}
	token := "frt_" + base64.RawURLEncoding.EncodeToString(entropy)
	sum := sha256.Sum256([]byte(token))
	// Atomic issuance rechecks consent after publication and preserves earlier release credentials.
	if err := repository.IssueOAuthDeploymentToken(r.Context(), hash, grant, hex.EncodeToString(sum[:])); err != nil {
		writeOAuthTokenError(w, 403, "deployment_denied")
		return
	}
	writeOAuthJSON(w, 200, oauthprovider.RuntimeTokenResponse{OAuthRuntimeGrant: grant, RuntimeToken: token})
}
