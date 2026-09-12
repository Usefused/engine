package engine

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/models"
)

// validateOAuthPlacementHeaders rejects incompatible header ownership in a selected security AND-set.
func validateOAuthPlacementHeaders(auths models.AuthConfigs) error {
	for index, auth := range auths {
		// Existing contracts retain their behavior unless they explicitly declare placement.
		if auth.OAuthTokenPlacement == nil {
			continue
		}
		// Validate before inspecting destination ownership or injecting any credential.
		if err := auth.OAuthTokenPlacement.Validate(auth.Type); err != nil {
			return err
		}
		for otherIndex, other := range auths {
			// Only distinct schemes can conflict with this credential destination.
			if index != otherIndex && strings.EqualFold(auth.OAuthTokenPlacement.Name, selectedAuthHeader(other)) {
				return authRoutingError("header_collision")
			}
		}
	}
	return nil
}

// selectedAuthHeader mirrors dispatcher credential destinations without reading any secret values.
func selectedAuthHeader(auth models.AuthConfig) string {
	// Explicit OAuth placement replaces the usual Authorization header.
	if auth.OAuthTokenPlacement != nil {
		return auth.OAuthTokenPlacement.Name
	}
	// Header ownership depends on the credential strategy, not its display name.
	switch authrouting.CanonicalType(auth.Type, auth.Scheme) {
	case "basic", "bearer", "oauth", "oidc", "oauth1", "digest":
		return "Authorization"
	case "api_key":
		// Query and cookie API keys do not compete with custom OAuth headers.
		if auth.Location == "header" {
			return auth.KeyName
		}
	}
	return ""
}

// oauthPlacementClient prevents custom credential headers from following redirects to another origin.
func oauthPlacementClient(client *http.Client, auths models.AuthConfigs) *http.Client {
	for _, auth := range auths {
		// Ordinary OAuth keeps the existing client; explicit placement requires stricter forwarding rules.
		if auth.OAuthTokenPlacement == nil {
			continue
		}
		scoped := *client
		previous := scoped.CheckRedirect
		// A private client copy preserves shared transport and any existing mTLS redirect checks.
		scoped.CheckRedirect = func(next *http.Request, via []*http.Request) error {
			// Bound redirects as net/http normally does, even when no earlier policy was installed.
			if len(via) == 0 || len(via) >= 10 {
				return errors.New("OAuth redirect limit exceeded")
			}
			origin := via[0].URL
			// Compare scheme and authority; custom auth headers are not stripped automatically by net/http.
			if next.URL.User != nil || !strings.EqualFold(next.URL.Host, origin.Host) || !strings.EqualFold(next.URL.Scheme, origin.Scheme) {
				return errors.New("OAuth cross-origin redirect blocked")
			}
			// Preserve stronger transport constraints from an existing client.
			if previous != nil {
				return previous(next, via)
			}
			return nil
		}
		return &scoped
	}
	return client
}
