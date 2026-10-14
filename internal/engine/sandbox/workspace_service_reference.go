package sandbox

import (
	"errors"
	"strings"
)

// registryWorkspaceServiceReference keeps Registry's provider namespace attached to every persisted slug.
func registryWorkspaceServiceReference(slug, canonicalRef string) (string, error) {
	parts := strings.Split(strings.TrimPrefix(canonicalRef, "@"), "/")
	// A missing or inconsistent namespace cannot fall back to a bare slug shared by unrelated accounts.
	if !strings.HasPrefix(canonicalRef, "@") || len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[1] != slug || strings.ContainsAny(canonicalRef, " \t\r\n") {
		return "", errors.New("Registry service canonical_ref is missing or invalid")
	}
	return canonicalRef, nil
}
