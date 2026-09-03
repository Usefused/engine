package sandbox

import (
	"errors"
	"strings"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

const credentialKeyFusedAuthType = "fused_auth_type"
const credentialKeyFusedAuthName = "fused_auth_name"

// requestedAuthType reads the SDK-facing selector from credentials so every
// execution path shares the same auth-type vocabulary.
func requestedAuthType(credentials map[string]any) string {
	return canonicalAuthSelector(credentialString(credentials, credentialKeyFusedAuthType))
}

func requestedAuthName(credentials map[string]any) string {
	return strings.TrimSpace(credentialString(credentials, credentialKeyFusedAuthName))
}

// canonicalAuthSelector accepts only the public selector vocabulary so imported
// OpenAPI spellings never leak into SDK method calls.
func canonicalAuthSelector(raw string) string {
	authType := strings.ToLower(strings.TrimSpace(raw))
	authType = strings.ReplaceAll(authType, "-", "_")
	switch authType {
	case "api_key", "oauth", "oidc", "basic", "bearer", "mtls", "oauth1", "digest":
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
	case "oauth1":
		return "oauth1"
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
	case "digest":
		return "digest"
	default:
		return "http"
	}
}

// isConnectedAuthSelector gates bucket user-connection lookups to flows the
// connect table can actually satisfy.
func isConnectedAuthSelector(selector string) bool {
	return selector == "oauth" || selector == "oidc"
}

// orderedStaticSecretAlternatives preserves OpenAPI OR ordering while keeping
// every AND-set together. The store can therefore choose one complete branch
// without decrypting values belonging to unused alternatives.
func orderedStaticSecretAlternatives(auths fusedobject.AuthConfigs, requirements authrouting.Requirements, credentials map[string]any) ([]store.SecretKeyAlternative, error) {
	if len(requirements) == 0 {
		return nil, errors.New("auth routing contract requires explicit security requirements")
	}
	definitions, err := fusedAuthDefinitions(auths)
	if err != nil {
		return nil, err
	}
	var alternatives []store.SecretKeyAlternative
	for _, requirement := range requirements {
		candidate, eligible, err := staticSecretAlternative(requirement, definitions, credentials)
		if err != nil {
			return nil, err
		}
		if eligible {
			alternatives = append(alternatives, candidate)
		}
	}
	return alternatives, nil
}

func fusedAuthDefinitions(auths fusedobject.AuthConfigs) (map[string]fusedobject.AuthConfig, error) {
	definitions := make(map[string]fusedobject.AuthConfig, len(auths))
	for _, auth := range auths {
		if auth.Name == "" || definitions[auth.Name].Name != "" {
			return nil, errors.New("auth routing contract has invalid auth definitions")
		}
		definitions[auth.Name] = auth
	}
	return definitions, nil
}

// staticSecretAlternative keeps each storage key bound to its exact reviewed
// scheme so the store can resolve a whole-auth reference without prefix guesses.
func staticSecretAlternative(alternative authrouting.Alternative, definitions map[string]fusedobject.AuthConfig, credentials map[string]any) (store.SecretKeyAlternative, bool, error) {
	// A selector mismatch makes this whole OpenAPI OR branch ineligible.
	if !alternativeMatchesSelectors(alternative, definitions, credentials) {
		return store.SecretKeyAlternative{}, false, nil
	}
	candidate := store.SecretKeyAlternative{}
	for _, requirement := range alternative.Schemes {
		auth, ok := definitions[requirement.Scheme]
		// Unknown scheme identities cannot be safely mapped to stored material.
		if !ok {
			return store.SecretKeyAlternative{}, false, errors.New("auth routing contract references an unknown scheme")
		}
		// Connected schemes resolve through user grants and contribute no static keys.
		if selectedAuthIsConnected(auth, credentials) {
			continue
		}
		required, optional, eligible := secretKeysNeeded(auth, credentials)
		// An invalid or incompatible static contract invalidates this AND-set.
		if !eligible {
			return store.SecretKeyAlternative{}, false, nil
		}
		candidate.Required = appendUnique(candidate.Required, required...)
		candidate.Optional = appendUnique(candidate.Optional, optional...)
		bindAlternativeAuthIdentity(&candidate, authCredentialName(auth), canonicalFusedAuthType(auth), required, optional)
	}
	return candidate, true, nil
}

// bindAlternativeAuthIdentity records exact key ownership and canonical family
// once for both required and optional material so runtime SQL cannot drift.
func bindAlternativeAuthIdentity(candidate *store.SecretKeyAlternative, authName, authType string, keySets ...[]string) {
	for _, keys := range keySets {
		for _, key := range keys {
			// Allocate both maps together because a partial identity must never authorize a reference.
			if candidate.AuthNames == nil {
				candidate.AuthNames = make(map[string]string)
				candidate.AuthTypes = make(map[string]string)
			}
			// Exact name and canonical type jointly authorize this destination key.
			candidate.AuthNames[key] = authName
			candidate.AuthTypes[key] = authType
		}
	}
}

func alternativeMatchesSelectors(alternative authrouting.Alternative, definitions map[string]fusedobject.AuthConfig, credentials map[string]any) bool {
	wantName := requestedAuthName(credentials)
	wantType := requestedAuthType(credentials)
	nameMatched := wantName == ""
	typeMatched := wantType == ""
	for _, requirement := range alternative.Schemes {
		auth, ok := definitions[requirement.Scheme]
		if !ok {
			continue
		}
		nameMatched = nameMatched || auth.Name == wantName
		typeMatched = typeMatched || canonicalFusedAuthType(auth) == wantType
	}
	return nameMatched && typeMatched
}

func secretKeysNeeded(auth fusedobject.AuthConfig, credentials map[string]any) ([]string, []string, bool) {
	name := authCredentialName(auth)
	if name == "" {
		return nil, nil, false
	}
	switch canonicalFusedAuthType(auth) {
	case "basic":
		return basicSecretKeysNeeded(auth, credentials)
	case "mtls":
		return missingNonemptyKeys(credentials, name+"_cert", name+"_key"), nil, true
	default:
		return missingNonemptyKeys(credentials, name), nil, true
	}
}

// basicSecretKeysNeeded applies the shared Basic default while preserving explicit optional and empty-password contracts.
func basicSecretKeysNeeded(auth fusedobject.AuthConfig, credentials map[string]any) ([]string, []string, bool) {
	name := authCredentialName(auth)
	required := missingNonemptyKeys(credentials, name+"_username")
	mode, valid := authrouting.EffectiveBasicPasswordMode(auth.BasicPasswordMode)
	// A malformed explicit mode cannot authorize secret selection.
	if !valid {
		return nil, nil, false
	}
	// Omitted mode resolves to required so credential lookup and dispatch share one invariant.
	switch mode {
	case authrouting.BasicPasswordRequired:
		return appendUnique(required, missingNonemptyKeys(credentials, name+"_password")...), nil, true
	case authrouting.BasicPasswordOptional:
		if _, supplied := credentials[name+"_password"]; supplied {
			return required, nil, true
		}
		return required, []string{name + "_password"}, true
	case authrouting.BasicPasswordEmpty:
		return required, nil, credentialString(credentials, name+"_password") == ""
	default:
		return nil, nil, false
	}
}

func missingNonemptyKeys(credentials map[string]any, keys ...string) []string {
	missing := make([]string, 0, len(keys))
	for _, key := range keys {
		if credentialString(credentials, key) == "" {
			missing = append(missing, key)
		}
	}
	return missing
}

func appendUnique(existing []string, values ...string) []string {
	for _, value := range values {
		found := false
		for _, current := range existing {
			found = found || current == value
		}
		if !found {
			existing = append(existing, value)
		}
	}
	return existing
}

// selectedAuthIsConnected keeps OAuth/OIDC application registration and user
// grants on the connection path even before a caller supplies user identity.
func selectedAuthIsConnected(auth fusedobject.AuthConfig, _ map[string]any) bool {
	// OAuth/OIDC bucket values are application client pairs, never provider access tokens for direct dispatch.
	return isConnectedAuthSelector(canonicalFusedAuthType(auth))
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
		if auth.BasicPasswordMode == authrouting.BasicPasswordEmpty {
			return []string{name + "_username"}
		}
		return []string{name + "_username", name + "_password"}
	case "mtls":
		return []string{name + "_cert", name + "_key"}
	case "oauth1":
		return []string{name + "_consumer_key", name + "_consumer_secret"}
	case "digest":
		return []string{name + "_username", name + "_password"}
	default:
		return []string{name}
	}
}
