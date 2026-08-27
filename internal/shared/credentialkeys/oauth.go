// Package credentialkeys owns deterministic workspace-secret names for
// credential families whose values must be stored and resolved atomically.
package credentialkeys

import "strings"

const (
	OAuthClientIDField     = "client_id"
	OAuthClientSecretField = "client_secret"
)

// OAuthApplication returns the fixed storage keys for one OAuth/OIDC app registration.
func OAuthApplication(authName string) (clientIDKey, clientSecretKey string, ok bool) {
	authName = strings.TrimSpace(authName)
	// A blank scheme name cannot form a stable bucket credential identity.
	if authName == "" {
		return "", "", false
	}
	return authName + "_client_id", authName + "_client_secret", true
}

// OAuthApplicationField maps one semantic client field to its deterministic storage key.
func OAuthApplicationField(authName, field string) (string, bool) {
	clientIDKey, clientSecretKey, ok := OAuthApplication(authName)
	// Invalid family identities are rejected before a field-specific decision.
	if !ok {
		return "", false
	}
	switch strings.TrimSpace(field) {
	case OAuthClientIDField:
		return clientIDKey, true
	case OAuthClientSecretField:
		return clientSecretKey, true
	default:
		// Unknown fields must not create ad-hoc secret keys outside the family contract.
		return "", false
	}
}
