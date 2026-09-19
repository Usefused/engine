package cmd

import "github.com/Usefused/engine/internal/engine/accesscontrol"

// appPermissionTestSnapshot preserves broad legacy fixtures by enumerating concrete grants explicitly.
// New boundary tests use the real constructor and request one type at a time.
func appPermissionTestSnapshot(revision int64, grants ...accesscontrol.Grant) (accesscontrol.AuthorizationSnapshot, error) {
	explicit := make([]accesscontrol.Grant, 0, len(grants))
	for _, grant := range grants {
		// Only trusted test shorthand expands; production rejects generic grants.
		if accesscontrol.IsAppAction(grant.Permission) {
			for _, permission := range accesscontrol.AppPermissions(grant.Permission) {
				explicit = append(explicit, accesscontrol.Grant{Permission: permission, Resource: grant.Resource})
			}
		} else {
			explicit = append(explicit, grant)
		}
	}
	return accesscontrol.NewAuthorizationSnapshot(revision, explicit...)
}
