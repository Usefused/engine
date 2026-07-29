package sandbox

import (
	"fmt"
	"strings"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

const credentialKeyFusedAuthType = "fused_auth_type"

// requestedAuthType reads the SDK-facing selector from credentials so every
// execution path shares the same auth-type vocabulary.
func requestedAuthType(credentials map[string]any) string {
	return canonicalAuthSelector(credentialString(credentials, credentialKeyFusedAuthType))
}

// canonicalAuthSelector accepts only the public selector vocabulary so imported
// OpenAPI spellings never leak into SDK method calls.
func canonicalAuthSelector(raw string) string {
	authType := strings.ToLower(strings.TrimSpace(raw))
	authType = strings.ReplaceAll(authType, "-", "_")
	switch authType {
	case "api_key", "oauth", "oidc", "basic", "bearer", "mtls":
		return authType
	default:
		// User-facing selectors intentionally reject imported/OpenAPI spellings
		// such as oauth2/openIdConnect; registry auth configs normalize below.
		return authType
	}
}

func CanonicalFusedAuthType(auth fusedobject.AuthConfig) string {
	return canonicalFusedAuthType(auth)
}

// canonicalFusedAuthType normalizes registry auth configs before connected-auth
// resolution, where auth names may be absent.
func canonicalFusedAuthType(auth fusedobject.AuthConfig) string {
	return canonicalAuthConfigType(auth.Type, auth.Scheme)
}

// canonicalModelAuthType normalizes dispatcher auth configs after registry-to-
// model projection so selection works in the actual execution shape.
func canonicalModelAuthType(auth models.AuthConfig) string {
	return canonicalAuthConfigType(auth.Type, auth.Scheme)
}

// canonicalAuthConfigType collapses OpenAPI and Fused aliases into user-facing
// auth families; callers should not need to remember imported schema spelling.
func canonicalAuthConfigType(rawType, scheme string) string {
	authType := strings.ToLower(strings.TrimSpace(rawType))
	authType = strings.ReplaceAll(authType, "-", "_")
	switch authType {
	case "apikey", "api_key":
		return "api_key"
	case "oauth", "oauth2", "oauth2_authorization_code":
		return "oauth"
	case "openidconnect", "open_id_connect", "oidc":
		return "oidc"
	case "mutualtls", "mutual_tls", "mtls":
		return "mtls"
	case "basic", "bearer":
		return authType
	case "http":
		return canonicalHTTPScheme(scheme)
	default:
		return authType
	}
}

// canonicalHTTPScheme treats OpenAPI http schemes as selectable auth families
// because users think in terms of "basic" or "bearer", not "http".
func canonicalHTTPScheme(scheme string) string {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "basic":
		return "basic"
	case "bearer":
		return "bearer"
	default:
		return "http"
	}
}

// isConnectedAuthSelector gates bucket user-connection lookups to flows the
// connect table can actually satisfy.
func isConnectedAuthSelector(selector string) bool {
	return selector == "oauth" || selector == "oidc"
}

// selectedAuthConfigsForExecution narrows auths for applyAuth's first-auth
// contract while keeping the selection policy outside the dispatcher.
func selectedAuthConfigsForExecution(auths models.AuthConfigs, credentials map[string]any) (models.AuthConfigs, string, error) {
	selector := requestedAuthType(credentials)
	if selector == "" {
		return auths, firstAuthType(auths), nil
	}
	for _, auth := range auths {
		if canonicalModelAuthType(auth) == selector {
			// applyAuth intentionally consumes auths[0], so selection happens here
			// where the full request context is still available.
			return models.AuthConfigs{auth}, selector, nil
		}
	}
	return nil, selector, fmt.Errorf("auth type %q is not configured for this service", selector)
}

// firstAuthType records the implicit default so telemetry can explain what was
// applied even when the caller omitted fused.authType.
func firstAuthType(auths models.AuthConfigs) string {
	if len(auths) == 0 {
		return ""
	}
	return canonicalModelAuthType(auths[0])
}

// connectedAuthNameForType bridges public auth-type selection to the private
// credential key where the connected access token must be injected.
func connectedAuthNameForType(auths fusedobject.AuthConfigs, selector string) string {
	if !isConnectedAuthSelector(selector) {
		return ""
	}
	for _, auth := range auths {
		if canonicalFusedAuthType(auth) == selector {
			return authCredentialName(auth)
		}
	}
	return ""
}

// requiredStaticSecretKeys derives the exact bucket secret names for this
// execution, preventing service-wide secret loads while preserving per-call
// auth selection.
func requiredStaticSecretKeys(auths fusedobject.AuthConfigs, credentials map[string]any) []string {
	auth, ok := selectedFusedAuthForExecution(auths, credentials)
	if !ok || selectedAuthIsConnected(auth, credentials) {
		return nil
	}
	return staticSecretKeysForAuth(auth)
}

// selectedFusedAuthForExecution mirrors dispatcher selection before mapping to
// models.AuthConfig, so secret reads and request auth stay in lockstep.
func selectedFusedAuthForExecution(auths fusedobject.AuthConfigs, credentials map[string]any) (fusedobject.AuthConfig, bool) {
	selector := requestedAuthType(credentials)
	if selector == "" {
		if len(auths) == 0 {
			return fusedobject.AuthConfig{}, false
		}
		return auths[0], true
	}
	for _, auth := range auths {
		if canonicalFusedAuthType(auth) == selector {
			return auth, true
		}
	}
	return fusedobject.AuthConfig{}, false
}

// selectedAuthIsConnected avoids fetching a static token for OAuth/OIDC calls
// that will resolve a bucket-owned user connection instead.
func selectedAuthIsConnected(auth fusedobject.AuthConfig, credentials map[string]any) bool {
	return connectedEndUserRef(credentials) != "" && isConnectedAuthSelector(canonicalFusedAuthType(auth))
}

// staticSecretKeysForAuth maps one selected auth family to the minimal set of
// encrypted bucket keys needed by applyAuth.
func staticSecretKeysForAuth(auth fusedobject.AuthConfig) []string {
	name := authCredentialName(auth)
	if name == "" {
		return nil
	}
	switch canonicalFusedAuthType(auth) {
	case "basic":
		return []string{name + "_username", name + "_password"}
	case "mtls":
		return []string{name + "_cert", name + "_key"}
	default:
		return []string{name}
	}
}
